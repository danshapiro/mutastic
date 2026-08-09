package light

// Decoded is one checksum-valid inbound frame (an echo/ACK of a command we
// wrote, or an unprompted knob broadcast - same wire format either way).
type Decoded struct {
	Tag     byte
	Payload []byte
}

// Parser incrementally extracts valid 0x3A frames from a raw serial byte
// stream. The device gives no alignment guarantees, so the parser
// resynchronizes on the header byte and validates the 16-bit big-endian
// checksum before emitting a frame; anything else is dropped byte-by-byte.
type Parser struct {
	buf []byte
}

// Feed appends data to the internal buffer and returns every complete valid
// frame now available. Partial frames stay buffered for the next Feed.
func (p *Parser) Feed(data []byte) []Decoded {
	p.buf = append(p.buf, data...)
	var out []Decoded
	for {
		// Drop noise before the next possible header.
		for len(p.buf) > 0 && p.buf[0] != header {
			p.buf = p.buf[1:]
		}
		if len(p.buf) < 3 {
			return out
		}
		n := int(p.buf[2]) // payload_len
		total := 3 + n + 2 // header+tag+len + payload + checksum
		if len(p.buf) < total {
			return out
		}
		frame := p.buf[:total]
		var sum uint16
		for _, b := range frame[:3+n] {
			sum += uint16(b)
		}
		if frame[3+n] != byte(sum>>8) || frame[4+n] != byte(sum&0xFF) {
			// Not a real frame boundary: skip this header byte and resync.
			p.buf = p.buf[1:]
			continue
		}
		payload := make([]byte, n)
		copy(payload, frame[3:3+n])
		out = append(out, Decoded{Tag: frame[1], Payload: payload})
		p.buf = p.buf[total:]
	}
}
