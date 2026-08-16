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
// atomic conditional mic verbs "mute-if <expected>" /
// "unmute-if <expected>" with expected ∈ {muted, unmuted}. Each asks the
// daemon to perform its absolute verb (plus the F24 meeting-app sweep)
// ONLY when its tracked state still equals <expected> - premise check
// and action in ONE serveUDP step (R6-F2). Replies: "ok" (premise
// matched: verb written + sweep injected), "flipped muted|unmuted" or
// "flipped unknown" (premise failed: NO verb, NO inject), or
// "error: <reason>" (the absolute verb's write or the injection failed).
const (
	ConditionalMuteVerbPrefixMute   = "mute-if"
	ConditionalMuteVerbPrefixUnmute = "unmute-if"
)

// ParseConditionalMute decodes one atomic conditional mic verb command.
// ok=true only for the exact grammar "<verb> <expected>" with verb ∈
// {mute-if, unmute-if} and expected ∈ {muted, unmuted} - everything else
// (a valid prefix with an invalid expectation, a missing argument, extra
// tokens, case drift) yields ok=false so the daemon replies its generic
// "error: unknown command" and buggy clients fail loudly.
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
	switch arg {
	case "muted":
		return target, true, true
	case "unmuted":
		return target, false, true
	}
	return false, false, false
}
