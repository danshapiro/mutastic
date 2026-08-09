package light

import (
	"bytes"
	"testing"
)

// Expected bytes come from docs/pl81-pro-serial-protocol.md. The first two
// CCT frames were echoed byte-for-byte by this machine's unit on 2026-08-08
// (live probe transcript), so they are hardware-proven ground truth.
func TestFrameChecksumMatchesProvenFrames(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"on 100% temp 0x09", CCT(0x64, 0x09), []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD}},
		{"off is brightness 0", CCT(0x00, 0x09), []byte{0x3A, 0x02, 0x03, 0x01, 0x00, 0x09, 0x00, 0x49}},
		{"generic frame tag 0x06 on", Frame(0x06, 0x01), []byte{0x3A, 0x06, 0x01, 0x01, 0x00, 0x42}},
		{"generic frame tag 0x06 off", Frame(0x06, 0x02), []byte{0x3A, 0x06, 0x01, 0x02, 0x00, 0x43}},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: got % x, want % x", c.name, c.got, c.want)
		}
	}
}

func TestKelvinToByteAnchorsAndClamping(t *testing.T) {
	cases := []struct {
		kelvin int
		want   byte
	}{
		{2900, 0x00}, // low anchor
		{4950, 0x09}, // mid anchor: the live probe's temp byte
		{7000, 0x12}, // high anchor: exactly 18 steps
		{5600, 0x0C}, // sunlight preset
		{5000, 0x09}, // afternoon preset / default temp
		{4500, 0x07}, // sunset preset
		{3400, 0x02}, // candle preset
		{2000, 0x00}, // below range: clamps to floor
		{9000, 0x12}, // above range: clamps to ceiling
	}
	for _, c := range cases {
		if got := KelvinToByte(c.kelvin); got != c.want {
			t.Errorf("KelvinToByte(%d) = 0x%02x, want 0x%02x", c.kelvin, got, c.want)
		}
	}
}

func TestByteToKelvinInverseAndClamp(t *testing.T) {
	cases := []struct {
		b    byte
		want int
	}{
		{0x00, 2900},
		{0x09, 4950},
		{0x0C, 5633},
		{0x12, 7000},
		{0x20, 7000}, // firmware clamps bytes above 0x12; mirror that host-side
	}
	for _, c := range cases {
		if got := ByteToKelvin(c.b); got != c.want {
			t.Errorf("ByteToKelvin(0x%02x) = %d, want %d", c.b, got, c.want)
		}
	}
}

func TestPresetFramesMatchProtocolDoc(t *testing.T) {
	// Full frames derived in the protocol doc (brightness hex + temp byte +
	// 16-bit BE sum checksum).
	cases := []struct {
		name string
		want []byte
	}{
		{"cold", []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x12, 0x00, 0xB6}},
		{"sunlight", []byte{0x3A, 0x02, 0x03, 0x01, 0x1C, 0x0C, 0x00, 0x68}},
		{"afternoon", []byte{0x3A, 0x02, 0x03, 0x01, 0x10, 0x09, 0x00, 0x59}},
		{"sunset", []byte{0x3A, 0x02, 0x03, 0x01, 0x10, 0x07, 0x00, 0x57}},
		{"candle", []byte{0x3A, 0x02, 0x03, 0x01, 0x1C, 0x02, 0x00, 0x5E}},
	}
	for _, c := range cases {
		p, ok := Presets[c.name]
		if !ok {
			t.Fatalf("preset %q missing", c.name)
		}
		got := CCT(byte(p.Brightness), KelvinToByte(p.Kelvin))
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s: got % x, want % x", c.name, got, c.want)
		}
	}
}
