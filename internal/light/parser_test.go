package light

import (
	"bytes"
	"testing"
)

func TestParserExtractsSingleFrame(t *testing.T) {
	var p Parser
	frames := p.Feed([]byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD})
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Tag != TagCCT || !bytes.Equal(frames[0].Payload, []byte{0x01, 0x64, 0x09}) {
		t.Fatalf("got tag=0x%02x payload=% x, want tag=0x02 payload=01 64 09",
			frames[0].Tag, frames[0].Payload)
	}
}

func TestParserReassemblesSplitFrame(t *testing.T) {
	var p Parser
	if got := p.Feed([]byte{0x3A, 0x02, 0x03, 0x01}); len(got) != 0 {
		t.Fatalf("premature frames from first half: %d", len(got))
	}
	frames := p.Feed([]byte{0x64, 0x09, 0x00, 0xAD})
	if len(frames) != 1 || frames[0].Tag != TagCCT {
		t.Fatalf("got %d frames after second half, want 1 CCT frame", len(frames))
	}
}

func TestParserResyncsPastGarbage(t *testing.T) {
	var p Parser
	stream := append([]byte{0xFF, 0x00, 0x13},
		0x3A, 0x02, 0x03, 0x01, 0x00, 0x09, 0x00, 0x49)
	frames := p.Feed(stream)
	if len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0x01, 0x00, 0x09}) {
		t.Fatalf("got %v, want the off frame after leading garbage", frames)
	}
}

func TestParserDiscardsBadChecksumAndRecovers(t *testing.T) {
	var p Parser
	bad := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0xFF, 0xFF} // corrupt checksum
	good := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD}
	frames := p.Feed(append(bad, good...))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want exactly the 1 valid frame", len(frames))
	}
	if !bytes.Equal(frames[0].Payload, []byte{0x01, 0x64, 0x09}) {
		t.Fatalf("recovered frame payload = % x", frames[0].Payload)
	}
}

func TestParserHandlesBackToBackFrames(t *testing.T) {
	var p Parser
	stream := append(
		[]byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD},
		0x3A, 0x02, 0x03, 0x01, 0x00, 0x09, 0x00, 0x49)
	frames := p.Feed(stream)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Payload[1] != 0x64 || frames[1].Payload[1] != 0x00 {
		t.Fatalf("frames out of order: % x then % x", frames[0].Payload, frames[1].Payload)
	}
}
