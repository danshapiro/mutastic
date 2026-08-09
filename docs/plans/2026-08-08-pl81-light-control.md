# PL81 Light Control Implementation Plan

> **For agentic workers:** This plan is executed task-by-task by the
> workflow's execute stage: a fresh implementer per task, with a spec +
> quality review after each task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** Add control of a NEEWER PL81 PRO LED streaming light (USB serial) to the existing mutastic daemon, exposed via `mutastic light ...` CLI/UDP commands and toggled by the LEFT foot pedal (F13).

**Architecture:** A new self-contained `internal/light` package mirrors the mic's design: a manager with its own goroutine reconnect loop opens the CH340 serial port by VID/PID, sends the wake sequence, continuously parses echoed/broadcast 0x3A frames to track state, and persists the "last look" to `%LOCALAPPDATA%\mutastic\light-state.json`. The existing UDP server (`internal/daemon`) routes any command starting with `light ` to the manager through a small `CommandHandler` interface. The CLI gains a `light` subcommand; AHK gains an F13 hotkey.

**Tech Stack:** Go 1.26.3 (module `mutastic`), `go.bug.st/serial` **v1.8.0, pinned** (+ its `enumerator` subpackage; validated 2026-08-08: pure Go on Windows, cross-compiles under this repo's exact cgo/mingw build), existing `github.com/sstallion/go-hid` (untouched), AutoHotkey v1.1, cmd batch deploy script.

## Global Constraints

- Module `mutastic`, `go 1.26.3`. Dev in WSL2; build target windows/amd64 via `./build.sh` (cgo, `CC=x86_64-w64-mingw32-gcc`) → `bin/mutastic.exe` (never committed).
- Quality gate after EVERY task: `go test -race ./... && go vet ./...` — both clean. Existing mic tests must keep passing.
- UDP command surface: `127.0.0.1:42814` (`const udpAddr`, `main.go:21`). One datagram = one command; server read buffer is 64 bytes — all light commands are well under that. Error replies use the exact prefix `error: ` (drives CLI exit code 1).
- Serial device: CH340 bridge, **VID `0x1A86` PID `0x7523`** — ALWAYS enumerate by VID/PID, NEVER hardcode COM4. 115200 8N1. Open sequence: flush both buffers, write wake bytes `00 00 00 00`, sleep ~100 ms. Keep ≥60 ms spacing before every frame write. Open with **DTR/RTS deasserted** (`InitialStatusBits: &serial.ModemOutputBits{RTS: false, DTR: false}`): the proven 2026-08-08 probe ran on .NET SerialPort defaults (both deasserted) while go.bug.st v1.8.0's default asserts both (verified in library source) — replicate the proven line state, trust neither default. Set the **1 s read timeout once, at open** (v1.8.0 opens in NoTimeout mode where `Read` blocks forever, and re-issuing `SetCommTimeouts` per read can race an in-flight `Write`). Never `Close` a port concurrently with `Read` (go.bug.st issue #219): close only after the session loop has returned.
- Frame: `[0x3A] [tag] [payload_len] [payload...] [cs_hi] [cs_lo]`; checksum = 16-bit **big-endian** sum of all preceding bytes.
- Only tag `0x02` (CCT) is functional: `3A 02 03 <pwr=0x01> <brightness 0-100> <tempByte> <cs_hi> <cs_lo>`. OFF = brightness 0 with `pwr=0x01`. Tag `0x06` power frames are a no-op on this model — never use them.
- Temp byte: `byte = round((K - 2900) * 18 / 4100)`, clamped to `0x00`–`0x12`; Kelvin clamped to 2900–7000. Inverse: `K = round(2900 + byte*4100/18)`.
- Presets (host-side pairs): cold 100%/7000K · sunlight 28%/5600K · afternoon 16%/5000K · sunset 16%/4500K · candle 28%/3400K.
- No query command exists: state is learned ONLY from byte-for-byte echoes (ACKs) and unprompted knob broadcasts. State is `unknown` until the first inbound frame. Inbound CCT frames with `pwr != 0x01` are treated as OFF (brightness 0): upstream captures show panels can signal off via the pwr byte (`0x00`/`0x02`) while the brightness field stays non-zero.
- Persisted state file: `%LOCALAPPDATA%\mutastic\light-state.json` (same dir as `mutastic.log`, via `os.UserCacheDir()`).
- `ahk/MuteAllMeetings.ahk` is UTF-8 **with BOM** + **CRLF**, AHK **v1.1** syntax. Preserve both exactly; do NOT change F14 behavior; F15 belongs to Winpepper — never touch it.
- `README.md` is the only end-user markdown doc; `docs/` files are working/protocol docs.
- Deployed system: `C:\Users\dan\code\mutastic-deploy\mutastic.exe` + `MuteAllMeetings.ahk`, Startup shortcuts `Mutastic Daemon.lnk` and `MuteAllMeetings.lnk`. `deploy/deploy.cmd` needs no structural change.
- Live-test end state (non-negotiable): the LIGHT is OFF and the MIC is UNMUTED — both are the user's live devices.
- WSL→Windows interop (`cmd.exe`, `powershell.exe`, `/mnt/c`) is required for live steps and has been flaky today: if it breaks mid-run, HALT that task and surface it as a blocker — do not guess or fake results.
- Commits are focused and atomic, one per task.

## Scope Check

This is one subsystem (light control) plus its integration points (UDP dispatch, CLI, AHK, deploy, docs). It builds on the existing, already-tested mic daemon. A single plan is appropriate; whole-system coverage comes from Task 9's live E2E (light AND mic regression).

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/light/frame.go` | Create | Pure protocol encode: frame builder + checksum, CCT command, Kelvin↔byte mapping, presets |
| `internal/light/frame_test.go` | Create | Encode/mapping tests against the doc's proven byte sequences |
| `internal/light/parser.go` | Create | Incremental inbound frame parser: resync on 0x3A, checksum validation |
| `internal/light/parser_test.go` | Create | Parser tests: split frames, garbage, bad checksum, back-to-back |
| `internal/light/state.go` | Create | Tri-state light state tracker + JSON persistence of the "last look" |
| `internal/light/state_test.go` | Create | State transition + persistence round-trip tests |
| `internal/light/manager.go` | Create | Manager: port handle + rate-limited writes + command handling (Task 4); session/reconnect loop (Task 5) |
| `internal/light/manager_test.go` | Create | fakePort, command semantics, session/reconnect tests |
| `internal/daemon/daemon.go` | Modify | `CommandHandler` interface, `Daemon.Light` field, `light ` prefix dispatch, `Run` signature |
| `internal/daemon/daemon_test.go` | Modify | Update `Run` call sites (nil light), add routing tests |
| `main.go` | Modify | `light` CLI subcommand (arg joining, 2 s timeout), usage, daemon wiring, `lightStatePath()` |
| `main_test.go` | Modify | Multi-word command passthrough test |
| `light_windows.go` | Create | `openPL81`: VID/PID enumeration + serial open (go.bug.st/serial), `light.Port` adapter |
| `light_other.go` | Create | Non-Windows stub for `openPL81` |
| `go.mod` / `go.sum` | Modify | Add `go.bug.st/serial` |
| `ahk/MuteAllMeetings.ahk` | Modify | F13 hotkey (light toggle), tray tip, header comment (BOM+CRLF preserved) |
| `docs/pedal-and-mute.md` | Modify | Left-pedal row: F13 now assigned |
| `README.md` | Modify | Light commands, F13 binding, troubleshooting |
| `docs/pl81-pro-serial-protocol.md` | Modify | Newly confirmed facts + recorded human questions |
| `deploy/deploy.cmd` | None | Already copies the single exe + single ahk — no change |

Design note (locked in): the light manager is a **sibling** of the mic logic, not a refactor of it. `internal/daemon` keeps owning the mic session and the UDP server; it delegates `light ...` strings through the one-method `CommandHandler` interface so the daemon package never imports `internal/light`. `main.go` wires both. This is the smallest change that leaves the proven mic path untouched.

Divergence from the mic session, on purpose: the mic session has a handshake-liveness gate because `GetVolume` provokes a reply. The PL81 has **no query command**, so a silent-but-healthy idle light is indistinguishable from a dead one — the light session therefore has NO query-based liveness gate; a session ends on a read/write error, or when the optional device-presence check (USB re-enumeration consulted after ~10 s of read silence) reports the device gone — the CH340 driver's surprise-removal error behavior is unverified, so presence is checked directly rather than assumed.

---

### Task 1: Frame encoding, temperature mapping, presets

**Files:**
- Create: `internal/light/frame.go`
- Test: `internal/light/frame_test.go`

**Interfaces:**
- Consumes: nothing (pure functions, stdlib `math` only).
- Produces (later tasks rely on these exact names):
  - `func Frame(tag byte, payload ...byte) []byte`
  - `func CCT(brightness, temp byte) []byte`
  - `func KelvinToByte(k int) byte` / `func ByteToKelvin(b byte) int`
  - `const TagCCT = 0x02`, `const MinKelvin = 2900`, `const MaxKelvin = 7000`
  - `type Preset struct { Brightness int; Kelvin int }`, `var Presets map[string]Preset`

- [ ] **Step 1: Write the failing test**

Create `internal/light/frame_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/dan/code/mutastic/.worktrees/pl81-light-control && go test ./internal/light/ -v`
Expected: FAIL to build — `undefined: CCT` (and friends); the package has no non-test source yet.

- [ ] **Step 3: Write the implementation**

Create `internal/light/frame.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/light/ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: all packages pass, vet silent.

- [ ] **Step 6: Commit**

```bash
git add internal/light/frame.go internal/light/frame_test.go
git commit -m "feat: PL81 frame encoding, temp mapping, presets"
```

---

### Task 2: Inbound frame parser

**Files:**
- Create: `internal/light/parser.go`
- Test: `internal/light/parser_test.go`

**Interfaces:**
- Consumes: `TagCCT`, `header` (Task 1).
- Produces:
  - `type Decoded struct { Tag byte; Payload []byte }`
  - `type Parser struct { ... }` with `func (p *Parser) Feed(data []byte) []Decoded`

- [ ] **Step 1: Write the failing test**

Create `internal/light/parser_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/light/ -v -run TestParser`
Expected: FAIL to build — `undefined: Parser`.

- [ ] **Step 3: Write the implementation**

Create `internal/light/parser.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/light/ -v -run TestParser`
Expected: PASS (5 tests).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/light/parser.go internal/light/parser_test.go
git commit -m "feat: PL81 inbound frame parser with resync and checksum validation"
```

---

### Task 3: Light state tracker with persistence

**Files:**
- Create: `internal/light/state.go`
- Test: `internal/light/state_test.go`

**Interfaces:**
- Consumes: `KelvinToByte`, `ByteToKelvin`, `maxTempByte` (Task 1).
- Produces:
  - `func NewState(path string) *State` (path `""` disables persistence)
  - `func (s *State) Set(brightness int, temp byte) error` (error = persist failure only)
  - `func (s *State) Status() (on bool, brightness int, temp byte, known bool)`
  - `func (s *State) TargetOn() (brightness int, temp byte)`
  - `func (s *State) StatusString() string` → `"unknown"` / `"off"` / `"on <b>% <K>K"`

Semantics locked in here (later tasks and README depend on them):
- Tri-state like the mic's `Tracker`: `known=false` until the first `Set` (echo, broadcast, or optimistic post-write).
- "On" is derived: `brightness > 0` (pwr byte is always 0x01 on the wire).
- The restore target ("previous look") is the last **non-zero** brightness plus the current temp byte; defaults 100% and 5000 K (byte 0x09) when nothing is persisted.
- Persisted JSON: `{"on":bool,"brightness":lastNonZero,"temp_byte":int}`; loaded values seed ONLY the restore targets — status stays `unknown` after a restart until a real frame arrives (there is nothing to query).
- Kelvin in `StatusString` is the quantized hardware step (e.g. requesting 5000 K shows `4950K`) — the display is derived from the byte, which is the only ground truth the device reports.

- [ ] **Step 1: Write the failing test**

Create `internal/light/state_test.go`:

```go
package light

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateUnknownUntilFirstSet(t *testing.T) {
	s := NewState("")
	if got := s.StatusString(); got != "unknown" {
		t.Fatalf("fresh state = %q, want unknown", got)
	}
	if err := s.Set(64, 0x09); err != nil {
		t.Fatal(err)
	}
	if got := s.StatusString(); got != "on 64% 4950K" {
		t.Fatalf("after Set(64, 0x09) = %q, want %q", got, "on 64% 4950K")
	}
	if err := s.Set(0, 0x09); err != nil {
		t.Fatal(err)
	}
	if got := s.StatusString(); got != "off" {
		t.Fatalf("after Set(0, 0x09) = %q, want off", got)
	}
}

func TestStateTargetOnDefaults(t *testing.T) {
	s := NewState("")
	b, temp := s.TargetOn()
	if b != 100 || temp != 0x09 { // 100%, 5000K quantized to byte 0x09
		t.Fatalf("defaults = (%d, 0x%02x), want (100, 0x09)", b, temp)
	}
}

func TestStateRemembersLastNonZeroBrightness(t *testing.T) {
	s := NewState("")
	if err := s.Set(40, 0x0C); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(0, 0x0C); err != nil { // off must not clobber the restore target
		t.Fatal(err)
	}
	b, temp := s.TargetOn()
	if b != 40 || temp != 0x0C {
		t.Fatalf("restore target = (%d, 0x%02x), want (40, 0x0C)", b, temp)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-state.json")
	s := NewState(path)
	if err := s.Set(64, 0x12); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(0, 0x12); err != nil { // turned off; the look must survive
		t.Fatal(err)
	}

	s2 := NewState(path) // simulated daemon restart
	if got := s2.StatusString(); got != "unknown" {
		t.Fatalf("restored state must stay unknown until a frame arrives, got %q", got)
	}
	b, temp := s2.TargetOn()
	if b != 64 || temp != 0x12 {
		t.Fatalf("restored target = (%d, 0x%02x), want (64, 0x12)", b, temp)
	}
}

func TestStateSurvivesCorruptStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewState(path)
	b, temp := s.TargetOn()
	if b != 100 || temp != 0x09 {
		t.Fatalf("corrupt file must fall back to defaults, got (%d, 0x%02x)", b, temp)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/light/ -v -run TestState`
Expected: FAIL to build — `undefined: NewState`.

- [ ] **Step 3: Write the implementation**

Create `internal/light/state.go`:

```go
package light

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Defaults used when nothing has been persisted yet.
const (
	defaultBrightness = 100
	defaultKelvin     = 5000
)

// persisted is the on-disk shape of light-state.json.
type persisted struct {
	On         bool `json:"on"`         // last known power state
	Brightness int  `json:"brightness"` // last non-zero brightness (restore target)
	TempByte   int  `json:"temp_byte"`  // last temp byte
}

// State tracks the light's last-known condition. Like the mic's Tracker it
// is tri-state: known=false until the first echo/broadcast or optimistic
// Set. It additionally remembers the last non-zero brightness (the
// "previous look") and persists it, with the temp byte and power state, so
// on/toggle can restore the look across daemon restarts. There is no query
// command, so persisted values seed only the restore targets - never
// "known".
type State struct {
	mu         sync.Mutex
	path       string // persistence file; "" disables persistence
	known      bool
	brightness int       // 0-100; 0 means off
	temp       byte      // hardware temp step 0x00-0x12
	lastOn     int       // last non-zero brightness
	saved      persisted // last snapshot written to disk (skips no-op writes)
}

// NewState loads persisted restore targets from path if it exists. Missing
// or corrupt files silently fall back to defaults (100%, 5000K).
func NewState(path string) *State {
	s := &State{path: path, lastOn: defaultBrightness, temp: KelvinToByte(defaultKelvin)}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var p persisted
	if json.Unmarshal(data, &p) != nil {
		return s
	}
	if p.Brightness > 0 && p.Brightness <= 100 {
		s.lastOn = p.Brightness
	}
	if p.TempByte >= 0 && p.TempByte <= maxTempByte {
		s.temp = byte(p.TempByte)
	}
	s.saved = p
	return s
}

// Set records a known state - from an echo, a knob broadcast, or
// optimistically after a successful write - and persists it if it changed.
// The returned error is a persistence failure only; the in-memory state is
// always updated.
func (s *State) Set(brightness int, temp byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known = true
	s.brightness = brightness
	s.temp = temp
	if brightness > 0 {
		s.lastOn = brightness
	}
	return s.persistLocked()
}

func (s *State) persistLocked() error {
	if s.path == "" {
		return nil
	}
	p := persisted{On: s.brightness > 0, Brightness: s.lastOn, TempByte: int(s.temp)}
	if p == s.saved {
		return nil // no change since last write (knob turns won't spam the disk)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, data, 0o644); err != nil {
		return err
	}
	s.saved = p
	return nil
}

// Status returns the tri-state condition. on is brightness > 0.
func (s *State) Status() (on bool, brightness int, temp byte, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.brightness > 0, s.brightness, s.temp, s.known
}

// TargetOn returns what "turn it on" should send: the last non-zero
// brightness and the current/persisted temp byte.
func (s *State) TargetOn() (brightness int, temp byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOn, s.temp
}

// StatusString renders the UDP status reply: "unknown", "off", or
// "on <brightness>% <kelvin>K" (Kelvin is the quantized hardware step).
func (s *State) StatusString() string {
	on, b, temp, known := s.Status()
	if !known {
		return "unknown"
	}
	if !on {
		return "off"
	}
	return fmt.Sprintf("on %d%% %dK", b, ByteToKelvin(temp))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/light/ -v -run TestState`
Expected: PASS (5 tests).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/light/state.go internal/light/state_test.go
git commit -m "feat: light state tracker with persisted restore targets"
```

---

### Task 4: Manager command handling

**Files:**
- Create: `internal/light/manager.go`
- Test: `internal/light/manager_test.go`

**Interfaces:**
- Consumes: `CCT`, `KelvinToByte`, `MinKelvin`, `MaxKelvin`, `Presets` (Task 1); `NewState`, `(*State).Set/Status/TargetOn/StatusString` (Task 3).
- Produces (Task 5 extends this file; Tasks 6/7 call these):
  - `type Port interface { Write(p []byte) (int, error); Read(p []byte) (int, error); Close() error }` — like `daemon.Device`: a poll-timeout read returns `(0, nil)` (the Windows adapter fixes the 1 s read timeout ONCE at open — never per read, see Task 7); any non-nil error means "device gone".
  - `Manager` field `Present func() bool` — optional device-presence probe (nil disables); the Task 5 session loop uses it as the liveness fallback.
  - `type Manager struct { Logger *log.Logger; ... }`
  - `func NewManager(logger *log.Logger, statePath string) *Manager`
  - `func (m *Manager) HandleCommand(cmd string) string` — receives the command WITHOUT the `light ` prefix, already trimmed (e.g. `"brightness 40"`).
  - `func (m *Manager) setPort(port Port)` (unexported; session/tests use it)
  - `var writeSpacing = 60 * time.Millisecond` (var so tests can shrink it)

Command semantics (locked in; README documents these in Task 10):
- `status` → `StatusString()` (no device write; works even with no port).
- `on` → CCT(restore target). `off` → CCT(0, current temp). `toggle` → off if known-on, otherwise ON at the restore target (unknown state counts as off — a pedal press on a fresh daemon turns the light on).
- `brightness <0-100>` → CCT(n, current temp); n>0 inherently turns on; n=0 is off.
- `temp <2900-7000>` → while on: keep brightness; while off/unknown: set temp AND turn on at the restore brightness (a bare temp change is a "make it look like this" request — documented choice).
- `preset <name>` → CCT(preset pair).
- Success replies are the resulting status string (`on 40% 4950K`, `off`). Failures: `error: no light`, `error: brightness must be 0-100`, `error: temp must be 2900-7000`, `error: unknown preset`, `error: unknown light command`.
- Every write is optimistic (`state.Set` after a successful port write) — the device's byte-for-byte echo re-confirms through the session loop (Task 5), exactly like the mic's `Track.Set` + 0x20 echo pattern.

- [ ] **Step 1: Write the failing test**

Create `internal/light/manager_test.go`:

```go
package light

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakePort implements Port for tests, mirroring the mic's fakeDevice: reads
// block on channels (10ms poll timeout), writes are recorded.
type fakePort struct {
	mu      sync.Mutex
	writes  [][]byte
	reads   chan []byte
	readErr chan error
}

func newFakePort() *fakePort {
	return &fakePort{reads: make(chan []byte, 8), readErr: make(chan error, 1)}
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(p))
	copy(c, p)
	f.writes = append(f.writes, c)
	return len(p), nil
}

func (f *fakePort) Read(p []byte) (int, error) {
	select {
	case data := <-f.reads:
		return copy(p, data), nil
	case err := <-f.readErr:
		return 0, err
	case <-time.After(10 * time.Millisecond):
		return 0, nil // timeout, no data
	}
}

func (f *fakePort) Close() error { return nil }

func (f *fakePort) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakePort) write(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[i]
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func setFastWrites(t *testing.T) {
	t.Helper()
	old := writeSpacing
	writeSpacing = time.Millisecond
	t.Cleanup(func() { writeSpacing = old })
}

func TestHandleCommandWithoutPort(t *testing.T) {
	m := NewManager(testLogger(), "")
	if got := m.HandleCommand("on"); got != "error: no light" {
		t.Fatalf("on without port = %q, want %q", got, "error: no light")
	}
	if got := m.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status needs no port; got %q, want unknown", got)
	}
}

func TestHandleCommandToggleCycle(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	// Unknown state counts as off: toggle turns ON at the defaults.
	if got := m.HandleCommand("toggle"); got != "on 100% 4950K" {
		t.Fatalf("toggle from unknown = %q, want %q", got, "on 100% 4950K")
	}
	wantOn := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD}
	if p.writeCount() != 1 || !bytes.Equal(p.write(0), wantOn) {
		t.Fatalf("frame 0 = % x, want % x", p.write(0), wantOn)
	}

	// Toggle again: off via brightness 0, temp retained.
	if got := m.HandleCommand("toggle"); got != "off" {
		t.Fatalf("toggle from on = %q, want off", got)
	}
	wantOff := []byte{0x3A, 0x02, 0x03, 0x01, 0x00, 0x09, 0x00, 0x49}
	if !bytes.Equal(p.write(1), wantOff) {
		t.Fatalf("frame 1 = % x, want % x", p.write(1), wantOff)
	}

	// Toggle once more: back on at the remembered brightness.
	if got := m.HandleCommand("toggle"); got != "on 100% 4950K" {
		t.Fatalf("toggle from off = %q, want %q", got, "on 100% 4950K")
	}
}

func TestHandleCommandBrightness(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	if got := m.HandleCommand("brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("brightness 40 = %q, want %q", got, "on 40% 4950K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x28, 0x09, 0x00, 0x71}
	if !bytes.Equal(p.write(0), want) {
		t.Fatalf("frame = % x, want % x", p.write(0), want)
	}
	if got := m.HandleCommand("brightness 0"); got != "off" {
		t.Fatalf("brightness 0 = %q, want off", got)
	}
	for _, bad := range []string{"brightness", "brightness 101", "brightness -1", "brightness x"} {
		if got := m.HandleCommand(bad); got != "error: brightness must be 0-100" {
			t.Fatalf("%q = %q, want validation error", bad, got)
		}
	}
	if p.writeCount() != 2 {
		t.Fatalf("invalid commands must not write; wrote %d frames", p.writeCount())
	}
}

func TestHandleCommandTemp(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	// While off/unknown: temp change turns the light ON at the restore
	// brightness (documented choice).
	if got := m.HandleCommand("temp 3400"); got != "on 100% 3356K" {
		t.Fatalf("temp while unknown = %q, want %q", got, "on 100% 3356K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x02, 0x00, 0xA6}
	if !bytes.Equal(p.write(0), want) {
		t.Fatalf("frame = % x, want % x", p.write(0), want)
	}

	// While on: brightness is kept.
	if got := m.HandleCommand("brightness 40"); got != "on 40% 3356K" {
		t.Fatalf("brightness 40 = %q", got)
	}
	if got := m.HandleCommand("temp 7000"); got != "on 40% 7000K" {
		t.Fatalf("temp while on = %q, want %q", got, "on 40% 7000K")
	}
	wantHot := []byte{0x3A, 0x02, 0x03, 0x01, 0x28, 0x12, 0x00, 0x7A}
	if !bytes.Equal(p.write(2), wantHot) {
		t.Fatalf("frame = % x, want % x", p.write(2), wantHot)
	}

	for _, bad := range []string{"temp", "temp 2899", "temp 7001", "temp warm"} {
		if got := m.HandleCommand(bad); got != "error: temp must be 2900-7000" {
			t.Fatalf("%q = %q, want validation error", bad, got)
		}
	}
}

func TestHandleCommandPreset(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	if got := m.HandleCommand("preset candle"); got != "on 28% 3356K" {
		t.Fatalf("preset candle = %q, want %q", got, "on 28% 3356K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x1C, 0x02, 0x00, 0x5E}
	if !bytes.Equal(p.write(0), want) {
		t.Fatalf("frame = % x, want % x", p.write(0), want)
	}
	for _, bad := range []string{"preset", "preset disco"} {
		if got := m.HandleCommand(bad); got != "error: unknown preset" {
			t.Fatalf("%q = %q, want unknown preset error", bad, got)
		}
	}
}

func TestHandleCommandUnknownVerb(t *testing.T) {
	m := NewManager(testLogger(), "")
	for _, bad := range []string{"", "blink", "status extra"} {
		if got := m.HandleCommand(bad); got != "error: unknown light command" {
			t.Fatalf("%q = %q, want unknown light command error", bad, got)
		}
	}
}

func TestHandleCommandOnUsesPersistedLook(t *testing.T) {
	setFastWrites(t)
	path := filepath.Join(t.TempDir(), "light-state.json")

	m1 := NewManager(testLogger(), path)
	p1 := newFakePort()
	m1.setPort(p1)
	m1.HandleCommand("brightness 40")
	m1.HandleCommand("temp 7000")
	m1.HandleCommand("off")

	m2 := NewManager(testLogger(), path) // simulated daemon restart
	p2 := newFakePort()
	m2.setPort(p2)
	if got := m2.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status after restart = %q, want unknown", got)
	}
	if got := m2.HandleCommand("on"); got != "on 40% 7000K" {
		t.Fatalf("on after restart = %q, want %q", got, "on 40% 7000K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x28, 0x12, 0x00, 0x7A}
	if !bytes.Equal(p2.write(0), want) {
		t.Fatalf("restored frame = % x, want % x", p2.write(0), want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/light/ -v -run TestHandleCommand`
Expected: FAIL to build — `undefined: NewManager`.

- [ ] **Step 3: Write the implementation**

Create `internal/light/manager.go`:

```go
package light

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port is the minimal serial handle the manager needs. Like the mic's
// daemon.Device: Read returns (0, nil) when the poll timeout elapses with
// no data (the Windows adapter fixes the 1 s read timeout once, at open -
// re-issuing it per read could race an in-flight Write); any non-nil
// error means "device gone" and triggers a reconnect.
type Port interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Close() error
}

// writeSpacing is the minimum delay before each frame write; the PL81 is
// rate-sensitive (protocol doc: ~60ms). Var so tests can shrink it.
var writeSpacing = 60 * time.Millisecond

var errNoLight = errors.New("no light")

// Manager owns the PL81: the current port handle, the tracked/persisted
// state, and rate-limited frame writes. Commands arrive via HandleCommand
// (from the UDP goroutine); inbound frames arrive via the session loop.
type Manager struct {
	Logger *log.Logger

	// Present optionally reports whether the device is still attached
	// (USB enumeration; nil disables the check). The session loop consults
	// it during long read silences: the CH340 driver's surprise-removal
	// error behavior is unverified, so presence is checked rather than
	// assumed (Task 5).
	Present func() bool

	state *State

	mu        sync.Mutex
	port      Port
	lastWrite time.Time
}

// NewManager returns a Manager whose restore targets are seeded from
// statePath (which may be "" to disable persistence).
func NewManager(logger *log.Logger, statePath string) *Manager {
	return &Manager{Logger: logger, state: NewState(statePath)}
}

// setPort installs the live port handle (nil while disconnected).
func (m *Manager) setPort(port Port) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.port = port
}

// writeFrame sends one frame, honoring the minimum write spacing. Writes
// are serialized by the mutex (the UDP goroutine is the only caller).
func (m *Manager) writeFrame(f []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.port == nil {
		return errNoLight
	}
	if wait := writeSpacing - time.Since(m.lastWrite); wait > 0 {
		time.Sleep(wait)
	}
	_, err := m.port.Write(f)
	m.lastWrite = time.Now()
	return err
}

// HandleCommand executes one "light ..." UDP command (prefix already
// stripped and trimmed, e.g. "brightness 40"). Success replies are the
// resulting status string; failures use the "error: " prefix.
func (m *Manager) HandleCommand(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "error: unknown light command"
	}
	switch fields[0] {
	case "status":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		return m.state.StatusString()
	case "on":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		b, temp := m.state.TargetOn()
		return m.apply(b, temp)
	case "off":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		_, _, temp, _ := m.state.Status()
		return m.apply(0, temp)
	case "toggle":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		on, _, temp, known := m.state.Status()
		if known && on {
			return m.apply(0, temp)
		}
		// Unknown state counts as off: turn ON at the persisted look.
		b, tt := m.state.TargetOn()
		return m.apply(b, tt)
	case "brightness":
		if len(fields) != 2 {
			return "error: brightness must be 0-100"
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 0 || n > 100 {
			return "error: brightness must be 0-100"
		}
		_, _, temp, _ := m.state.Status()
		return m.apply(n, temp)
	case "temp":
		if len(fields) != 2 {
			return "error: temp must be 2900-7000"
		}
		k, err := strconv.Atoi(fields[1])
		if err != nil || k < MinKelvin || k > MaxKelvin {
			return "error: temp must be 2900-7000"
		}
		temp := KelvinToByte(k)
		// While on: keep the brightness. While off/unknown: set temp AND
		// turn on at the restore brightness - a bare temp change is a
		// "make it look like this" request.
		on, b, _, known := m.state.Status()
		if known && on {
			return m.apply(b, temp)
		}
		lb, _ := m.state.TargetOn()
		return m.apply(lb, temp)
	case "preset":
		if len(fields) != 2 {
			return "error: unknown preset"
		}
		p, ok := Presets[fields[1]]
		if !ok {
			return "error: unknown preset"
		}
		return m.apply(p.Brightness, KelvinToByte(p.Kelvin))
	default:
		return "error: unknown light command"
	}
}

// apply sends one CCT frame and optimistically records the result; the
// device's byte-for-byte echo then re-confirms it via the session loop.
func (m *Manager) apply(brightness int, temp byte) string {
	if err := m.writeFrame(CCT(byte(brightness), temp)); err != nil {
		return "error: " + err.Error()
	}
	if err := m.state.Set(brightness, temp); err != nil {
		m.Logger.Printf("light: persist state: %v", err)
	}
	return m.state.StatusString()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/light/ -v`
Expected: PASS (all Task 1–4 tests).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/light/manager.go internal/light/manager_test.go
git commit -m "feat: light manager with rate-limited writes and command handling"
```

---

### Task 5: Manager session and reconnect loop

**Files:**
- Modify: `internal/light/manager.go` (append; nothing existing changes)
- Test: `internal/light/manager_test.go` (append)

**Interfaces:**
- Consumes: `Parser`/`Decoded` (Task 2), `Manager`/`Port`/`setPort`/`writeFrame` (Task 4).
- Produces:
  - `type OpenFunc func() (Port, error)`
  - `func (m *Manager) Run(ctx context.Context, open OpenFunc)` — blocks until ctx is done; `main.go` calls it via `go` (Task 7).
  - `var wakeDelay = 100 * time.Millisecond`, `var openRetryDelay = 3 * time.Second`, `var reconnectDelay = 2 * time.Second`, `var presenceInterval = 10 * time.Second` (vars so tests can shrink them — keeps `-race` runs fast, unlike the mic package's fixed 2s/3s waits).

Behavior locked in (mirrors the mic's `Run` shape from `internal/daemon/daemon.go`):
- Loop while ctx alive: `open()` → on error log + sleep `openRetryDelay` + retry; on success run `session`; on session end close port, log, sleep `reconnectDelay`, retry. `port.Close()` runs only after `session` has returned — reader and closer are the same goroutine, so Close never races Read (go.bug.st issue #219).
- `session`: write wake bytes `00 00 00 00` raw (no frame, no checksum), sleep `wakeDelay`, THEN `setPort(port)` (commands answer `error: no light` until the wake completes), `defer setPort(nil)`; continuous `port.Read(buf)` loop (1 s poll timeout, fixed at open) feeding a `Parser`; every valid CCT frame (tag 0x02, 3-byte payload) updates state via `state.Set(...)` — with inbound `payload[0] != 0x01` mapped to brightness 0, because upstream captures show the pwr byte can carry panel-off state — and is logged with the `light:` prefix (the log file is how E2E confirms echoes); other frames are logged as ignored. NO query-based liveness gate (no query command exists — see design note above); instead, after `presenceInterval` of continuous read silence the loop consults `m.Present` (when non-nil) and ends the session with an error if the device is no longer enumerated.
- All light log lines start with `"light: "` so the two managers' interleaved log lines stay distinguishable.

- [ ] **Step 1: Write the failing test**

Append to `internal/light/manager_test.go` (and add `"context"` and `"errors"` to its import block):

```go
func fastTimings(t *testing.T) {
	t.Helper()
	oldSpacing, oldWake := writeSpacing, wakeDelay
	oldOpen, oldReconnect := openRetryDelay, reconnectDelay
	oldPresence := presenceInterval
	writeSpacing, wakeDelay = time.Millisecond, time.Millisecond
	openRetryDelay, reconnectDelay = 10*time.Millisecond, 10*time.Millisecond
	presenceInterval = time.Millisecond
	t.Cleanup(func() {
		writeSpacing, wakeDelay = oldSpacing, oldWake
		openRetryDelay, reconnectDelay = oldOpen, oldReconnect
		presenceInterval = oldPresence
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionWakesThenAppliesInboundFrames(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.session(ctx, p)
		close(done)
	}()

	waitFor(t, "wake write", func() bool { return p.writeCount() >= 1 })
	if !bytes.Equal(p.write(0), []byte{0x00, 0x00, 0x00, 0x00}) {
		t.Fatalf("first write = % x, want the 00 00 00 00 wake bytes", p.write(0))
	}

	// A knob-style broadcast (50%, temp 0x0C) must update the state.
	p.reads <- []byte{0x3A, 0x02, 0x03, 0x01, 0x32, 0x0C, 0x00, 0x7E}
	waitFor(t, "state from broadcast", func() bool {
		return m.HandleCommand("status") == "on 50% 5633K"
	})

	cancel()
	<-done
}

func TestSessionReturnsOnReadError(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	p.readErr <- errors.New("unplugged")
	err := m.session(context.Background(), p)
	if err == nil || err.Error() != "unplugged" {
		t.Fatalf("session err = %v, want unplugged", err)
	}
	if got := m.HandleCommand("on"); got != "error: no light" {
		t.Fatalf("port must be cleared after session ends; got %q", got)
	}
}

func TestSessionMapsNonOnPwrByteToOff(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.session(ctx, p)
		close(done)
	}()

	waitFor(t, "wake write", func() bool { return p.writeCount() >= 1 })
	// Panel-off style frame: pwr=0x02 with a non-zero brightness field (the
	// official app and non-Pro broadcasts carry off-state in the pwr byte).
	// Must land as "off", never "on 50%".
	p.reads <- []byte{0x3A, 0x02, 0x03, 0x02, 0x32, 0x0C, 0x00, 0x7F}
	waitFor(t, "off state from pwr byte", func() bool {
		return m.HandleCommand("status") == "off"
	})

	cancel()
	<-done
}

func TestSessionEndsWhenDeviceGoesAbsent(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	m.Present = func() bool { return false }
	p := newFakePort()
	err := m.session(context.Background(), p)
	if err == nil || err.Error() != "device no longer present" {
		t.Fatalf("session err = %v, want device-absent error", err)
	}
}

func TestRunReconnectsAfterSessionError(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	ports := []*fakePort{newFakePort(), newFakePort()}
	var mu sync.Mutex
	opened := 0
	open := func() (Port, error) {
		mu.Lock()
		defer mu.Unlock()
		if opened >= len(ports) {
			return nil, errors.New("gone")
		}
		p := ports[opened]
		opened++
		return p, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, open)
		close(done)
	}()

	waitFor(t, "first port woken", func() bool { return ports[0].writeCount() >= 1 })
	ports[0].readErr <- errors.New("unplugged")
	waitFor(t, "second port opened and woken", func() bool { return ports[1].writeCount() >= 1 })

	cancel()
	<-done
}

func TestRunRetriesWhenOpenFails(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	var mu sync.Mutex
	failFirst := true
	open := func() (Port, error) {
		mu.Lock()
		defer mu.Unlock()
		if failFirst {
			failFirst = false
			return nil, errors.New("not present")
		}
		return p, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, open)
		close(done)
	}()

	waitFor(t, "port opened after a failed attempt", func() bool { return p.writeCount() >= 1 })

	cancel()
	<-done
}

func TestCommandsWorkDuringLiveSession(t *testing.T) {
	fastTimings(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	open := func() (Port, error) { return p, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, open)
		close(done)
	}()

	waitFor(t, "wake", func() bool { return p.writeCount() >= 1 })
	if got := m.HandleCommand("on"); got != "on 100% 4950K" {
		t.Fatalf("on during live session = %q", got)
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD}
	waitFor(t, "command frame written", func() bool { return p.writeCount() >= 2 })
	if !bytes.Equal(p.write(1), want) {
		t.Fatalf("frame = % x, want % x", p.write(1), want)
	}

	// The echo arrives; state stays consistent.
	p.reads <- want
	waitFor(t, "echo folded into state", func() bool {
		return m.HandleCommand("status") == "on 100% 4950K"
	})

	cancel()
	<-done
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/light/ -v -run 'TestSession|TestRun|TestCommandsWork'`
Expected: FAIL to build — `undefined: wakeDelay`, `undefined: m.session`, `undefined: m.Run`.

- [ ] **Step 3: Write the implementation**

Append to `internal/light/manager.go` (and add `"context"` to its imports):

```go
// OpenFunc opens the PL81's serial port (enumerated by VID/PID).
type OpenFunc func() (Port, error)

// Timing knobs, vars so tests can shrink them.
var (
	wakeDelay      = 100 * time.Millisecond // settle time after the wake bytes
	openRetryDelay = 3 * time.Second        // "not present yet" backoff
	reconnectDelay = 2 * time.Second        // "was here, went away" backoff
	// presenceInterval is how long the session tolerates continuous read
	// silence before consulting Manager.Present (nil = never). A silent
	// idle light is normal (no status stream), but the CH340 driver's
	// surprise-removal error behavior is unverified - so during long
	// silences the device's continued USB presence is checked directly.
	presenceInterval = 10 * time.Second
)

// wakeBytes is the raw wake sequence (not a frame - no header/checksum).
var wakeBytes = []byte{0x00, 0x00, 0x00, 0x00}

// Run maintains the light session until ctx is cancelled: open, wake, read
// continuously, reconnect on any error. Mirrors the mic's reconnect loop in
// internal/daemon.
func (m *Manager) Run(ctx context.Context, open OpenFunc) {
	for ctx.Err() == nil {
		port, err := open()
		if err != nil {
			m.Logger.Printf("light: open: %v (retrying in %v)", err, openRetryDelay)
			sleepCtx(ctx, openRetryDelay)
			continue
		}
		m.Logger.Printf("light: port opened")
		err = m.session(ctx, port)
		// Close only after session has returned: reader and closer are the
		// same goroutine, so Close never races a pending Read (go.bug.st
		// issue #219).
		port.Close()
		if ctx.Err() != nil {
			break
		}
		m.Logger.Printf("light: session ended: %v (reconnecting in %v)", err, reconnectDelay)
		sleepCtx(ctx, reconnectDelay)
	}
}

// session wakes the device then reads frames until the port errors
// (unplug) or ctx is cancelled. Echoes of our own commands and unprompted
// knob broadcasts look identical and both simply update the state.
//
// Unlike the mic session there is deliberately NO query-based liveness
// gate: the PL81 has no query command, so a healthy idle light is silent
// and cannot be distinguished from a dead one by silence alone. Instead,
// during long silences the loop re-checks that the device is still
// enumerated (Manager.Present) - the CH340 driver is not trusted to
// surface an unplug as a read error.
func (m *Manager) session(ctx context.Context, port Port) error {
	if _, err := port.Write(wakeBytes); err != nil {
		return err
	}
	time.Sleep(wakeDelay)
	m.setPort(port)
	defer m.setPort(nil)

	var parser Parser
	buf := make([]byte, 64)
	lastData := time.Now()
	for ctx.Err() == nil {
		n, err := port.Read(buf) // 1 s poll timeout, fixed at open
		if err != nil {
			return err
		}
		if n == 0 { // timeout, no data
			if m.Present != nil && time.Since(lastData) >= presenceInterval {
				if !m.Present() {
					return errors.New("device no longer present")
				}
				lastData = time.Now() // present; recheck one interval from now
			}
			continue
		}
		lastData = time.Now()
		for _, fr := range parser.Feed(buf[:n]) {
			if fr.Tag == TagCCT && len(fr.Payload) == 3 {
				// Upstream captures show the pwr byte can carry panel-off
				// state (0x00/0x02) with a non-zero brightness field; treat
				// anything but 0x01 as OFF so status never lies "on".
				b := int(fr.Payload[1])
				if fr.Payload[0] != 0x01 {
					b = 0
				}
				if err := m.state.Set(b, fr.Payload[2]); err != nil {
					m.Logger.Printf("light: persist state: %v", err)
				}
				m.Logger.Printf("light: frame pwr=0x%02x brightness=%d temp=0x%02x -> %s",
					fr.Payload[0], fr.Payload[1], fr.Payload[2], m.state.StatusString())
			} else {
				m.Logger.Printf("light: frame tag=0x%02x payload=% x (ignored)", fr.Tag, fr.Payload)
			}
		}
	}
	return ctx.Err()
}

func sleepCtx(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/light/ -v`
Expected: PASS (all light package tests).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/light/manager.go internal/light/manager_test.go
git commit -m "feat: light session and reconnect loop with wake sequence"
```

---

### Task 6: Route `light ...` UDP commands through the daemon

**Files:**
- Modify: `internal/daemon/daemon.go`
- Modify: `internal/daemon/daemon_test.go`
- Modify: `main.go` (one line — keep the tree compiling)

**Interfaces:**
- Consumes: nothing from `internal/light` (deliberately — the interface keeps `internal/daemon` decoupled).
- Produces:
  - `type CommandHandler interface { HandleCommand(cmd string) string }` (in package `daemon`)
  - `Daemon` struct field `Light CommandHandler`
  - NEW signature: `func Run(ctx context.Context, open OpenFunc, light CommandHandler, pc net.PacketConn, logger *log.Logger) error`
  - Dispatch rule: any command equal to `light` or starting with `light ` goes to `d.Light.HandleCommand(<rest, trimmed>)`; if `d.Light` is nil the reply is `error: no light support`. `lightning` etc. must NOT match.

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/daemon_test.go`:

```go
type fakeLightHandler struct {
	reply string
	got   []string
}

func (f *fakeLightHandler) HandleCommand(cmd string) string {
	f.got = append(f.got, cmd)
	return f.reply
}

func TestHandleCommandRoutesLightPrefix(t *testing.T) {
	d := New(testLogger())
	f := &fakeLightHandler{reply: "on 100% 4950K"}
	d.Light = f
	if got := d.HandleCommand("light toggle"); got != "on 100% 4950K" {
		t.Fatalf("reply = %q, want the handler's reply", got)
	}
	if got := d.HandleCommand("light brightness 40"); got != "on 100% 4950K" {
		t.Fatalf("reply = %q, want the handler's reply", got)
	}
	want := []string{"toggle", "brightness 40"}
	if len(f.got) != 2 || f.got[0] != want[0] || f.got[1] != want[1] {
		t.Fatalf("handler received %v, want %v", f.got, want)
	}
}

func TestHandleCommandLightWithoutHandler(t *testing.T) {
	d := New(testLogger())
	if got := d.HandleCommand("light toggle"); got != "error: no light support" {
		t.Fatalf("reply = %q, want %q", got, "error: no light support")
	}
}

func TestHandleCommandDoesNotRouteLightPrefixWords(t *testing.T) {
	d := New(testLogger())
	d.Light = &fakeLightHandler{reply: "x"}
	if got := d.HandleCommand("lightning"); got != "error: unknown command" {
		t.Fatalf("reply = %q, want unknown command", got)
	}
}

func TestLightCommandOverUDPWithoutManager(t *testing.T) {
	open := func() (Device, error) { return newFakeDevice(), nil }
	_, ask := startDaemon(t, open)
	if got := ask("light status"); got != "error: no light support" {
		t.Fatalf("UDP reply = %q, want %q", got, "error: no light support")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestHandleCommandRoutesLightPrefix|TestHandleCommandLightWithoutHandler|TestHandleCommandDoesNotRouteLightPrefixWords|TestLightCommandOverUDPWithoutManager' -v`
Expected: FAIL to build — `d.Light undefined`.

- [ ] **Step 3: Implement the dispatch**

In `internal/daemon/daemon.go`:

3a. Below the `Device` interface, add:

```go
// CommandHandler answers one already-trimmed command string. It is how
// device managers other than the mic (today: the PL81 light) plug their
// verbs into the UDP surface without this package importing them.
type CommandHandler interface {
	HandleCommand(cmd string) string
}
```

3b. Add the field to the `Daemon` struct — change:

```go
type Daemon struct {
	Track  Tracker
	Logger *log.Logger

	mu  sync.Mutex
	dev Device
}
```

to:

```go
type Daemon struct {
	Track  Tracker
	Logger *log.Logger
	Light  CommandHandler // nil when no light support is wired in

	mu  sync.Mutex
	dev Device
}
```

3c. At the TOP of `HandleCommand`, before the existing `switch cmd {`, insert:

```go
	if rest, ok := strings.CutPrefix(cmd, "light"); ok && (rest == "" || rest[0] == ' ') {
		if d.Light == nil {
			return "error: no light support"
		}
		return d.Light.HandleCommand(strings.TrimSpace(rest))
	}
```

(`strings` is already imported by this file for `serveUDP`.) Also replace `HandleCommand`'s doc comment — change:

```go
// HandleCommand executes one UDP text command and returns the reply.
// Replies are exactly: "muted", "unmuted", "unknown", or "error: <reason>".
```

to:

```go
// HandleCommand executes one UDP text command and returns the reply.
// Mic replies are exactly: "muted", "unmuted", "unknown", or
// "error: <reason>". Commands starting with "light" are delegated to
// d.Light, whose replies are the light's status strings ("on 64% 4950K",
// "off", "unknown") or "error: <reason>".
```

(If the existing comment's wording differs slightly, adapt the old block to match the file — the new comment text stands.)

3d. Change `Run`'s signature and wire the field — change:

```go
func Run(ctx context.Context, open OpenFunc, pc net.PacketConn, logger *log.Logger) error {
	d := New(logger)
```

to:

```go
func Run(ctx context.Context, open OpenFunc, light CommandHandler, pc net.PacketConn, logger *log.Logger) error {
	d := New(logger)
	d.Light = light
```

- [ ] **Step 4: Fix every `Run` call site**

Run: `go build ./... 2>&1` — the compiler lists them all. Expected call sites:
- `internal/daemon/daemon_test.go`: the `startDaemon` helper's `Run(ctx, open, pc, testLogger())` → `Run(ctx, open, nil, pc, testLogger())`; plus any other direct `Run(` calls in that file (fix each the same way — insert `nil` as the third argument).
- `main.go`: `daemon.Run(context.Background(), open, pc, logger)` → `daemon.Run(context.Background(), open, nil, pc, logger)` (temporary; Task 7 replaces the `nil` with the real manager).

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/daemon/ -v`
Expected: PASS — the 4 new tests AND every pre-existing daemon test (`TestServeUDPSurvivesTransientErrors` in particular must stay green).

- [ ] **Step 6: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go main.go
git commit -m "feat: route light-prefixed UDP commands via CommandHandler"
```

---

### Task 7: Serial port open, CLI `light` subcommand, daemon wiring, cross-compile

**Files:**
- Create: `light_windows.go`, `light_other.go` (package `main`)
- Modify: `main.go`, `go.mod`, `go.sum`
- Test: `main_test.go` (append)

**Interfaces:**
- Consumes: `light.NewManager`, `(*light.Manager).Run`, `light.Port`, `light.OpenFunc` (Tasks 4–5); `daemon.Run` new signature (Task 6); existing `runClient(cmd, addr string, timeout time.Duration, out io.Writer) int` and `const udpAddr = "127.0.0.1:42814"`.
- Produces:
  - `func openPL81(logger *log.Logger) (light.Port, error)` (windows real / other stub)
  - `func pl81Present() bool` (windows real / other stub) — read-only VID/PID re-enumeration for the session's presence check
  - `func lightStatePath() string`
  - CLI: `mutastic light <args...>` → joins `os.Args[1:]` with single spaces, sends over UDP with a **2 second** timeout (light writes are paced ~60 ms; 2 s gives headroom, and the mic commands keep their existing 1 s).

- [ ] **Step 1: Add the serial dependency**

```bash
cd /home/dan/code/mutastic/.worktrees/pl81-light-control
go get go.bug.st/serial@v1.8.0
go mod tidy
```

Expected: `go.mod` gains `go.bug.st/serial v1.8.0` (plus indirect deps, e.g. `github.com/creack/goselect`, `golang.org/x/sys`); `go build ./...` still succeeds. v1.8.0 is pinned deliberately — it is the version validated on 2026-08-08 (cross-compiles under this repo's exact cgo/mingw env AND with `CGO_ENABLED=0`; API surface confirmed against module source). Do not float to `@latest`.

- [ ] **Step 2: Write the client test**

Append to `main_test.go` (match the existing test style; add any missing imports — `bytes`, `net`, `strings`, `time` are likely already present):

```go
func TestRunClientPassesMultiWordCommandVerbatim(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	gotCmd := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		gotCmd <- string(buf[:n])
		pc.WriteTo([]byte("on 100% 4950K"), addr)
	}()

	var out bytes.Buffer
	code := runClient("light brightness 100", pc.LocalAddr().String(), time.Second, &out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (output %q)", code, out.String())
	}
	if got := <-gotCmd; got != "light brightness 100" {
		t.Fatalf("daemon received %q, want %q", got, "light brightness 100")
	}
	if got := strings.TrimSpace(out.String()); got != "on 100% 4950K" {
		t.Fatalf("printed %q, want the reply", got)
	}
}
```

Run: `go test . -run TestRunClientPassesMultiWordCommandVerbatim -v`
Expected: PASS immediately (`runClient` already handles arbitrary strings — this test pins the multi-word contract the new CLI case relies on). That is acceptable: the failing-test steps for this task are the build failures fixed in Steps 3–5.

- [ ] **Step 3: Create the Windows serial open + adapter**

Create `light_windows.go`:

```go
//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	"mutastic/internal/light"
)

// PL81 Pro = CH340 USB-serial bridge (docs/pl81-pro-serial-protocol.md).
// The enumerator reports VID/PID as hex strings.
const (
	pl81VID = "1A86"
	pl81PID = "7523"
)

// openPL81 finds the CH340 bridge by USB VID/PID - NEVER by COM number, the
// port name can change - and opens it at 115200 8N1 with both buffers
// flushed. The wake sequence is the session's job, not this function's.
// Every candidate port is logged so the log file doubles as a diagnostic
// record, mirroring openYetiX.
func openPL81(logger *log.Logger) (light.Port, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	var name string
	for _, p := range ports {
		logger.Printf("light: serial port: %s usb=%v vid=%s pid=%s", p.Name, p.IsUSB, p.VID, p.PID)
		if name == "" && p.IsUSB && strings.EqualFold(p.VID, pl81VID) && strings.EqualFold(p.PID, pl81PID) {
			name = p.Name
		}
	}
	if name == "" {
		return nil, errors.New("PL81 (CH340, VID 1A86 PID 7523) not found")
	}
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
		// The proven 2026-08-08 probe ran on .NET SerialPort defaults: DTR
		// and RTS DEASSERTED. go.bug.st's default asserts both, and CH340
		// boards are often line-state sensitive - replicate the proven
		// configuration explicitly; trust neither stack's default.
		InitialStatusBits: &serial.ModemOutputBits{RTS: false, DTR: false},
	}
	port, err := serial.Open(name, mode)
	if err != nil {
		// Typically "access denied" if something else holds the port
		// exclusively (e.g. NEEWER Control Center).
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	if err := port.ResetInputBuffer(); err != nil {
		port.Close()
		return nil, err
	}
	if err := port.ResetOutputBuffer(); err != nil {
		port.Close()
		return nil, err
	}
	// Fix the poll timeout exactly once, here, before the port is shared:
	// v1.8.0 opens in NoTimeout mode (a Read would block forever), and
	// re-issuing SetCommTimeouts per read can race an in-flight Write.
	if err := port.SetReadTimeout(time.Second); err != nil {
		port.Close()
		return nil, err
	}
	return serialPort{port}, nil
}

// serialPort adapts go.bug.st/serial to light.Port. The 1 s read timeout
// was fixed once in openPL81, so a Read that returns (0, nil) on expiry
// matches the Port contract exactly - no per-read SetReadTimeout (which
// could race an in-flight Write via SetCommTimeouts).
type serialPort struct {
	p serial.Port
}

func (s serialPort) Write(b []byte) (int, error) { return s.p.Write(b) }

func (s serialPort) Read(b []byte) (int, error) { return s.p.Read(b) }

func (s serialPort) Close() error { return s.p.Close() }

// pl81Present reports whether a 1A86:7523 device is currently enumerated
// (SetupAPI, present devices only). The session loop uses it as its
// liveness fallback during long read silences, because the CH340 driver's
// surprise-removal error behavior is unverified. Enumeration failures
// count as present (fail open - never kill a session on an enumerator
// glitch).
func pl81Present() bool {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return true
	}
	for _, p := range ports {
		if p.IsUSB && strings.EqualFold(p.VID, pl81VID) && strings.EqualFold(p.PID, pl81PID) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Create the non-Windows stub**

Create `light_other.go` (same pattern as `hid_other.go`, so `go test ./...` keeps working from WSL):

```go
//go:build !windows

package main

import (
	"errors"
	"log"

	"mutastic/internal/light"
)

func openPL81(_ *log.Logger) (light.Port, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

// pl81Present is never consulted off-Windows (no session can ever start).
func pl81Present() bool { return false }
```

- [ ] **Step 5: Wire the CLI and the daemon**

In `main.go`:

5a. Update the package doc comment (lines 1–5) to:

```go
// mutastic controls the Blue Yeti X hardware mute and the NEEWER PL81 PRO
// streaming light.
//
//	mutastic daemon                     resident: HID + serial sessions + UDP server
//	mutastic toggle|mute|unmute|status  one-shot client: mic hardware mute
//	mutastic light <subcommand...>      one-shot client: light control
package main
```

5b. In `main()`'s switch, after the existing `case "toggle", "mute", "unmute", "status":` arm, add:

```go
	case "light":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		os.Exit(runClient(strings.Join(os.Args[1:], " "), udpAddr, 2*time.Second, os.Stdout))
```

(add `"strings"` to main.go's imports if not present).

5c. Replace `usage()` with:

```go
func usage() {
	fmt.Fprintln(os.Stderr, "usage: mutastic daemon | toggle | mute | unmute | status")
	fmt.Fprintln(os.Stderr, "       mutastic light toggle|on|off|status")
	fmt.Fprintln(os.Stderr, "       mutastic light brightness <0-100> | temp <2900-7000> | preset <cold|sunlight|afternoon|sunset|candle>")
}
```

5d. In `runDaemon()`, replace:

```go
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	daemon.Run(context.Background(), open, nil, pc, logger)
```

with:

```go
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	ctx := context.Background()
	lm := light.NewManager(logger, lightStatePath())
	lm.Present = pl81Present
	go lm.Run(ctx, func() (light.Port, error) { return openPL81(logger) })
	daemon.Run(ctx, open, lm, pc, logger)
```

5e. Add to `main.go` (near `openLogFile`), with `"path/filepath"` and `"mutastic/internal/light"` added to the imports:

```go
// lightStatePath returns %LOCALAPPDATA%\mutastic\light-state.json (the same
// directory as mutastic.log). An empty string disables persistence rather
// than failing the daemon.
func lightStatePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mutastic", "light-state.json")
}
```

- [ ] **Step 6: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean (on Linux the windows file is excluded by the build tag; the stub keeps package main compiling).

- [ ] **Step 7: Cross-compile for Windows**

Run: `./build.sh`
Expected: `built bin/mutastic.exe` — this is the ONLY step that compiles `light_windows.go`, so treat a failure here as a Task 7 failure (fix the windows file, not the gate).

- [ ] **Step 8: Commit**

```bash
git add light_windows.go light_other.go main.go main_test.go go.mod go.sum
git commit -m "feat: PL81 serial open by VID/PID, mutastic light CLI, daemon wiring"
```

---

### Task 8: AHK F13 hotkey and pedal doc

**Files:**
- Modify: `ahk/MuteAllMeetings.ahk` (UTF-8 BOM + CRLF — byte-level edit only)
- Modify: `docs/pedal-and-mute.md`

**Interfaces:**
- Consumes: the deployed `mutastic.exe light toggle` CLI (Task 7).
- Produces: F13 hotkey block (F14 body and F15 untouched).

- [ ] **Step 1: Record the pre-edit encoding facts**

```bash
cd /home/dan/code/mutastic/.worktrees/pl81-light-control
file ahk/MuteAllMeetings.ahk        # expect: UTF-8 (with BOM), CRLF
head -c 3 ahk/MuteAllMeetings.ahk | xxd   # expect: efbb bf
wc -l ahk/MuteAllMeetings.ahk       # expect: 93
```

- [ ] **Step 2: Make the byte-level edit**

The file must be edited at the byte level to guarantee BOM + CRLF survive. Run exactly (note: the heredoc delimiter is `PYEOF`):

```bash
cd /home/dan/code/mutastic/.worktrees/pl81-light-control
python3 - <<'PYEOF'
from pathlib import Path

p = Path("ahk/MuteAllMeetings.ahk")
data = p.read_bytes()
assert data.startswith(b"\xef\xbb\xbf"), "BOM missing before edit"

# 1. F13 hotkey block, inserted after the F14 block (lines 25-28).
old_f14 = (b"F14::\r\n"
           b"Run, \"%A_ScriptDir%\\mutastic.exe\" toggle, %A_ScriptDir%, Hide UseErrorLevel\r\n"
           b"ToggleAllMeetings()\r\n"
           b"return\r\n")
new_f14 = old_f14 + (b"\r\n"
           b"F13::\r\n"
           b"Run, \"%A_ScriptDir%\\mutastic.exe\" light toggle, %A_ScriptDir%, Hide UseErrorLevel\r\n"
           b"return\r\n")
assert data.count(old_f14) == 1, "F14 block not found exactly once"
data = data.replace(old_f14, new_f14)

# 2. Tray tip now advertises both pedals.
old_tip = b"Menu, Tray, Tip, MuteAllMeetings - F14 toggles mute in all meetings\r\n"
new_tip = b"Menu, Tray, Tip, MuteAllMeetings - F14 mutes meetings+mic, F13 toggles light\r\n"
assert data.count(old_tip) == 1, "tray tip line not found exactly once"
data = data.replace(old_tip, new_tip)

# 3. Header comment: mention F13.
old_hdr = (b"; Middle USB foot pedal (F14) toggles microphone mute in ALL running\r\n"
           b"; meeting apps at once: MS Teams, Zoom, Webex, and Google Meet tabs.\r\n")
new_hdr = old_hdr + b"; Left pedal (F13) toggles the NEEWER PL81 PRO light via mutastic.\r\n"
assert data.count(old_hdr) == 1, "header lines not found exactly once"
data = data.replace(old_hdr, new_hdr)

# 4. Fix the stale doc pointer (foot-pedal.md was retired).
old_ptr = (b"; Local tool for this machine. Documented in\r\n"
           b"; ~/code/this-machine-projects/docs/foot-pedal.md\r\n")
new_ptr = (b"; Local tool for this machine. Documented in the mutastic repo:\r\n"
           b"; README.md and docs/pedal-and-mute.md\r\n")
assert data.count(old_ptr) == 1, "doc pointer lines not found exactly once"
data = data.replace(old_ptr, new_ptr)

p.write_bytes(data)
print("edit ok")
PYEOF
```

Expected: `edit ok`. If any assertion fails, the file differs from what this plan recorded — STOP, run `git checkout -- ahk/MuteAllMeetings.ahk`, read the actual file bytes, and adapt the exact old-byte strings before retrying. Never hand-rewrite the file.

- [ ] **Step 3: Verify encoding and diff**

```bash
file ahk/MuteAllMeetings.ahk
# Expected: Unicode text, UTF-8 (with BOM) text, with CRLF line terminators
wc -l < ahk/MuteAllMeetings.ahk && grep -c $'\r' ahk/MuteAllMeetings.ahk
# Expected: both 98 (93 + 4 new F13/blank lines + 1 new header line; pointer swap is line-neutral)
git diff --stat ahk/MuteAllMeetings.ahk
# Expected: small +/- counts. If the diff shows every line changed, the line
# endings were destroyed: git checkout -- ahk/MuteAllMeetings.ahk and redo.
git diff ahk/MuteAllMeetings.ahk
# Expected: only the four edits above.
```

- [ ] **Step 4: AHK syntax check (WSL interop)**

```bash
"/mnt/c/Program Files/AutoHotkey/AutoHotkeyU64.exe" /ErrorStdOut /iLib nul \
  "$(wslpath -w /home/dan/code/mutastic/.worktrees/pl81-light-control/ahk/MuteAllMeetings.ahk)"; echo "exit=$?"
```

Expected: no output, `exit=0`. Contingencies: if the interpreter rejects the UNC path, copy the .ahk to `/mnt/c/Users/dan/AppData/Local/Temp/` and check it there. If WSL interop itself fails (cmd/powershell unreachable), HALT and surface the blocker — do not skip the check silently.

- [ ] **Step 5: Update the pedal table**

In `docs/pedal-and-mute.md`, find the pedal table row (verify exact current text first by reading the file):

```markdown
| Left | `F13` | (unassigned as of 2026-08-08) |
```

replace with:

```markdown
| Left | `F13` | mutastic light toggle (NEEWER PL81 PRO streaming light) |
```

Also check for other stale "unassigned" mentions of F13 in that file and update them consistently:
`grep -n "F13\|unassigned" docs/pedal-and-mute.md`

- [ ] **Step 6: Commit**

```bash
git add ahk/MuteAllMeetings.ahk docs/pedal-and-mute.md
git commit -m "feat: F13 pedal toggles the PL81 light via AHK"
```

---

### Task 9: Deploy and live E2E acceptance (light + mic regression)

**Files:** none created/modified in the repo (verification task). If a live failure requires a code fix, fix it, re-run the full gate, commit the fix with its own focused message, and repeat this task from Step 2.

**Interfaces:**
- Consumes: everything. The deployed exe at `C:\Users\dan\code\mutastic-deploy\mutastic.exe`, the daemon log at `%LOCALAPPDATA%\mutastic\mutastic.log`, and the physically connected, currently-OFF PL81.
- Produces: recorded pass/fail evidence for Task 10's doc updates, at `/home/dan/code/mutastic/.worktrees/.the-usual-logs/pl81-light-control/e2e-results.md`.

All Windows-side steps use WSL interop. **Pre-flight is mandatory; if interop is broken at any point, HALT this task and surface it as a blocker — do not fabricate results.**

- [ ] **Step 1: Pre-flight interop check**

```bash
cmd.exe /c echo interop-ok
```
Expected: `interop-ok`. Anything else → HALT, report blocker.

- [ ] **Step 2: Fresh build + gate**

```bash
cd /home/dan/code/mutastic/.worktrees/pl81-light-control
go test -race ./... && go vet ./... && ./build.sh
```
Expected: clean gate, `built bin/mutastic.exe`.

- [ ] **Step 3: Deploy (single quotes are load-bearing)**

```bash
cmd.exe /c '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\pl81-light-control\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\pl81-light-control'
echo "exit=$?"
```
Expected: `== Stopping running instances ==` … `Deploy complete.`, `exit=0`. (A "UNC paths are not supported" cwd warning from cmd is harmless.) No structural change to deploy.cmd was needed — it already copies the one exe and one ahk.

- [ ] **Step 4: Verify deployment state**

```bash
sleep 8   # give the daemon time to bind UDP, open both devices, and wake the light
ls -l /mnt/c/Users/dan/code/mutastic-deploy/
ls "/mnt/c/Users/dan/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/"
tasklist.exe | grep -iE 'mutastic|AutoHotkey'
```
Expected: fresh `mutastic.exe` + `MuteAllMeetings.ahk` timestamps; both `Mutastic Daemon.lnk` and `MuteAllMeetings.lnk` present; both `mutastic.exe` and `AutoHotkeyU64.exe` in the process list. If `mutastic.exe` is NOT running, check Windows Defender first (`powershell.exe -NoProfile -Command 'Get-MpThreatDetection | Select-Object -First 5'`) — unsigned stripped cgo binaries are a known false-positive class; the recorded contingency is rebuilding without `-ldflags "-s -w"`.

```bash
tail -30 /mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log
```
Expected: daemon startup lines, HID enumeration, AND new `light:` lines — `light: serial port: COM... vid=1A86 pid=7523` followed by `light: port opened`. If instead `light: open: ... not found (retrying in 3s)` repeats, the light is unplugged/asleep → surface to the user, do not proceed to Step 5.

- [ ] **Step 5: Light E2E (echo-confirmed, software-verifiable)**

```bash
D=/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe
L=/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log
$D light status        # expect: unknown  (fresh daemon, no echo yet)
$D light on            # expect: on 100% 4950K   (defaults; or the persisted look on re-runs)
sleep 1
$D light status        # expect: same "on ..." string
grep 'light: frame' $L | tail -5
```
Expected: an echo line matching the sent command, e.g.
`light: frame pwr=0x01 brightness=100 temp=0x09 -> on 100% 4950K`
This is the **echo-confirmed ON**: the frame came back from the hardware, not from optimism. The human can also see the light is on, but the log line is the acceptance evidence.

```bash
$D light off           # expect: off
sleep 1
$D light status        # expect: off
grep 'light: frame' $L | tail -3
cat /mnt/c/Users/dan/AppData/Local/mutastic/light-state.json
```
Expected: echo line with `brightness=0` (**echo-confirmed OFF**); state file exists with `{"on":false,"brightness":100,"temp_byte":9}` (brightness = last non-zero look).

Optional extra coverage (leave the light OFF afterwards): `$D light brightness 30`, `$D light temp 3400`, `$D light preset candle`, each replying `on ...`, then `$D light off`. Record which commands were exercised for Task 10.

- [ ] **Step 6: Mic regression**

```bash
$D status              # expect: muted | unmuted | unknown (any non-error reply)
$D mute                # expect: muted
sleep 1
$D unmute              # expect: unmuted
sleep 1
$D status              # expect: unmuted
grep 'event op=' $L | tail -4
```
Expected: the mic still answers, and the log shows `event op=0x2? ... -> muted=true` then `-> muted=false` ack lines for the mute/unmute pair — proving the mic session still receives device echoes with the light manager running alongside.

- [ ] **Step 7: Enforce the end state**

```bash
$D light status        # MUST print: off
$D status              # MUST print: unmuted
```
If either differs, issue `$D light off` / `$D unmute` and re-check. Do not finish this task with the light on or the mic muted.

- [ ] **Step 8: Record results**

Write the observed evidence (exact replies, the echo log lines, whether any knob broadcasts appeared in the log, any deviations) to `/home/dan/code/mutastic/.worktrees/.the-usual-logs/pl81-light-control/e2e-results.md` for Task 10 to consume. This file lives outside the repo — nothing to commit. If code fixes were made during this task, they were already committed in their own commits.

---

### Task 10: README and protocol doc updates, recorded human questions

**Files:**
- Modify: `README.md`
- Modify: `docs/pl81-pro-serial-protocol.md`

**Interfaces:**
- Consumes: Task 9's recorded evidence (`/home/dan/code/mutastic/.worktrees/.the-usual-logs/pl81-light-control/e2e-results.md`).
- Produces: end-user docs. No code changes.

- [ ] **Step 1: Update README.md**

1a. Intro — after the existing numbered list item 2 (`**mutastic daemon (Go)** ...`), add a new paragraph:

```markdown
The left pedal (`F13`) toggles a NEEWER PL81 PRO LED streaming light: the
AHK script runs `mutastic.exe light toggle`, and the same daemon drives the
light over its CH340 USB-serial port.
```

1b. `## Components` — extend the daemon bullet: after `Reconnects automatically if the mic disappears.` append (same bullet):

```markdown
  Also owns the NEEWER PL81 PRO light (CH340 serial, VID 1A86 PID 7523,
  115200 8N1) with its own independent reconnect loop, tracking the light's
  true state from its echo/broadcast frames and persisting the last look to
  `%LOCALAPPDATA%\mutastic\light-state.json`.
```

1c. `## Components` — after the existing one-shot client bullet, add:

```markdown
- **Light commands** — `mutastic light toggle | on | off | status`,
  `mutastic light brightness <0-100>`, `mutastic light temp <2900-7000>`,
  `mutastic light preset <cold|sunlight|afternoon|sunset|candle>`.
  Replies: `on 64% 4950K`, `off`, `unknown`, or `error: <reason>` (same
  exit codes as the mic commands). Notes: OFF is brightness 0 (the panel
  has no working power command); `on`/`toggle` restore the last non-zero
  brightness and temperature (default 100% / 5000 K); setting `temp` while
  the light is off turns it on at the restored brightness; temperatures
  are quantized to the panel's 19 hardware steps (~228 K), so `temp 5000`
  reads back as `4950K`; `status` is `unknown` after a daemon restart
  until the light first echoes or its knob is touched (the hardware has no
  query command).
```

1d. `## Components` — update the AHK bullet to cover both pedals:

```markdown
- **`ahk/MuteAllMeetings.ahk`** — the F14 handler runs
  `mutastic.exe toggle` (hidden, non-blocking) and then toggles the meeting
  apps as before; the F13 handler runs `mutastic.exe light toggle` the same
  way.
```

1e. `## Troubleshooting` — add bullets after the existing `Mic unplugged/replugged` bullet:

```markdown
- **Light unplugged/replugged:** same as the mic — the daemon logs
  `light: session ended` and reopens the port automatically.
- **`light ...` says `error: no light`:** the CH340 port wasn't found or
  couldn't be opened. The COM port is exclusive — close NEEWER Control
  Center (or anything else holding the port) and check the log's
  `light: serial port:` enumeration lines.
- **Light state file:** `%LOCALAPPDATA%\mutastic\light-state.json` holds
  the restore-on-`on` look; deleting it just resets the defaults
  (100% / 5000 K).
```

1f. Closing "See …" paragraph — replace the doc list sentence with:

```markdown
See `docs/yeti-x-hid-protocol.md` for the mic's reverse-engineered HID
protocol, `docs/pl81-pro-serial-protocol.md` for the light's serial
protocol, and `docs/pedal-and-mute.md` for the machine setup (pedal
firmware mapping, deployment, install state).
```

- [ ] **Step 2: Update the protocol doc with confirmed facts**

2a. FIX the transport section's stray claim first: the doc currently says the device "streams status at 60–80 ms intervals" — that line is a mis-import of the Rokkit (non-Pro) README's "Frame timing: repeats every ~60-80ms", which describes how fast knob-adjustment *broadcasts repeat while a knob is being turned*, not idle streaming. Pro units are silent when idle (m-rk RESEARCH + this machine's probe transcript). Locate the exact wording (`grep -n "60" docs/pl81-pro-serial-protocol.md`) and correct it to: idle connections are silent; unprompted frames occur only while a physical control is being adjusted (repeating at ~60–80 ms on the non-Pro during adjustment).

2b. Append to `docs/pl81-pro-serial-protocol.md` (adjust the factual claims to match what Task 9's `e2e-results.md` ACTUALLY recorded — if a claim below was not observed, write what was observed instead; never record unobserved claims):

```markdown
## Daemon integration results (2026-08-08)

- Echo-as-ACK confirmed end-to-end from the Go daemon via go.bug.st/serial:
  every CCT frame written by `mutastic light on|off|...` came back
  byte-for-byte and was logged by the read loop (`light: frame ...` lines
  in `%LOCALAPPDATA%\mutastic\mutastic.log`).
- OFF-as-brightness-0 re-confirmed through the daemon (light off,
  echo `pwr=0x01 brightness=0`).
- Knob broadcasts were NOT exercised (no human at the desk during the
  automated run); their exact format remains uncaptured — see human
  follow-ups.

## Recorded human questions (follow-ups needing eyes/feet)

1. **Temperature-sweep calibration:** with the daemon running, sweep
   `mutastic light temp 2900` → `7000` in ~228 K steps at fixed brightness
   and watch where visible change stops, to confirm the 19-step clamp
   (byte 0x12) on this unit. (Pre-existing TODO, still open.)
2. **Real pedal press:** press the LEFT pedal (F13) and confirm the light
   toggles; confirm F14 (mute) and F15 (Winpepper) still behave.
3. **Knob broadcast + panel-off capture:** touch the physical knob while
   the daemon runs, then check the log for `light: frame` lines to finally
   capture a broadcast transcript (expected: CCT-shaped 8-byte frames).
   Also turn the panel off/on with its own physical control and check what
   the log records — this settles whether the pwr byte carries off-state
   (`0x00`/`0x02`), which the daemon already tolerates defensively.
4. **Unplug/replug:** with the daemon running, unplug the light's USB
   cable; confirm the log shows `light: session ended` within ~15 s (read
   error or the presence check), then replug and confirm `light: port
   opened` returns. This settles the CH340 surprise-removal behavior,
   which was validated only at source level.
5. **Long-idle re-sleep check:** after the daemon has been connected and
   idle for some hours, press F13 (or run `mutastic light on`) and confirm
   the light actually responds — settles whether wake-once-per-session
   suffices.
```

- [ ] **Step 3: Commit**

```bash
git add README.md docs/pl81-pro-serial-protocol.md
git commit -m "docs: light commands, F13 binding, protocol confirmations and human follow-ups"
```

---

## Recorded human questions (surface to the user at the end of the run)

These cannot be answered by software alone and are deliberately NOT plan
tasks (Task 10 records them in `docs/pl81-pro-serial-protocol.md`):

1. Optional temperature-sweep calibration (`temp` from 2900 to 7000 in steps)
   to confirm the 19-step clamp (byte 0x12) on this specific unit.
2. A real pedal-press test of F13 (and confirmation that F14/F15 behavior is
   unchanged).
3. Touching the light's physical knob while the daemon runs to capture a
   knob-broadcast transcript in the log — and turning the panel off/on with
   its own physical control, to settle whether the pwr byte carries
   off-state (the daemon already tolerates both conventions).
4. Unplugging/replugging the light's USB cable while the daemon runs:
   expect `light: session ended` within ~15 s and an automatic reopen
   (settles the CH340 surprise-removal behavior).
5. After some hours of idle, pressing F13 (or `mutastic light on`) and
   confirming the light responds (settles wake-once-per-session).
