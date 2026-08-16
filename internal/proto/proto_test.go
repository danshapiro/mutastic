package proto

import (
	"bytes"
	"testing"
)

func TestEncodeCommandInitHandshake(t *testing.T) {
	// Doc example: 01 00 00 00 05 00 00 00 08 00 ... 00 (65 bytes)
	got := EncodeCommand(OpInit, nil)
	want := make([]byte, 65)
	want[0] = 0x01
	want[4] = 0x05
	want[8] = 0x08
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeCommand(OpInit, nil) = % x, want % x", got, want)
	}
}

func TestEncodeCommandGetVolume(t *testing.T) {
	got := EncodeCommand(OpGetVolume, nil)
	if len(got) != 65 || got[0] != 0x01 || got[4] != 0x01 || got[8] != 0x08 {
		t.Fatalf("EncodeCommand(OpGetVolume, nil) = % x", got)
	}
}

func TestEncodeCommandMuteOn(t *testing.T) {
	// Doc example: 01 00 00 00 20 00 00 00 09 31 00 ... 00
	got := EncodeCommand(OpMute, []byte("1"))
	want := make([]byte, 65)
	want[0] = 0x01
	want[4] = 0x20
	want[8] = 0x09 // 8 + len("1")
	want[9] = 0x31 // ASCII '1'
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeCommand(OpMute, \"1\") = % x, want % x", got, want)
	}
}

func TestEncodeCommandMuteOff(t *testing.T) {
	got := EncodeCommand(OpMute, []byte("0"))
	if got[4] != 0x20 || got[8] != 0x09 || got[9] != 0x30 {
		t.Fatalf("EncodeCommand(OpMute, \"0\") = % x", got)
	}
}

func inputReport(op, value byte) []byte {
	b := make([]byte, 64)
	b[0] = 0x01
	b[1] = 0x80
	b[4] = op
	b[9] = value
	return b
}

func TestDecodeEventDeviceMute(t *testing.T) {
	ev, ok := DecodeEvent(inputReport(0x21, 0x01))
	if !ok || ev.Op != EvtDeviceMute || ev.Value != 0x01 {
		t.Fatalf("DecodeEvent = %+v, %v; want {Op:0x21 Value:0x01}, true", ev, ok)
	}
}

func TestDecodeEventRejectsWrongPrefix(t *testing.T) {
	b := inputReport(0x20, 0x01)
	b[1] = 0x00 // break the 01 80 00 00 prefix
	if _, ok := DecodeEvent(b); ok {
		t.Fatal("DecodeEvent accepted a report without the 01 80 00 00 prefix")
	}
}

func TestDecodeEventRejectsShortBuffer(t *testing.T) {
	if _, ok := DecodeEvent([]byte{0x01, 0x80, 0x00, 0x00, 0x20}); ok {
		t.Fatal("DecodeEvent accepted a buffer shorter than 10 bytes")
	}
}

func TestMutedFromValue(t *testing.T) {
	cases := []struct {
		v         byte
		muted, ok bool
	}{
		{0x00, false, true}, // binary unmuted
		{0x01, true, true},  // binary muted
		{0x30, false, true}, // ASCII '0'
		{0x31, true, true},  // ASCII '1'
		{0x42, false, false},
	}
	for _, c := range cases {
		muted, ok := MutedFromValue(c.v)
		if muted != c.muted || ok != c.ok {
			t.Errorf("MutedFromValue(0x%02x) = %v, %v; want %v, %v", c.v, muted, ok, c.muted, c.ok)
		}
	}
}

// TestParseConditionalMute pins the wire grammar of the atomic
// conditional mic verbs (R6-F2, restricted R11-F4): exactly the TWO SAFE
// opposite-state forms, "<verb> <premise it acts FROM>" - "mute-if
// unmuted" and "unmute-if muted" - so a premising client (the tray) and
// the daemon read the same forms. The two same-state combinations
// ("mute-if muted", "unmute-if unmuted") are NOT grammar: premised on
// the state the verb would only re-assert, they would still inject the
// blind F24 sweep and desynchronize the meeting apps from an UNCHANGED
// mic - they fall to the daemon's generic "error: unknown command",
// exactly like every other malformed shape, so a buggy client fails
// loudly instead of silently matching a looser grammar.
func TestParseConditionalMute(t *testing.T) {
	cases := []struct {
		cmd                          string
		targetMuted, expectMuted, ok bool
	}{
		{"mute-if unmuted", true, false, true}, // premise the Mute verb acts FROM
		{"unmute-if muted", false, true, true}, // premise the Unmute verb acts FROM
		// Everything below must NOT parse.
		{"mute-if muted", false, false, false},         // unsafe same-state form (R11-F4): premise already the target - would sweep an unchanged mic
		{"unmute-if unmuted", false, false, false},     // unsafe same-state form (R11-F4): ditto
		{"mute", false, false, false},                  // the plain absolute verb is not conditional
		{"unmute", false, false, false},                // ditto
		{"toggle", false, false, false},                // ditto
		{"mute-if", false, false, false},               // missing argument
		{"mute-if ", false, false, false},              // trailing space, empty expectation
		{"mute-if sideways", false, false, false},      // expectation outside {muted, unmuted}
		{"mute-if  unmuted", false, false, false},      // double space: the arg would carry a leading space
		{"mute-if unmuted extra", false, false, false}, // extra token
		{"mute-if UNMUTED", false, false, false},       // case-sensitive grammar
		{"Mute-if unmuted", false, false, false},
		{"mute-If unmuted", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		target, expect, ok := ParseConditionalMute(c.cmd)
		if target != c.targetMuted || expect != c.expectMuted || ok != c.ok {
			t.Errorf("ParseConditionalMute(%q) = %v, %v, %v; want %v, %v, %v",
				c.cmd, target, expect, ok, c.targetMuted, c.expectMuted, c.ok)
		}
	}
}
