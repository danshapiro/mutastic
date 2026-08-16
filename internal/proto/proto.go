// Package proto encodes and decodes Blue Yeti X vendor HID reports.
// Byte layouts follow docs/yeti-x-hid-protocol.md.
package proto

import "strings"

// Command opcodes (outbound, at offset 4).
const (
	OpGetVolume byte = 0x01
	OpInit      byte = 0x05
	OpMute      byte = 0x20
)

// Event codes (inbound, at offset 4).
const (
	EvtSoftwareMute byte = 0x20 // host-initiated mute echo
	EvtDeviceMute   byte = 0x21 // physical button on the mic
)

const outputReportLen = 65 // report ID + 64 data bytes

// EncodeCommand builds a complete 65-byte output report:
// [0]=0x01 report ID, [4]=op, [8]=8+len(payload), [9:]=ASCII payload, zero-padded.
func EncodeCommand(op byte, payload []byte) []byte {
	buf := make([]byte, outputReportLen)
	buf[0] = 0x01
	buf[4] = op
	buf[8] = byte(8 + len(payload))
	copy(buf[9:], payload)
	return buf
}

// Event is a decoded inbound vendor event.
type Event struct {
	Op    byte // event code from offset 4
	Value byte // raw value byte from offset 9
}

// DecodeEvent parses an inbound report (report ID included at [0]).
// It returns ok=false unless the report carries the 01 80 00 00 vendor
// event prefix and is long enough to contain the value byte.
func DecodeEvent(buf []byte) (Event, bool) {
	if len(buf) < 10 {
		return Event{}, false
	}
	if buf[0] != 0x01 || buf[1] != 0x80 || buf[2] != 0x00 || buf[3] != 0x00 {
		return Event{}, false
	}
	return Event{Op: buf[4], Value: buf[9]}, true
}

// MutedFromValue interprets a mute event's value byte. The protocol doc
// leaves open whether it is binary (0x00/0x01) or ASCII ('0'/'1'), so both
// are accepted; anything else returns ok=false.
func MutedFromValue(v byte) (muted bool, ok bool) {
	switch v {
	case 0x00, '0':
		return false, true
	case 0x01, '1':
		return true, true
	}
	return false, false
}

// --- daemon UDP text wire: atomic conditional mic verbs (R6-F2) ---
//
// The daemon's plain-text command surface lives in internal/daemon; the
// grammar of the CONDITIONAL mic verbs is pinned here beside the rest of
// the wire byte/word contracts so the server (daemon) and any client
// validation share one definition instead of drifting string literals
// per call site.

// ConditionalMuteVerbPrefixMute / -Unmute are the command heads of the
// atomic conditional mic verbs. Only the TWO SAFE opposite-state forms
// exist (R11-F4): "mute-if unmuted" and "unmute-if muted". Each asks the
// daemon to perform its absolute verb (plus the F24 meeting-app sweep)
// ONLY when its tracked state still equals the OPPOSITE premise -
// premise check and action in ONE serveUDP step (R6-F2). The two
// same-state combinations ("mute-if muted", "unmute-if unmuted") are NOT
// grammar: they pass their premise whenever it holds, write an
// already-satisfied absolute state, and would still inject the blind F24
// sweep - desynchronizing the meeting apps from an UNCHANGED mic. They
// are rejected at parse (ok=false) and fall to the generic
// "error: unknown command". Replies: "ok" (premise matched: verb written
// + sweep injected), "flipped muted|unmuted" or "flipped unknown"
// (premise failed: NO verb, NO inject), or "error: <reason>" (the
// absolute verb's write or the injection failed).
const (
	ConditionalMuteVerbPrefixMute   = "mute-if"
	ConditionalMuteVerbPrefixUnmute = "unmute-if"
)

// ParseConditionalMute decodes one atomic conditional mic verb command.
// ok=true only for the exact grammar "<verb> <opposite-premise>" -
// "mute-if unmuted" or "unmute-if muted" (R11-F4: the premise must be
// the state the verb acts FROM; the same-state combinations "mute-if
// muted" / "unmute-if unmuted" never act meaningfully and are rejected
// wholesale). Everything else (a valid prefix with an invalid or
// same-state expectation, a missing argument, extra tokens, case drift)
// yields ok=false so the daemon replies its generic "error: unknown
// command" and buggy clients fail loudly instead of injecting a sweep
// against an unchanged mic.
func ParseConditionalMute(cmd string) (targetMuted, expectMuted, ok bool) {
	verb, arg, found := strings.Cut(cmd, " ")
	if !found {
		return false, false, false
	}
	var target bool
	switch verb {
	case ConditionalMuteVerbPrefixMute:
		target = true
	case ConditionalMuteVerbPrefixUnmute:
		target = false
	default:
		return false, false, false
	}
	var expect bool
	switch arg {
	case "muted":
		expect = true
	case "unmuted":
		expect = false
	default:
		return false, false, false
	}
	if target == expect {
		// The two unsafe same-state forms: "mute-if muted" /
		// "unmute-if unmuted" - rejected at parse so they can never
		// reach the sweep (R11-F4).
		return false, false, false
	}
	return target, expect, true
}
