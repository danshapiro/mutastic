// Package light implements the NEEWER PL81 PRO USB-serial protocol and the
// daemon-side manager for the light. Protocol reference:
// docs/pl81-pro-serial-protocol.md (frame layout, checksum, temp encoding,
// and the live-probe transcript that proved the byte sequences below).
package light

import "math"

// TagCCT is the only functional command tag on the PL81 Pro: power +
// brightness + color temperature in one 3-byte payload.
const TagCCT = 0x02

// Kelvin range of the panel; inputs outside it are clamped.
const (
	MinKelvin = 2900
	MaxKelvin = 7000
)

const (
	header      = 0x3A // USB-serial transport prefix (BLE/WiFi differ - do not port)
	maxTempByte = 0x12 // firmware clamps here: 19 steps, 0x00..0x12
)

// Frame builds a complete PL81 serial frame:
// 0x3A | tag | payload_len | payload... | cs_hi | cs_lo
// where the checksum is the 16-bit big-endian sum of all preceding bytes.
func Frame(tag byte, payload ...byte) []byte {
	f := make([]byte, 0, 5+len(payload))
	f = append(f, header, tag, byte(len(payload)))
	f = append(f, payload...)
	var sum uint16
	for _, b := range f {
		sum += uint16(b)
	}
	return append(f, byte(sum>>8), byte(sum&0xFF))
}

// CCT builds the tag-0x02 command that sets everything the PL81 Pro can do.
// pwr is always 0x01: OFF is expressed as brightness 0, because the tag-0x06
// power command is a no-op on this model (verified upstream, m-rk).
func CCT(brightness, temp byte) []byte {
	return Frame(TagCCT, 0x01, brightness, temp)
}

// KelvinToByte quantizes a Kelvin value to the panel's 19 hardware steps
// (~228 K each), clamping to [MinKelvin, MaxKelvin]. m-rk calibration:
// byte = round((K - 2900) * 18 / 4100).
func KelvinToByte(k int) byte {
	if k < MinKelvin {
		k = MinKelvin
	}
	if k > MaxKelvin {
		k = MaxKelvin
	}
	return byte(math.Round(float64(k-MinKelvin) * 18.0 / 4100.0))
}

// ByteToKelvin is the inverse mapping, used to render status replies from
// echoed frames. Bytes above the firmware clamp render identically to 0x12.
func ByteToKelvin(b byte) int {
	if b > maxTempByte {
		b = maxTempByte
	}
	return int(math.Round(2900.0 + float64(b)*4100.0/18.0))
}

// Preset is a host-side (brightness%, Kelvin) pair. The device has no scene
// command; presets are sent as ordinary CCT frames.
type Preset struct {
	Brightness int
	Kelvin     int
}

// Presets mirrors the NEEWER app's built-in looks (protocol doc, section
// "HSI/RGB and scenes").
var Presets = map[string]Preset{
	"cold":      {Brightness: 100, Kelvin: 7000},
	"sunlight":  {Brightness: 28, Kelvin: 5600},
	"afternoon": {Brightness: 16, Kelvin: 5000},
	"sunset":    {Brightness: 16, Kelvin: 4500},
	"candle":    {Brightness: 28, Kelvin: 3400},
}
