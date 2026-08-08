// Package proto encodes and decodes Blue Yeti X vendor HID reports.
// Byte layouts follow docs/yeti-x-hid-protocol.md.
package proto

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
