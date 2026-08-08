# Yeti X Hardware-Mute Daemon (mutastic) Implementation Plan

> **For agentic workers:** This plan is executed task-by-task by the
> workflow's execute stage: a fresh implementer per task, with a spec +
> quality review after each task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** A single Go binary `mutastic.exe` (windows/amd64) whose `daemon` mode tracks and controls the Blue Yeti X hardware mute over vendor HID and serves UDP text commands, plus one-shot client subcommands, one new step in the existing F14 AutoHotkey handler, and a Windows deployment script.

**Architecture:** A resident daemon opens the Yeti X vendor HID control interface (usage == 1), performs the documented init handshake, reads input reports to track mute state (events 0x20/0x21, value at byte offset 9), and serves `toggle|mute|unmute|status` on UDP 127.0.0.1:42814. All protocol logic (report encode/decode, state tracking, command handling) is pure Go behind a tiny `Device` interface so it is unit-testable without hardware; only one windows-tagged file touches the cgo HID library. The AHK script and the deploy script invoke the same binary in client mode.

**Tech Stack:** Go (module `mutastic`, stdlib only except `github.com/sstallion/go-hid v0.15.0`), cgo cross-compile via mingw-w64, AutoHotkey v1 (existing script, one-line integration), Windows batch + PowerShell one-liners for deployment.

## Global Constraints

- Worktree/repo root (all paths below are relative to it): `/home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon`
- Target platform: windows/amd64. Build command (exact): `GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go build -ldflags "-s -w" -o bin/mutastic.exe .` (toolchain already installed: go1.26.3 at /usr/local/bin/go, x86_64-w64-mingw32-gcc 13)
- Dev environment is WSL2 Ubuntu. NEVER usbipd-attach the Yeti X to WSL (it would steal the system microphone). All live runs of the built .exe happen on the Windows side via WSL interop (`/mnt/c/...` or `cmd.exe /c`).
- HID library: `github.com/sstallion/go-hid v0.15.0` exactly. Only `hid_windows.go` may import it. Device selection: VID `0x046D`, PID one of `0x0AAF`, `0x0AD1`, `0x0AC6` (this machine: 0x0AAF), HID collection with `Usage == 1` (do NOT filter on UsagePage; log it). Verified live 2026-08-08 by a read-only probe (load-bearing validation): only PID 0x0AAF enumerates on this machine (4 collections, all on MI_03); `Usage == 1` matches exactly one collection (UsagePage 0xFFFF, `...MI_03&Col04...`), which opens non-elevated; the exact cross-compile command above builds go-hid v0.15.0 and the result runs on Windows.
- UDP endpoint (exact): `127.0.0.1:42814`, plain-text commands `toggle`, `mute`, `unmute`, `status`; replies are exactly `muted`, `unmuted`, `unknown`, or `error: <reason>`.
- Client exit codes: 0 = non-error reply, 1 = `error:` reply, 2 = no daemon reachable / bad usage.
- Daemon log file: `%LOCALAPPDATA%\mutastic\mutastic.log` on Windows — implemented as `os.UserCacheDir()` + `/mutastic/mutastic.log` (UserCacheDir IS %LOCALAPPDATA% on Windows).
- `ahk/MuteAllMeetings.ahk` is deployed and working: change ONLY the F14 handler (add one Run step). The file is UTF-8 **with BOM** and **CRLF** line endings — every edit must preserve both, verified via `git diff`.
- Deployment target dir: `C:\Users\dan\code\mutastic-deploy\`. Do NOT delete the old `C:\Users\dan\code\mute-unmute-meetings` folder (human decision later). `deploy/finish-setup.cmd.legacy` stays in the repo as historical reference (it is superseded, not deleted).
- README.md is the only end-user markdown doc to create/update (this plan under docs/plans/ is a working doc; docs/*.md already exist and the protocol doc gets a factual update in Task 8).
- Keep it simple: ~500-line utility, no frameworks, DRY, YAGNI, TDD, frequent commits.
- Physical actions (mic mute LED, pressing the mic button, unplug/replug, pressing the foot pedal) can only be verified by the human — surface them as questions at final review, never claim them.

## Scope note

This is one subsystem (daemon + one-line AHK integration + deploy script), so it is a single plan. System-level coverage comes from the UDP-level integration tests (Tasks 4–5) and the live Windows validation (Tasks 6, 7, 9, 11).

## File Structure

| Path | Responsibility |
|---|---|
| `go.mod`, `go.sum` | Module `mutastic`, go-hid dependency |
| `.gitignore` | Ignore `bin/` (build output) |
| `internal/proto/proto.go` | Pure report encode/decode: opcodes, `EncodeCommand`, `Event`, `DecodeEvent`, `MutedFromValue` |
| `internal/proto/proto_test.go` | Byte-exact tests for the above |
| `internal/daemon/tracker.go` | `Tracker`: mute-state tracking from events |
| `internal/daemon/tracker_test.go` | Tracker tests |
| `internal/daemon/daemon.go` | `Device` interface, `Daemon` (write path + command handling), `Run` (reconnect loop, handshake, read loop, UDP server) |
| `internal/daemon/daemon_test.go` | fake `Device`; command-handling unit tests; UDP-level integration tests (handshake, status, toggle, reconnect) |
| `main.go` | CLI dispatch, `runClient`, `runDaemon` (logging setup) |
| `main_test.go` | Client tests against a fake UDP server |
| `hid_windows.go` | `openYetiX` via go-hid (`//go:build windows`, cgo) + console hiding |
| `hid_other.go` | Non-windows stubs so `go test ./...` runs on WSL without cgo |
| `build.sh` | 3-line cross-compile script |
| `ahk/MuteAllMeetings.ahk` | MODIFY: F14 handler gains one `Run` step |
| `deploy/deploy.cmd` | CREATE: Windows deployment (replaces finish-setup.cmd.legacy) |
| `docs/yeti-x-hid-protocol.md` | MODIFY (Task 8 only): record empirically confirmed answers to the doc's open questions |
| `README.md` | MODIFY (Task 11): build, deploy, architecture, troubleshooting |

Interface stability rule: names and signatures defined in a task's **Produces** block are frozen — later tasks consume them verbatim.

---

### Task 1: Go module + protocol encode/decode (`internal/proto`)

**Files:**
- Create: `go.mod` (via `go mod init`)
- Create: `.gitignore`
- Create: `internal/proto/proto.go`
- Test: `internal/proto/proto_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces (frozen for Tasks 2–6):
  - Constants: `proto.OpGetVolume byte = 0x01`, `proto.OpInit byte = 0x05`, `proto.OpMute byte = 0x20`, `proto.EvtSoftwareMute byte = 0x20`, `proto.EvtDeviceMute byte = 0x21`
  - `func EncodeCommand(op byte, payload []byte) []byte` — returns the full 65-byte output report
  - `type Event struct { Op byte; Value byte }`
  - `func DecodeEvent(buf []byte) (Event, bool)` — false if not a vendor event report
  - `func MutedFromValue(v byte) (muted bool, ok bool)` — accepts binary and ASCII encodings

Protocol facts (from `docs/yeti-x-hid-protocol.md`, authoritative):

- **Outbound** (65-byte zero-filled buffer, sent as interrupt OUT via hid write, NOT feature report):
  `buf[0]=0x01` (report ID), `buf[1..3]=0x00`, `buf[4]=op`, `buf[5..7]=0x00`, `buf[8]=byte(8+len(payload))` (header-inclusive length), `buf[9:]=ASCII payload`, zero-padded to 65.
  Example — mute on: `01 00 00 00 20 00 00 00 09 31 00 … 00` (payload is ASCII `"1"`).
  Example — init: `01 00 00 00 05 00 00 00 08 00 … 00` (empty payload ⇒ length byte 0x08).
- **Inbound** (read buffer; report ID included at `[0]`): match prefix `buf[0]=0x01, buf[1]=0x80, buf[2]=0x00, buf[3]=0x00`; event code at `buf[4]`; value byte at `buf[9]` (RAW — may be binary `0x00/0x01` or ASCII `0x30/0x31`; the mute-event encoding is an open question resolved in Task 7, so `MutedFromValue` must accept both).
- Mute events: `0x20` SoftwareMute (host-initiated echo), `0x21` DeviceMute (physical button).

- [ ] **Step 1: Initialize the module and .gitignore**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
go mod init mutastic
printf 'bin/\n' > .gitignore
```

Expected: `go.mod` created with `module mutastic`.

- [ ] **Step 2: Write the failing tests**

Create `internal/proto/proto_test.go`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/proto/`
Expected: FAIL to build — `undefined: EncodeCommand`, `undefined: OpInit`, etc.

- [ ] **Step 4: Write the implementation**

Create `internal/proto/proto.go`:

```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/proto/ -v`
Expected: PASS (all 9 tests).

- [ ] **Step 6: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add go.mod .gitignore internal/proto/
git commit -m "feat: Yeti X HID report encode/decode (internal/proto)"
```

---

### Task 2: Mute-state tracker (`internal/daemon.Tracker`)

**Files:**
- Create: `internal/daemon/tracker.go`
- Test: `internal/daemon/tracker_test.go`

**Interfaces:**
- Consumes: `proto.Event`, `proto.EvtSoftwareMute`, `proto.EvtDeviceMute`, `proto.MutedFromValue` (Task 1).
- Produces (frozen for Tasks 3–4):
  - `type Tracker struct` (zero value usable, safe for concurrent use)
  - `func (t *Tracker) Apply(e proto.Event) (changed bool)` — updates state from mute events only; returns true iff the event was a decodable mute event
  - `func (t *Tracker) Set(muted bool)` — optimistic update after a successful outbound mute command
  - `func (t *Tracker) Status() (muted bool, known bool)` — known=false until the first Apply/Set

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/tracker_test.go`:

```go
package daemon

import (
	"testing"

	"mutastic/internal/proto"
)

func TestTrackerStartsUnknown(t *testing.T) {
	var tr Tracker
	if _, known := tr.Status(); known {
		t.Fatal("new Tracker should report known=false")
	}
}

func TestTrackerAppliesDeviceMuteBinary(t *testing.T) {
	var tr Tracker
	if !tr.Apply(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}) {
		t.Fatal("Apply should return true for a device mute event")
	}
	muted, known := tr.Status()
	if !known || !muted {
		t.Fatalf("Status() = %v, %v; want true, true", muted, known)
	}
}

func TestTrackerAppliesSoftwareUnmuteASCII(t *testing.T) {
	var tr Tracker
	tr.Apply(proto.Event{Op: proto.EvtSoftwareMute, Value: '1'})
	tr.Apply(proto.Event{Op: proto.EvtSoftwareMute, Value: '0'})
	muted, known := tr.Status()
	if !known || muted {
		t.Fatalf("Status() = %v, %v; want false, true", muted, known)
	}
}

func TestTrackerIgnoresNonMuteEvents(t *testing.T) {
	var tr Tracker
	if tr.Apply(proto.Event{Op: 0x23, Value: 0x32}) { // SoftwareVolume event
		t.Fatal("Apply should return false for non-mute events")
	}
	if _, known := tr.Status(); known {
		t.Fatal("non-mute events must not set known")
	}
}

func TestTrackerIgnoresUndecodableMuteValue(t *testing.T) {
	var tr Tracker
	if tr.Apply(proto.Event{Op: proto.EvtDeviceMute, Value: 0x42}) {
		t.Fatal("Apply should return false for an undecodable value byte")
	}
}

func TestTrackerSet(t *testing.T) {
	var tr Tracker
	tr.Set(true)
	muted, known := tr.Status()
	if !known || !muted {
		t.Fatalf("Status() after Set(true) = %v, %v; want true, true", muted, known)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/daemon/`
Expected: FAIL to build — `undefined: Tracker`.

- [ ] **Step 3: Write the implementation**

Create `internal/daemon/tracker.go`:

```go
// Package daemon implements the mutastic resident daemon: mute-state
// tracking, HID session management, and the UDP command server.
package daemon

import (
	"sync"

	"mutastic/internal/proto"
)

// Tracker holds the last known hardware mute state. The zero value is
// usable and reports known=false until the first Apply or Set.
type Tracker struct {
	mu    sync.Mutex
	known bool
	muted bool
}

// Apply updates the state from a mute event (0x20 SoftwareMute or
// 0x21 DeviceMute). It returns true iff the event was a mute event with a
// decodable value byte.
func (t *Tracker) Apply(e proto.Event) bool {
	if e.Op != proto.EvtSoftwareMute && e.Op != proto.EvtDeviceMute {
		return false
	}
	muted, ok := proto.MutedFromValue(e.Value)
	if !ok {
		return false
	}
	t.Set(muted)
	return true
}

// Set records a known mute state (used optimistically after a successful
// outbound mute command; the device echo then confirms it).
func (t *Tracker) Set(muted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.known = true
	t.muted = muted
}

// Status returns the current state; known is false if no mute event or Set
// has been seen yet.
func (t *Tracker) Status() (muted bool, known bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.muted, t.known
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/daemon/ -v`
Expected: PASS (all 6 tests).

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add internal/daemon/
git commit -m "feat: mute-state tracker"
```

---

### Task 3: Daemon command handling + device write path

**Files:**
- Create: `internal/daemon/daemon.go`
- Test: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: `Tracker` (Task 2), `proto.EncodeCommand`, `proto.OpMute` (Task 1).
- Produces (frozen for Tasks 4–5):
  - `type Device interface { Write(p []byte) (int, error); ReadWithTimeout(p []byte, timeout time.Duration) (int, error); Close() error }`
    (contract: ReadWithTimeout returns (0, nil) on timeout-with-no-data; a non-nil error means the device is gone)
  - `type Daemon struct { Track Tracker; Logger *log.Logger; … }`
  - `func New(logger *log.Logger) *Daemon`
  - `func (d *Daemon) SetDevice(dev Device)` — nil clears
  - `func (d *Daemon) WriteReport(report []byte) error` — serialized; error `no device` when disconnected
  - `func (d *Daemon) HandleCommand(cmd string) string` — replies exactly `muted`, `unmuted`, `unknown`, or `error: <reason>`
- Command semantics (frozen): `status` → `muted`/`unmuted`/`unknown`; `mute` sends op 0x20 payload `"1"`; `unmute` sends payload `"0"`; `toggle` inverts tracked state (defaults to MUTE when state unknown — safe default for a pedal press); after a successful send the tracker is optimistically `Set`; anything else → `error: unknown command`.

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/daemon_test.go`:

```go
package daemon

import (
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// fakeDevice implements Device for tests. Reads block on the events channel
// (10ms poll timeout); Writes are recorded.
type fakeDevice struct {
	mu      sync.Mutex
	writes  [][]byte
	events  chan []byte
	readErr chan error
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{events: make(chan []byte, 8), readErr: make(chan error, 1)}
}

func (f *fakeDevice) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(p))
	copy(c, p)
	f.writes = append(f.writes, c)
	return len(p), nil
}

func (f *fakeDevice) ReadWithTimeout(p []byte, _ time.Duration) (int, error) {
	select {
	case ev := <-f.events:
		return copy(p, ev), nil
	case err := <-f.readErr:
		return 0, err
	case <-time.After(10 * time.Millisecond):
		return 0, nil // timeout, no data
	}
}

func (f *fakeDevice) Close() error { return nil }

func (f *fakeDevice) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeDevice) write(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[i]
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestHandleCommandNoDevice(t *testing.T) {
	d := New(testLogger())
	if got := d.HandleCommand("toggle"); got != "error: no device" {
		t.Fatalf("toggle with no device = %q, want %q", got, "error: no device")
	}
	if got := d.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status with no state = %q, want %q", got, "unknown")
	}
}

func TestHandleCommandMuteUnmute(t *testing.T) {
	d := New(testLogger())
	dev := newFakeDevice()
	d.SetDevice(dev)

	if got := d.HandleCommand("mute"); got != "muted" {
		t.Fatalf("mute = %q, want %q", got, "muted")
	}
	w := dev.write(0)
	if w[4] != 0x20 || w[8] != 0x09 || w[9] != '1' {
		t.Fatalf("mute wrote % x; want op 0x20 len 0x09 payload '1'", w[:12])
	}
	if got := d.HandleCommand("status"); got != "muted" {
		t.Fatalf("status after mute = %q, want %q", got, "muted")
	}

	if got := d.HandleCommand("unmute"); got != "unmuted" {
		t.Fatalf("unmute = %q, want %q", got, "unmuted")
	}
	w = dev.write(1)
	if w[4] != 0x20 || w[9] != '0' {
		t.Fatalf("unmute wrote % x; want op 0x20 payload '0'", w[:12])
	}
}

func TestHandleCommandToggle(t *testing.T) {
	d := New(testLogger())
	dev := newFakeDevice()
	d.SetDevice(dev)

	// Unknown state: toggle defaults to mute.
	if got := d.HandleCommand("toggle"); got != "muted" {
		t.Fatalf("first toggle = %q, want %q", got, "muted")
	}
	if w := dev.write(0); w[9] != '1' {
		t.Fatalf("first toggle payload = %q, want '1'", w[9])
	}
	// Now known muted: toggle unmutes.
	if got := d.HandleCommand("toggle"); got != "unmuted" {
		t.Fatalf("second toggle = %q, want %q", got, "unmuted")
	}
	if w := dev.write(1); w[9] != '0' {
		t.Fatalf("second toggle payload = %q, want '0'", w[9])
	}
}

func TestHandleCommandUnknown(t *testing.T) {
	d := New(testLogger())
	if got := d.HandleCommand("frobnicate"); got != "error: unknown command" {
		t.Fatalf("unknown command reply = %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/daemon/`
Expected: FAIL to build — `undefined: Device`, `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `internal/daemon/daemon.go`:

```go
package daemon

import (
	"errors"
	"log"
	"sync"
	"time"

	"mutastic/internal/proto"
)

// Device is the minimal HID handle the daemon needs. Implementations must
// return (0, nil) from ReadWithTimeout when the timeout elapses with no
// data; any non-nil error is treated as "device gone" and triggers a
// reconnect.
type Device interface {
	Write(p []byte) (int, error)
	ReadWithTimeout(p []byte, timeout time.Duration) (int, error)
	Close() error
}

var errNoDevice = errors.New("no device")

// Daemon holds shared daemon state: the tracked mute state, the current
// device handle, and the logger.
type Daemon struct {
	Track  Tracker
	Logger *log.Logger

	mu  sync.Mutex
	dev Device
}

// New returns a Daemon that logs to logger.
func New(logger *log.Logger) *Daemon {
	return &Daemon{Logger: logger}
}

// SetDevice installs the current device handle (nil while disconnected).
func (d *Daemon) SetDevice(dev Device) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dev = dev
}

// WriteReport sends one output report. Writes are serialized by the mutex;
// per the protocol doc, the returned byte count is NOT asserted on (Windows
// hidapi reports 64 for a 65-byte buffer).
func (d *Daemon) WriteReport(report []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dev == nil {
		return errNoDevice
	}
	_, err := d.dev.Write(report)
	return err
}

// HandleCommand executes one UDP text command and returns the reply.
// Replies are exactly: "muted", "unmuted", "unknown", or "error: <reason>".
func (d *Daemon) HandleCommand(cmd string) string {
	switch cmd {
	case "status":
		muted, known := d.Track.Status()
		if !known {
			return "unknown"
		}
		if muted {
			return "muted"
		}
		return "unmuted"
	case "mute":
		return d.setMute(true)
	case "unmute":
		return d.setMute(false)
	case "toggle":
		muted, known := d.Track.Status()
		target := true // unknown state: default to mute (safe for a pedal press)
		if known {
			target = !muted
		}
		return d.setMute(target)
	default:
		return "error: unknown command"
	}
}

func (d *Daemon) setMute(muted bool) string {
	payload := []byte("0")
	if muted {
		payload = []byte("1")
	}
	if err := d.WriteReport(proto.EncodeCommand(proto.OpMute, payload)); err != nil {
		return "error: " + err.Error()
	}
	d.Track.Set(muted) // optimistic; the 0x20 echo confirms it
	if muted {
		return "muted"
	}
	return "unmuted"
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/daemon/ -v`
Expected: PASS (Tracker tests + the 4 new tests).

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add internal/daemon/
git commit -m "feat: daemon command handling and device write path"
```

---

### Task 4: Daemon run loop — handshake, event reading, reconnect, UDP server

**Files:**
- Modify: `internal/daemon/daemon.go` (append `OpenFunc`, `Run`, `session`, `serveUDP`, `sleepCtx`)
- Test: `internal/daemon/daemon_test.go` (append integration tests)

**Interfaces:**
- Consumes: everything from Tasks 1–3.
- Produces (frozen for Task 5):
  - `type OpenFunc func() (Device, error)` — opens the Yeti X control interface
  - `func Run(ctx context.Context, open OpenFunc, pc net.PacketConn, logger *log.Logger) error` — blocks until ctx is cancelled; caller owns creating the UDP listener (binding 127.0.0.1:42814 in production doubles as a single-instance lock: a second daemon fails to bind and exits)
- Behavior (frozen): on each successful open, send handshake `EncodeCommand(OpInit, nil)` then `EncodeCommand(OpGetVolume, nil)` (no delay between them); then loop `ReadWithTimeout(buf, 1s)`; every decoded event is logged with op and value in hex (Task 7 depends on these log lines); read error ⇒ close, clear device, retry after 2s; open failure ⇒ retry after 3s.
- Handshake liveness gate (added by load-bearing validation — assumption A4 was FALSIFIED): the protocol doc calls the handshake “somewhat flaky” and the reference implementation retries the whole open+handshake after 5s of post-handshake silence — a write-succeeded handshake does NOT imply a live event stream, and a silent session would leave the daemon permanently deaf (no events, no error, no reconnect). Therefore: the GetVolume request must provoke at least one input report; if NO input report has arrived within 5s of the handshake (package-level `var handshakeLiveness = 5 * time.Second`, shortened in tests), `session` returns an error and the session is retried like any device loss (close, clear device, 2s, reopen + re-handshake). After the first input report of a session, silence is normal idle and never triggers the gate.

- [ ] **Step 1: Write the failing tests**

Append to `internal/daemon/daemon_test.go`:

```go
// --- integration tests over real UDP with a fake device ---

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

func inputReport(op, value byte) []byte {
	b := make([]byte, 64)
	b[0] = 0x01
	b[1] = 0x80
	b[4] = op
	b[9] = value
	return b
}

// startDaemon runs Run() with the given OpenFunc on an ephemeral UDP port
// and returns the UDP address plus a UDP request helper.
func startDaemon(t *testing.T, open OpenFunc) (addr string, ask func(cmd string) string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, open, pc, testLogger())
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done // ensure Run has exited before later cleanups (e.g. restoring handshakeLiveness)
	})

	addr = pc.LocalAddr().String()
	ask = func(cmd string) string {
		conn, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(cmd)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("no reply to %q: %v", cmd, err)
		}
		return string(buf[:n])
	}
	return addr, ask
}

func TestRunSendsHandshake(t *testing.T) {
	dev := newFakeDevice()
	_, _ = startDaemon(t, func() (Device, error) { return dev, nil })

	waitFor(t, "handshake writes", func() bool { return dev.writeCount() >= 2 })
	if w := dev.write(0); w[4] != 0x05 || w[8] != 0x08 {
		t.Fatalf("first handshake write = % x; want op 0x05", w[:10])
	}
	if w := dev.write(1); w[4] != 0x01 || w[8] != 0x08 {
		t.Fatalf("second handshake write = % x; want op 0x01", w[:10])
	}
}

func TestDeviceEventsDriveStatusOverUDP(t *testing.T) {
	dev := newFakeDevice()
	_, ask := startDaemon(t, func() (Device, error) { return dev, nil })
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	// Physical button press (binary value).
	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "muted status", func() bool { return ask("status") == "muted" })

	// Software echo with ASCII value.
	dev.events <- inputReport(0x20, '0')
	waitFor(t, "unmuted status", func() bool { return ask("status") == "unmuted" })
}

func TestToggleOverUDP(t *testing.T) {
	dev := newFakeDevice()
	_, ask := startDaemon(t, func() (Device, error) { return dev, nil })
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	if got := ask("toggle"); got != "muted" {
		t.Fatalf("toggle = %q, want muted", got)
	}
	if got := ask("toggle"); got != "unmuted" {
		t.Fatalf("second toggle = %q, want unmuted", got)
	}
	// Two mute commands were written after the 2 handshake reports.
	waitFor(t, "mute writes", func() bool { return dev.writeCount() >= 4 })
	if w := dev.write(2); w[4] != 0x20 || w[9] != '1' {
		t.Fatalf("first toggle wrote % x", w[:12])
	}
	if w := dev.write(3); w[4] != 0x20 || w[9] != '0' {
		t.Fatalf("second toggle wrote % x", w[:12])
	}
}

func TestReconnectAfterReadError(t *testing.T) {
	dev1 := newFakeDevice()
	dev2 := newFakeDevice()
	var opens atomic.Int32
	open := func() (Device, error) {
		if opens.Add(1) == 1 {
			return dev1, nil
		}
		return dev2, nil
	}
	_, ask := startDaemon(t, open)
	waitFor(t, "first handshake", func() bool { return dev1.writeCount() >= 2 })

	dev1.readErr <- errors.New("device unplugged")
	// Reconnect delay is 2s; allow up to 5s for the second handshake.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && dev2.writeCount() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if dev2.writeCount() < 2 {
		t.Fatal("daemon did not reconnect and re-handshake after a read error")
	}
	dev2.events <- inputReport(0x21, 0x01)
	waitFor(t, "status after reconnect", func() bool { return ask("status") == "muted" })
}

func TestOpenFailureRetriesWithoutCrashing(t *testing.T) {
	open := func() (Device, error) { return nil, errors.New("not found") }
	_, ask := startDaemon(t, open)
	if got := ask("status"); got != "unknown" {
		t.Fatalf("status with no device = %q, want unknown", got)
	}
	if got := ask("mute"); got != "error: no device" {
		t.Fatalf("mute with no device = %q, want error: no device", got)
	}
}

func TestRehandshakeAfterSilentHandshake(t *testing.T) {
	old := handshakeLiveness
	handshakeLiveness = 200 * time.Millisecond
	t.Cleanup(func() { handshakeLiveness = old })

	dev1 := newFakeDevice() // never emits an input report: a silent (flaky) handshake
	dev2 := newFakeDevice()
	var opens atomic.Int32
	open := func() (Device, error) {
		if opens.Add(1) == 1 {
			return dev1, nil
		}
		return dev2, nil
	}
	_, ask := startDaemon(t, open)

	// The daemon must abandon the silent dev1 (liveness gate) and re-handshake
	// on dev2. Reconnect delay is 2s; allow up to 5s.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && dev2.writeCount() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if dev2.writeCount() < 2 {
		t.Fatal("daemon did not re-handshake after a silent (flaky) handshake")
	}
	// An input report on dev2 clears the gate and drives status normally.
	dev2.events <- inputReport(0x21, 0x01)
	waitFor(t, "status after re-handshake", func() bool { return ask("status") == "muted" })
}
```

Update the import block at the top of `internal/daemon/daemon_test.go` to:

```go
import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)
```

(Note: an `inputReport` helper also exists in `internal/proto/proto_test.go`, but that is a different package — the 8-line duplication across test packages is deliberate.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test ./internal/daemon/`
Expected: FAIL to build — `undefined: Run`, `undefined: OpenFunc`.

- [ ] **Step 3: Write the implementation**

Append to `internal/daemon/daemon.go` (and add `"context"`, `"net"`, `"strings"` to its imports):

```go
// OpenFunc opens the Yeti X HID control interface.
type OpenFunc func() (Device, error)

// Run serves UDP commands on pc and maintains the device session until ctx
// is cancelled. The caller owns pc; binding the production port
// (127.0.0.1:42814) doubles as a single-instance lock.
func Run(ctx context.Context, open OpenFunc, pc net.PacketConn, logger *log.Logger) error {
	d := New(logger)
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	go d.serveUDP(pc)

	for ctx.Err() == nil {
		dev, err := open()
		if err != nil {
			logger.Printf("open device: %v (retrying in 3s)", err)
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		logger.Printf("device opened")
		d.SetDevice(dev)
		err = d.session(ctx, dev)
		d.SetDevice(nil)
		dev.Close()
		if ctx.Err() != nil {
			break
		}
		logger.Printf("device session ended: %v (reconnecting in 2s)", err)
		sleepCtx(ctx, 2*time.Second)
	}
	return nil
}

// handshakeLiveness bounds how long a fresh session may stay silent after the
// handshake. The protocol doc calls the handshake "somewhat flaky": writes can
// succeed yet the event stream never comes up, which would leave the daemon
// deaf with no error to trigger a reconnect. The GetVolume request sent during
// the handshake provokes a reply, so a live session always produces at least
// one input report quickly. Var (not const) so tests can shorten it.
var handshakeLiveness = 5 * time.Second

var errHandshakeSilence = errors.New("no input report within the handshake liveness window (flaky handshake); reinitializing")

// session performs the init handshake then reads input reports until the
// device errors (unplug), the handshake proves dead (liveness gate), or ctx
// is cancelled.
func (d *Daemon) session(ctx context.Context, dev Device) error {
	if err := d.WriteReport(proto.EncodeCommand(proto.OpInit, nil)); err != nil {
		return err
	}
	if err := d.WriteReport(proto.EncodeCommand(proto.OpGetVolume, nil)); err != nil {
		return err
	}
	deadline := time.Now().Add(handshakeLiveness)
	live := false // becomes true on the first input report of this session
	buf := make([]byte, 128)
	for ctx.Err() == nil {
		n, err := dev.ReadWithTimeout(buf, time.Second)
		if err != nil {
			return err
		}
		if n == 0 {
			if !live && time.Now().After(deadline) {
				return errHandshakeSilence
			}
			continue // timeout, no data
		}
		live = true
		ev, ok := proto.DecodeEvent(buf[:n])
		if !ok {
			continue
		}
		if d.Track.Apply(ev) {
			muted, _ := d.Track.Status()
			d.Logger.Printf("event op=0x%02x value=0x%02x -> muted=%v", ev.Op, ev.Value, muted)
		} else {
			d.Logger.Printf("event op=0x%02x value=0x%02x (ignored)", ev.Op, ev.Value)
		}
	}
	return ctx.Err()
}

func (d *Daemon) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 64)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return // listener closed on shutdown
		}
		cmd := strings.TrimSpace(string(buf[:n]))
		reply := d.HandleCommand(cmd)
		d.Logger.Printf("command %q -> %q", cmd, reply)
		if _, err := pc.WriteTo([]byte(reply), addr); err != nil {
			return
		}
	}
}

func sleepCtx(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}
```

- [ ] **Step 4: Run all tests, including the race detector**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test -race ./... && go vet ./...`
Expected: PASS for `internal/proto` and `internal/daemon`; no vet findings. (`TestReconnectAfterReadError` and `TestRehandshakeAfterSilentHandshake` legitimately take ~2–3s each.)

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add internal/daemon/
git commit -m "feat: daemon run loop with handshake, reconnect, and UDP server"
```

---

### Task 5: CLI — subcommand dispatch, one-shot client, daemon bootstrap, platform wiring

**Files:**
- Create: `main.go`
- Create: `hid_windows.go`
- Create: `hid_other.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `daemon.Run`, `daemon.OpenFunc`, `daemon.Device` (Task 4).
- Produces (frozen for Tasks 6–11):
  - Binary usage: `mutastic daemon` | `mutastic toggle|mute|unmute|status`; unknown/missing subcommand prints usage to stderr, exit 2
  - `func runClient(cmd, addr string, timeout time.Duration, out io.Writer) int` — prints the daemon reply (or `error: no daemon reachable`) to out; returns exit code 0/1/2 per Global Constraints
  - `const udpAddr = "127.0.0.1:42814"`
  - `func openYetiX(logger *log.Logger) (daemon.Device, error)` — windows: go-hid enumeration (VID 0x046D; PIDs 0x0AAF, 0x0AD1, 0x0AC6; pick `Usage == 1`, log every collection's Usage/UsagePage/Path through the passed daemon logger so the lines reach `mutastic.log`); non-windows: returns an error
  - `func hideConsoleIfOwned()` — windows: hides the console window only when this process is its sole owner (i.e., launched from a shortcut, not from a terminal); non-windows: no-op
  - Daemon logging: file at `os.UserCacheDir()/mutastic/mutastic.log` (= `%LOCALAPPDATA%\mutastic\mutastic.log` on Windows), rotated to `.old` above 5 MB at startup, tee'd to stderr

- [ ] **Step 1: Write the failing client tests**

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRunClientRoundTrip(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) == "status" {
			pc.WriteTo([]byte("muted"), addr)
		}
	}()

	var out bytes.Buffer
	code := runClient("status", pc.LocalAddr().String(), 2*time.Second, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != "muted" {
		t.Fatalf("output = %q, want muted", got)
	}
}

func TestRunClientErrorReply(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		_, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		pc.WriteTo([]byte("error: no device"), addr)
	}()

	var out bytes.Buffer
	if code := runClient("mute", pc.LocalAddr().String(), 2*time.Second, &out); code != 1 {
		t.Fatalf("exit code = %d, want 1 for an error reply", code)
	}
}

func TestRunClientNoDaemon(t *testing.T) {
	var out bytes.Buffer
	// Nothing listens on this port; expect timeout/refusal -> exit 2.
	code := runClient("status", "127.0.0.1:59999", 300*time.Millisecond, &out)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 when no daemon is reachable", code)
	}
	if !strings.Contains(out.String(), "no daemon reachable") {
		t.Fatalf("output = %q, want it to mention 'no daemon reachable'", out.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && go test .`
Expected: FAIL — `undefined: runClient` (if `go test .` instead complains there are no Go files in the root package, that IS the failure signal here).

- [ ] **Step 3: Write main.go**

Create `main.go`:

```go
// mutastic controls the Blue Yeti X hardware mute.
//
//	mutastic daemon                     resident: HID session + UDP server
//	mutastic toggle|mute|unmute|status  one-shot client for the daemon
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mutastic/internal/daemon"
)

const udpAddr = "127.0.0.1:42814"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(runDaemon())
	case "toggle", "mute", "unmute", "status":
		os.Exit(runClient(os.Args[1], udpAddr, time.Second, os.Stdout))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mutastic daemon | toggle | mute | unmute | status")
}

// runClient sends one UDP command to the daemon and prints the reply.
// Exit codes: 0 = ok, 1 = "error:" reply from the daemon, 2 = no daemon.
func runClient(cmd, addr string, timeout time.Duration, out io.Writer) int {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		fmt.Fprintln(out, "error: no daemon reachable:", err)
		return 2
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(cmd)); err != nil {
		fmt.Fprintln(out, "error: no daemon reachable:", err)
		return 2
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		fmt.Fprintln(out, "error: no daemon reachable")
		return 2
	}
	reply := strings.TrimSpace(string(buf[:n]))
	fmt.Fprintln(out, reply)
	if strings.HasPrefix(reply, "error:") {
		return 1
	}
	return 0
}

func runDaemon() int {
	hideConsoleIfOwned()

	logw, logPath, err := openLogFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutastic: cannot open log file:", err)
		logw = nopWriteCloser{}
	}
	defer logw.Close()
	logger := log.New(io.MultiWriter(os.Stderr, logw), "", log.LstdFlags)
	logger.Printf("mutastic daemon starting (log: %s)", logPath)

	pc, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		// Port already bound: another daemon instance is running.
		logger.Printf("cannot bind %s (daemon already running?): %v", udpAddr, err)
		return 1
	}
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	daemon.Run(context.Background(), open, pc, logger)
	return 0
}

// openLogFile opens %LOCALAPPDATA%\mutastic\mutastic.log (os.UserCacheDir
// is %LOCALAPPDATA% on Windows), rotating to .old above 5 MB.
func openLogFile() (io.WriteCloser, string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, "", err
	}
	logDir := filepath.Join(dir, "mutastic")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(logDir, "mutastic.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
```

- [ ] **Step 4: Write the windows HID wiring and the non-windows stubs**

Create `hid_windows.go`:

```go
//go:build windows

package main

import (
	"errors"
	"log"
	"syscall"
	"time"
	"unsafe"

	hid "github.com/sstallion/go-hid"

	"mutastic/internal/daemon"
)

const yetiVID = 0x046D

var yetiPIDs = []uint16{0x0AAF, 0x0AD1, 0x0AC6}

var hidReady = false

// openYetiX finds the Yeti X vendor control interface: the HID collection
// with Usage == 1 (per docs/yeti-x-hid-protocol.md; UsagePage is logged but
// deliberately NOT filtered on, mirroring the reference implementation).
// logger is the daemon's MultiWriter logger, so the enumeration lines land
// in mutastic.log as well as on stderr (Task 8 consumes them from the file).
func openYetiX(logger *log.Logger) (daemon.Device, error) {
	if !hidReady {
		if err := hid.Init(); err != nil {
			return nil, err
		}
		hidReady = true
	}
	var path string
	for _, pid := range yetiPIDs {
		_ = hid.Enumerate(yetiVID, pid, func(info *hid.DeviceInfo) error {
			logger.Printf("hid collection: pid=0x%04x usage_page=0x%04x usage=0x%04x path=%s",
				pid, info.UsagePage, info.Usage, info.Path)
			if path == "" && info.Usage == 1 {
				path = info.Path
			}
			return nil
		})
		if path != "" {
			break
		}
	}
	if path == "" {
		return nil, errors.New("Yeti X control interface (usage==1) not found")
	}
	dev, err := hid.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return wrappedDevice{dev}, nil
}

// wrappedDevice normalizes go-hid behavior to the daemon.Device contract:
// a read timeout must surface as (0, nil), not an error.
type wrappedDevice struct {
	d *hid.Device
}

func (w wrappedDevice) Write(p []byte) (int, error) { return w.d.Write(p) }

func (w wrappedDevice) ReadWithTimeout(p []byte, t time.Duration) (int, error) {
	n, err := w.d.ReadWithTimeout(p, t)
	if err != nil && errors.Is(err, hid.ErrTimeout) {
		return 0, nil
	}
	return n, err
}

func (w wrappedDevice) Close() error { return w.d.Close() }

// hideConsoleIfOwned hides this process's console window, but only when the
// process is the console's sole owner (launched via shortcut / AHK Run —
// not from an interactive terminal, whose window we must not hide).
func hideConsoleIfOwned() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	hwnd, _, _ := k32.NewProc("GetConsoleWindow").Call()
	if hwnd == 0 {
		return
	}
	pids := make([]uint32, 4)
	n, _, _ := k32.NewProc("GetConsoleProcessList").Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		const swHide = 0
		syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow").Call(hwnd, swHide)
	}
}
```

(Verified 2026-08-08 against the go-hid v0.15.0 source (hid.go:63-64, 223, 232-233): `hid.ErrTimeout` EXISTS and `ReadWithTimeout` returns `(0, hid.ErrTimeout)` — not `(0, nil)` — on a timeout with no data. The `errors.Is` guard above is therefore REQUIRED to satisfy the Device contract; do NOT remove it. Also verified: hidapi loads hid.dll/cfgmgr32.dll at runtime, so the build needs no extra linker flags.)

Create `hid_other.go`:

```go
//go:build !windows

package main

import (
	"errors"
	"log"

	"mutastic/internal/daemon"
)

func openYetiX(_ *log.Logger) (daemon.Device, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

func hideConsoleIfOwned() {}
```

- [ ] **Step 5: Add the dependency and run everything on linux**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
go get github.com/sstallion/go-hid@v0.15.0
go mod tidy
go build ./...
go test -race ./...
go vet ./...
```

Expected: dependency resolves; build + all tests PASS on linux (the go-hid import lives only behind `//go:build windows`, so no cgo is needed here). Verify the dependency survived tidy: `grep sstallion go.mod` shows the require line (it will — `go mod tidy` considers all GOOS builds).

- [ ] **Step 6: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add main.go main_test.go hid_windows.go hid_other.go go.mod go.sum
git commit -m "feat: mutastic CLI with client mode and windows HID wiring"
```

---

### Task 6: Cross-compile for Windows and smoke-run via interop

**Files:**
- Create: `build.sh`
- Output (gitignored): `bin/mutastic.exe`

**Interfaces:**
- Consumes: full source tree (Tasks 1–5).
- Produces: `bin/mutastic.exe` (windows/amd64) and `./build.sh` producing it; Tasks 7, 9, 10, 11 run this exe on Windows.

- [ ] **Step 1: Verify the toolchain (already installed; commands are the contingency)**

Run: `go version && x86_64-w64-mingw32-gcc --version | head -1`
Expected: `go1.26.x` and `x86_64-w64-mingw32-gcc (GCC) 13-win32`.
Contingency if missing: install the apt packages `gcc-mingw-w64-x86-64` and `golang-go` (root privileges are available on this machine for apt). If cross-compiling proves intractable (it should not — go-hid bundles the hidapi C sources, and mingw resolves the hid/setupapi Windows system libs automatically), the fallback is building on Windows via interop with a Windows Go toolchain if one exists (`cmd.exe /c go version` to check); if neither works, STOP and surface the blocker.

- [ ] **Step 2: Write build.sh**

Create `build.sh`:

```bash
#!/usr/bin/env bash
# Cross-compile mutastic.exe for Windows from WSL.
set -euo pipefail
cd "$(dirname "$0")"
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc \
  go build -ldflags "-s -w" -o bin/mutastic.exe .
echo "built bin/mutastic.exe"
```

Then: `chmod +x build.sh`

- [ ] **Step 3: Build**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && ./build.sh` (allow up to ~5 minutes on first run — cgo compiles hidapi).
Expected: `built bin/mutastic.exe`. Verify: `file bin/mutastic.exe` reports `PE32+ executable ... x86-64, for MS Windows`.

- [ ] **Step 4: Smoke-run the exe on Windows via interop**

```bash
mkdir -p /mnt/c/Users/dan/code/mutastic-deploy
cp /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon/bin/mutastic.exe /mnt/c/Users/dan/code/mutastic-deploy/
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status; echo "exit=$?"
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe bogus; echo "exit=$?"
```

Expected: first command prints `error: no daemon reachable` and `exit=2` (no daemon running yet — this PROVES the exe executes natively on Windows); second prints the usage line and `exit=2`.

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add build.sh
git commit -m "build: windows/amd64 cross-compile script"
```

---

### Task 7: Live daemon validation against the real Yeti X

**Files:**
- None modified (behavioral validation; code changes only if reality disagrees with the doc — see contingency).

**Interfaces:**
- Consumes: `bin/mutastic.exe` deployed to `C:\Users\dan\code\mutastic-deploy\mutastic.exe` (Task 6 Step 4).
- Produces: a validation record (log excerpts) that Task 8 turns into protocol-doc updates. The daemon log lines of interest look like `event op=0x20 value=0x01 -> muted=true`.

**Safety reminder:** never usbipd-attach the mic to WSL. Everything below runs the Windows exe via interop.

- [ ] **Step 1: Start the daemon on Windows (background)**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe daemon
```

Run with the bash tool's `run_in_background: true` (stderr streams the log). Give it ~3s, then confirm from the log output (or `tail -20 "/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log"`):
- `hid collection: ...` lines enumerate the Yeti X collections — record each `usage_page` value (open question in the protocol doc);
- `device opened` — proves VID/PID + usage==1 selection worked.
- Pre-verified 2026-08-08 by a read-only enumeration probe (expect the same lines here): PID 0x0AAF exposes 4 collections on MI_03 — usage_page/usage `0xFF43/0x0701`, `0xFF43/0x0702`, `0xFF43/0x0704`, `0xFFFF/0x0001`; `usage==1` matches exactly the `0xFFFF` Col04 collection.
- If the log shows repeated `no input report within the handshake liveness window` retries: that is Task 4's liveness gate working around the doc's known handshake flakiness — the daemon should come live within a few cycles. If it NEVER comes live after ~10 retries, jump to Step 2 contingency (c) (wrong collection), NOT to the payload-encoding contingency.

If instead `open device: Yeti X control interface (usage==1) not found` repeats: the mic is not attached to Windows, or another PID from the list applies, or G HUB claims it exclusively — STOP and surface to the human; do not guess.

- [ ] **Step 2: Exercise mute and unmute across TWO full cycles; capture echo encoding, new-vs-old semantics, and polarity**

```bash
for cycle in 1 2; do
  echo "=== cycle $cycle ==="
  /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe mute; echo "exit=$?"
  sleep 1
  /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status
  /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe unmute; echo "exit=$?"
  sleep 1
  /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status
done
tail -60 "/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log"
```

Expected: in BOTH cycles `mute` prints `muted` (exit 0) with status `muted`, and `unmute` prints `unmuted` with status `unmuted`. In the log, each command should be followed by a SoftwareMute echo line `event op=0x20 value=0xVV -> muted=...`. Record the exact `value=0xVV` for every one of the four commands and resolve THREE questions empirically (none may be assumed):

1. **Encoding** — `0x01/0x00` means the inbound value byte is binary; `0x31/0x30` means ASCII (protocol doc open questions 1–2).
2. **NEW vs OLD state** — the doc's Pattern events (`0x08`/`0x12`) demonstrably carry old/new pairs, so echoes carrying the PREVIOUS state are a real possibility. Across two full cycles the sequences differ unambiguously: new-state echoes decode muted, unmuted, muted, unmuted; old-state echoes lag one step. Contingency if echoes consistently carry the OLD state: change event handling so `0x20` SoftwareMute events are ignored for state tracking (the optimistic `Set` already covers host commands; `0x21` DeviceMute stays authoritative) — in `Tracker.Apply` accept only `proto.EvtDeviceMute`, update the affected tests (`TestTrackerAppliesSoftwareUnmuteASCII` becomes an ignored-event test; the `0x20` step in `TestDeviceEventsDriveStatusOverUDP` flips expectation), rebuild, redeploy, re-run this step, and commit as `fix: 0x20 echo carries previous state; ignore for tracking`. Record the finding for Task 8.
3. **Polarity** — a related device (Logitech Yeti GX) has an INVERTED set_mute. The echo after `mute` must decode to muted=true. Contingency if behavior is consistent but inverted: swap the payloads in `setMute` (`"1"`↔`"0"`), flip the corresponding payload assertions in `internal/daemon/daemon_test.go`, rebuild, redeploy, repeat this step, and commit as `fix: mute payload polarity inverted`. (The physically observable state — LED / actual audio mute — remains human question 1/2.)

Contingency if NO echo events appear or the hardware state does not actually change — several distinct failures share this exact symptom; diagnose in THIS order:

- **(a) Handshake liveness:** check the log for liveness-gate retries (Task 4). A daemon that never comes live has a handshake/collection problem, not an encoding problem — go to (c).
- **(b) Payload encoding:** change `setMute` in `internal/daemon/daemon.go` to use binary payloads `[]byte{0x01}` / `[]byte{0x00}` (length byte stays 0x09 automatically), update the two payload-byte assertions in `internal/daemon/daemon_test.go` and the two in `internal/proto/proto_test.go` (`0x31`→`0x01`, `0x30`→`0x00`), rebuild (`./build.sh`), redeploy (`taskkill.exe /F /IM mutastic.exe`, re-copy the exe, restart the daemon), and repeat this step. Whichever encoding produces a state change is the confirmed one; if it was binary, commit as `fix: mute payload is binary, not ASCII`.
- **(c) Wrong collection:** the mute protocol may live on one of the sibling `0xFF43` collections (usages 0x0701/0x0702/0x0704) instead of `0xFFFF/0x0001` — the usage==1 rule comes from the reference implementation, whose event handling was only battle-tested for volume. As a diagnostic experiment, temporarily change the selection in `openYetiX` to `info.UsagePage == 0xFF43 && info.Usage == 0x0701` (then 0x0702, then 0x0704), rebuild, redeploy, and retry mute/unmute (both encodings) on each. If one works, make that the permanent selection rule, update its comment, commit as `fix: mute protocol lives on the 0xFF43 collection`, and record it in Task 8.
- **(d) Total failure:** if no collection and no encoding changes the hardware state, op 0x20 does not control mute on this firmware — STOP and surface to the human with the collected log evidence (options to present: USB capture of Logitech G HUB / Blue Sherpa traffic to find the real command, or dropping the hardware-mute goal). Do NOT guess further.

- [ ] **Step 3: End-to-end toggle check (double-toggle returns to original state)**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status   # record ORIGINAL
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe toggle
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe toggle
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status   # must equal ORIGINAL
```

Expected: middle status is the inverse of ORIGINAL; final status equals ORIGINAL; the log shows two state transitions. This is the spec's required end-to-end check.

- [ ] **Step 4: Leave the mic unmuted and stop the test daemon**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe unmute
taskkill.exe /F /IM mutastic.exe
```

Expected: taskkill reports the process terminated. (Task 11's deploy starts the production instance.)

- [ ] **Step 5: Commit (only if the contingency changed code; otherwise nothing to commit)**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && git status --short
```

Expected: clean tree unless Step 2's contingency applied.

---

### Task 8: Record confirmed findings in the protocol doc

**Files:**
- Modify: `docs/yeti-x-hid-protocol.md` (its open-questions section)

**Interfaces:**
- Consumes: the observed `value=0xVV` bytes and `usage_page` values from Task 7 (preserved in `C:\Users\dan\AppData\Local\mutastic\mutastic.log`, readable at `/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log`).
- Produces: an updated protocol doc other tools can trust.

- [ ] **Step 1: Update the open questions with empirical answers**

Read the open-questions section of `docs/yeti-x-hid-protocol.md` and, for each question Task 7 resolved, replace speculation with the confirmed finding, marking each as **CONFIRMED (2026-08-08, mutastic live test)** and stating the evidence:
- Mute payload encoding: which outbound payload (ASCII `"1"`/`"0"`, or binary) actually toggled the device — per Task 7 Step 2 (or its contingency).
- Inbound value byte for mute events: binary (`0x00/0x01`) or ASCII (`0x30/0x31`) — the recorded echo values.
- The `usage_page` of the control collection — from the enumeration log lines.
- Whether the `0x20` echo reflects the NEW or the PREVIOUS state, and the confirmed polarity (payload `"1"` = muted, or inverted) — from Task 7 Step 2's two-cycle record.
- Which collection carries the mute protocol (`0xFFFF/0x0001` per the usage==1 rule, or a `0xFF43` sibling if Task 7 contingency (c) applied).
- The mute LED question must REMAIN OPEN, annotated: "pending human verification — software cannot observe the LED."

Do not restructure the document; only edit the open-questions content (and fix any statement elsewhere in the doc that the findings directly contradict, quoting the observed bytes).

- [ ] **Step 2: Verify the diff is surgical**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && git diff --stat`
Expected: only `docs/yeti-x-hid-protocol.md` changed, a handful of lines.

- [ ] **Step 3: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add docs/yeti-x-hid-protocol.md
git commit -m "docs: record empirically confirmed Yeti X mute protocol findings"
```

---

### Task 9: AutoHotkey integration — one step in the F14 handler

**Files:**
- Modify: `ahk/MuteAllMeetings.ahk` (lines 25–26 only)

**Interfaces:**
- Consumes: `mutastic.exe toggle` client semantics (Task 5); on Windows the exe sits next to the deployed .ahk in `C:\Users\dan\code\mutastic-deploy\` (Task 10's deploy script guarantees this), so `%A_ScriptDir%\mutastic.exe` resolves.
- Produces: F14 press = in-app meeting mute toggle (existing behavior, unchanged) + hardware mic mute toggle (new), non-blocking, no visible window.

**Encoding warning:** the file is UTF-8 with BOM + CRLF. Make the edit with a tool that preserves both (in-place string replacement of the exact lines; do NOT rewrite the whole file). Verify afterwards.

- [ ] **Step 1: Make the edit**

The current F14 handler (lines 25–26 of `ahk/MuteAllMeetings.ahk`):

```ahk
F14::ToggleAllMeetings()
return
```

Replace those two lines with (converting the single-line hotkey to a multi-line hotkey body — AHK v1 requires the label form for multiple statements):

```ahk
F14::
Run, "%A_ScriptDir%\mutastic.exe" toggle, %A_ScriptDir%, Hide UseErrorLevel
ToggleAllMeetings()
return
```

Why these options: `Run` is non-blocking by nature (it is not RunWait); `Hide` suppresses the launched console window; `UseErrorLevel` prevents a modal error dialog if `mutastic.exe` is missing (meetings still toggle — graceful degradation). Nothing else in the file changes.

- [ ] **Step 2: Verify bytes and diff**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git diff ahk/MuteAllMeetings.ahk
file ahk/MuteAllMeetings.ahk
```

Expected: the diff shows exactly one removed line (`F14::ToggleAllMeetings()`) and three added lines (the `F14::` label, the `Run` line, `ToggleAllMeetings()`), with the shared `return` context intact and NO other changes (no BOM churn on line 1, no whitespace noise). `file` still reports `Unicode text, UTF-8 (with BOM) text, with CRLF line terminators`. If the diff shows every line changed, the edit destroyed the line endings — `git checkout -- ahk/MuteAllMeetings.ahk` and redo preserving CRLF.

- [ ] **Step 3: Syntax-check with the real AHK v1 interpreter (parse-only)**

```bash
"/mnt/c/Program Files/AutoHotkey/AutoHotkeyU64.exe" /ErrorStdOut /iLib nul \
  "$(wslpath -w /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon/ahk/MuteAllMeetings.ahk)"; echo "exit=$?"
```

Expected: no output and `exit=0` (`/iLib` loads/parses the script without running it; syntax errors print via /ErrorStdOut with a nonzero exit). Contingency if the interpreter rejects the UNC path: `cp` the .ahk to `/mnt/c/Users/dan/AppData/Local/Temp/` and syntax-check it there instead. If the check reports a syntax error, fix the edit — do not commit a script that fails to parse.

- [ ] **Step 4: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add ahk/MuteAllMeetings.ahk
git commit -m "feat: F14 pedal press also toggles Yeti X hardware mute via mutastic"
```

---

### Task 10: deploy/deploy.cmd — Windows deployment script

**Files:**
- Create: `deploy/deploy.cmd`
- Keep as-is: `deploy/finish-setup.cmd.legacy` (historical reference; superseded, not deleted — git history plus the .legacy suffix already mark it)

**Interfaces:**
- Consumes: `bin/mutastic.exe` (Task 6), modified `ahk/MuteAllMeetings.ahk` (Task 9).
- Produces: an idempotent deployment — running `deploy.cmd` (optionally with a source-repo-root override as `%1`) leaves both programs deployed to `C:\Users\dan\code\mutastic-deploy\`, auto-started at login, and running now. Task 11 executes and verifies it.

Requirements it must implement (all from the spec):
1. Copy `mutastic.exe` and `ahk/MuteAllMeetings.ahk` from the repo — default source is derived from the script's own location (`%~dp0..`), which equals `\\wsl.localhost\Ubuntu\home\dan\code\mutastic` when run from the main checkout; `%1` overrides (used in Task 11 to deploy from this worktree).
2. Kill running instances: `mutastic.exe`, and ONLY the `AutoHotkeyU64.exe` process running MuteAllMeetings (match on command line — other AHK scripts on the machine must survive).
3. Create/update Startup-folder shortcuts for BOTH: the AHK script via `C:\Program Files\AutoHotkey\AutoHotkeyU64.exe`, and the daemon as `mutastic.exe daemon`.
4. Remove the old `MuteAllMeetings.lnk` Startup shortcut that points at `C:\Users\dan\code\mute-unmute-meetings` (explicit delete before recreating; the new shortcut reuses the same filename, so recreation also overwrites).
5. Relaunch both.
6. Do NOT delete the old `C:\Users\dan\code\mute-unmute-meetings` folder (the script only optionally READS the tray icon from it).
7. Extra (discovered during exploration): the tray icon `mic_mute_light.ico` is referenced by the .ahk (line 22) but absent from the repo — copy it from the repo if it ever appears there, else salvage it from the old deploy folder; warn if neither exists.

- [ ] **Step 1: Write the script**

Create `deploy/deploy.cmd` (ASCII, CRLF line endings — it runs under cmd.exe):

```bat
@echo off
setlocal
REM mutastic deployment (supersedes finish-setup.cmd.legacy).
REM Usage: deploy.cmd [source-repo-root]
REM Default source = this script's parent dir (the repo checkout).

set "SRC=%~dp0.."
if not "%~1"=="" set "SRC=%~1"
set "DEST=C:\Users\dan\code\mutastic-deploy"
set "STARTUP=%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup"
set "AHK_EXE=C:\Program Files\AutoHotkey\AutoHotkeyU64.exe"
set "OLD_DEPLOY=C:\Users\dan\code\mute-unmute-meetings"

echo == Stopping running instances ==
taskkill /F /IM mutastic.exe >nul 2>&1
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='AutoHotkeyU64.exe'\" | Where-Object { $_.CommandLine -like '*MuteAllMeetings*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }" >nul 2>&1

echo == Copying files from %SRC% ==
if not exist "%DEST%" mkdir "%DEST%"
copy /Y "%SRC%\bin\mutastic.exe" "%DEST%\mutastic.exe" >nul || goto :fail
copy /Y "%SRC%\ahk\MuteAllMeetings.ahk" "%DEST%\MuteAllMeetings.ahk" >nul || goto :fail
if exist "%SRC%\ahk\mic_mute_light.ico" copy /Y "%SRC%\ahk\mic_mute_light.ico" "%DEST%\" >nul
if not exist "%DEST%\mic_mute_light.ico" if exist "%OLD_DEPLOY%\mic_mute_light.ico" copy /Y "%OLD_DEPLOY%\mic_mute_light.ico" "%DEST%\" >nul
if not exist "%DEST%\mic_mute_light.ico" echo WARNING: mic_mute_light.ico not found - tray icon will be missing

echo == Replacing startup shortcuts ==
if exist "%STARTUP%\MuteAllMeetings.lnk" del /F "%STARTUP%\MuteAllMeetings.lnk"
powershell -NoProfile -Command "$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%STARTUP%\MuteAllMeetings.lnk'); $s.TargetPath = '%AHK_EXE%'; $s.Arguments = '%DEST%\MuteAllMeetings.ahk'; $s.WorkingDirectory = '%DEST%'; $s.Save()" || goto :fail
powershell -NoProfile -Command "$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%STARTUP%\Mutastic Daemon.lnk'); $s.TargetPath = '%DEST%\mutastic.exe'; $s.Arguments = 'daemon'; $s.WorkingDirectory = '%DEST%'; $s.Save()" || goto :fail

echo == Relaunching ==
start "" "%DEST%\mutastic.exe" daemon
start "" "%AHK_EXE%" "%DEST%\MuteAllMeetings.ahk"

echo Deploy complete.
exit /b 0

:fail
echo DEPLOY FAILED
exit /b 1
```

Design notes baked in: paths under `%DEST%` contain no spaces, so the PowerShell `Arguments` values need no embedded quoting; `%~dp0` expands correctly when the script is invoked via its `\\wsl.localhost\...` UNC path, and UNC works for `copy` sources (only a `cd` would break on UNC — the script never changes directory); the old folder is never written to or deleted (requirement 6).

After writing the file, force CRLF: `unix2dos deploy/deploy.cmd` (if unix2dos is absent: `sed -i 's/$/\r/' deploy/deploy.cmd` — but first confirm with `file` that the endings are currently LF, to avoid doubling `\r`).

- [ ] **Step 2: Verify CRLF**

Run: `cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon && file deploy/deploy.cmd`
Expected: `... with CRLF line terminators`.

- [ ] **Step 3: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
git add deploy/deploy.cmd
git commit -m "feat: Windows deployment script (replaces finish-setup.cmd.legacy)"
```

(Live execution and verification of the deployment is Task 11 — kept separate so this task's reviewable unit is the script itself.)

---

### Task 11: Run the deployment, end-to-end acceptance, README

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: `deploy/deploy.cmd` (Task 10), built exe (Task 6), integrated .ahk (Task 9).
- Produces: the deployed, running production setup on this machine + an updated README + the human-verification question list for final review.

- [ ] **Step 1: Run the deployment from the worktree (source override argument)**

```bash
cmd.exe /c '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\yeti-x-mute-daemon\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\yeti-x-mute-daemon'
echo "exit=$?"
```

The single quotes are REQUIRED: inside bash double quotes the leading `\\` collapses to a single `\`, so cmd.exe would receive a drive-rooted `\wsl.localhost\...` path instead of a UNC path and fail with `The system cannot find the path specified.` (empirically verified 2026-08-08). Single quotes preserve every backslash.

Expected: section banners, `Deploy complete.`, `exit=0`. (cmd may print a benign "UNC paths are not supported" cwd warning — harmless, the script never relies on cwd.)

- [ ] **Step 2: Verify deployed artifacts, shortcuts, and processes**

```bash
ls -l /mnt/c/Users/dan/code/mutastic-deploy/
ls -l "/mnt/c/Users/dan/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/"
powershell.exe -NoProfile -Command '$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut([Environment]::GetFolderPath("Startup") + "\MuteAllMeetings.lnk"); $s.TargetPath; $s.Arguments'
tasklist.exe | grep -iE 'mutastic|AutoHotkey'
ls -d /mnt/c/Users/dan/code/mute-unmute-meetings
```

(The powershell command MUST be single-quoted for bash: inside double quotes bash would expand the unset `$ws`/`$s` to empty strings and PowerShell would receive an unparsable ` = New-Object ...`. Single quotes hand `$ws`/`$s` through intact; the inner PowerShell string literals use double quotes for that reason.)

Expected:
- deploy dir contains `mutastic.exe`, `MuteAllMeetings.ahk` (and `mic_mute_light.ico` unless the WARNING fired — if it fired, note it for the human);
- Startup folder contains `MuteAllMeetings.lnk` AND `Mutastic Daemon.lnk`;
- the `MuteAllMeetings.lnk` TargetPath is `C:\Program Files\AutoHotkey\AutoHotkeyU64.exe` and its Arguments point into `mutastic-deploy` — NOT `mute-unmute-meetings` (proves the old shortcut is gone/replaced);
- `tasklist` shows both `mutastic.exe` and `AutoHotkeyU64.exe` running;
- the old folder still exists (it must NOT have been deleted).

Contingency if `mutastic.exe` is NOT running (or later vanishes): check Windows Defender Protection History FIRST (read-only: `powershell.exe -NoProfile -Command "Get-MpThreatDetection | Select-Object -First 5 | Format-List"`, or the Windows Security UI) — this machine has real-time + behavior monitoring active with a history of quarantines, and unsigned stripped cgo binaries are a known false-positive class. If Defender quarantined it: rebuild without `-ldflags "-s -w"` (edit build.sh), redeploy, and document the finding in the README troubleshooting section. (SmartScreen is a non-issue: files copied via the WSL bridge carry no Mark-of-the-Web — verified 2026-08-08.)

- [ ] **Step 3: End-to-end acceptance against the deployed daemon**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe unmute    # seed known state (see note below)
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status    # record ORIGINAL -- must be `unmuted`
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe toggle
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status    # must be `muted` (inverse of ORIGINAL)
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe toggle
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status    # must equal ORIGINAL (`unmuted`)
tail -20 "/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log"
```

The leading `unmute` is REQUIRED: Step 1's deploy freshly (re)started the daemon, and a fresh daemon reports `unknown` until its first mute command/event (the exact behavior the README documents in Step 4) -- without seeding, ORIGINAL would be `unknown` and the double-toggle check could never equal it.

Expected: ORIGINAL is `unmuted`; the middle status is `muted`; the final status equals ORIGINAL; the log file exists at the spec'd location (`%LOCALAPPDATA%\mutastic\mutastic.log`) and shows the transitions plus `command "toggle" -> ...` lines. The final state is `unmuted`, which also satisfies the leave-the-mic-unmuted requirement (no extra command needed).

- [ ] **Step 4: Update README.md**

Read the existing `README.md` and extend it (keep its existing intro; this stays the only end-user doc). It must cover, concisely:
- What mutastic is: one pedal press (F14) toggles in-app mute in all meeting apps AND the Yeti X hardware mic mute; components: `mutastic daemon` (HID + UDP 127.0.0.1:42814), the one-shot client (`mutastic toggle|mute|unmute|status`, exit codes 0/1/2), the AHK script.
- Build: `./build.sh` (WSL; cross-compiles windows/amd64 with cgo/mingw; output `bin/mutastic.exe`, not committed — build before deploying).
- Deploy: build first, then run `deploy\deploy.cmd` on Windows (source defaults to the checkout the script lives in; optional first argument overrides). What it deploys, where (`C:\Users\dan\code\mutastic-deploy`), and the two Startup shortcuts it manages.
- Troubleshooting: log at `%LOCALAPPDATA%\mutastic\mutastic.log`; `mutastic status` says `unknown` until the first mute event/command after daemon start; a second daemon exits immediately because port 42814 is taken; the daemon reconnects automatically on unplug/replug.
- Warning: never usbipd-attach the Yeti X to WSL.

- [ ] **Step 5: Run the full test suite one last time and commit**

```bash
cd /home/dan/code/mutastic/.worktrees/yeti-x-mute-daemon
go test -race ./... && go vet ./...
git add README.md
git commit -m "docs: README for mutastic build, deploy, and troubleshooting"
```

Expected: all tests PASS; focused commit.

- [ ] **Step 6: Record the human-verification questions (do NOT claim these)**

Surface these at final review as open questions for the human — software cannot verify them:
1. Does the mic's mute LED follow software-initiated mute/unmute (`mutastic mute`/`unmute`)? (Protocol-doc question deliberately left open in Task 8.)
2. Full pedal flow: does one physical F14 press toggle both the meeting apps and the mic hardware mute?
3. Physical mute button on the mic: with the daemon running, does pressing it show `event op=0x21 ...` in the log and flip `mutastic status`?
4. Unplug/replug the mic: does the log show the session ending and a successful reopen (live proof of the reconnect loop — the mechanism itself is unit-tested)?
5. Tray icon: if the deploy printed the `mic_mute_light.ico` WARNING, drop the icon file into `C:\Users\dan\code\mutastic-deploy\` (or the repo's `ahk/` dir).
6. When satisfied, the human may delete `C:\Users\dan\code\mute-unmute-meetings` (deliberately not automated).

---

## Self-Review (performed while writing this plan)

**1. Spec coverage:**
- Daemon: HID open w/ VID/PIDs + usage==1 (Task 5 `hid_windows.go`), init handshake 0x05 then 0x01 (Task 4), input-report tracking of 0x20/0x21 value@offset-9 (Tasks 1, 2, 4), UDP 127.0.0.1:42814 with toggle/mute/unmute/status where status reports current state (Tasks 3–4), op 0x20 Mute with ASCII payload (Tasks 1, 3), unplug/replug reconnect loop (Task 4 `TestReconnectAfterReadError`; live physical replug is human question 4), log file at `%LOCALAPPDATA%\mutastic\mutastic.log` (Task 5, existence verified live in Task 11 Step 3). ✓
- One-shot client printing the reply with nonzero exit when no daemon reachable (Task 5; proven on real Windows in Task 6 Step 4). ✓
- AHK: exactly one added step in the F14 handler, hidden + non-blocking, no other rewrites (Task 9). ✓
- Deployment: copies both artifacts from the repo UNC path, kills both processes (command-line-scoped AHK kill), Startup shortcuts for both, old shortcut removed, relaunches both, old folder preserved (Tasks 10–11). ✓
- Environment: no usbipd (Global Constraints, restated in Tasks 7/11 and README), exact cross-compile env vars (Task 6; toolchain verified present on this machine), Windows-toolchain fallback noted (Task 6 Step 1). ✓
- Empirical validation: payload encoding + inbound value-byte encoding resolved on real hardware (Task 7) and recorded in the protocol doc (Task 8); LED surfaced as a human question (Tasks 8, 11); daemon-running double-toggle-returns-to-original check (Tasks 7 and 11). ✓
- Tests: pure-Go unit tests for encode/decode and state tracking with the HID device abstracted behind a small interface (Tasks 1–5); no hardware needed for any `go test`. ✓

**1b. No silent deferrals:** The fake `Device` exists only in test files; production behavior is proven live on the real mic in Tasks 7 and 11 (enumeration, open, handshake, mute/unmute/toggle/status, log file — no stubs, no synthetic endpoints). Physically-actuated outcomes (LED, pedal press, mic button, replug) are inherently human-observable and are explicitly surfaced as final-review questions per the spec's own instruction ("can only be confirmed by the human — surface that as a question at final review"), not silently deferred. No UNRESOLVED COVERAGE GAPS.

**2. Placeholder scan:** No TBD/TODO/"handle edge cases"/"similar to Task N" anywhere; every code step contains the complete code; every contingency names the exact alternate action (binary-payload variant with the exact test assertions to flip, /iLib UNC fallback, hid.ErrTimeout guard removal with a verification command, toolchain install packages, Windows-side build fallback).

**3. Type consistency:** `EncodeCommand`/`DecodeEvent`/`Event`/`MutedFromValue` (Task 1) are used with identical signatures in Tasks 2–4; `Tracker.Apply/Set/Status` (Task 2) in Tasks 3–4; `Device`/`New`/`SetDevice`/`WriteReport`/`HandleCommand` (Task 3) in Task 4's `Run`/`session`/`serveUDP`; `OpenFunc` and `Run(ctx, open, pc, logger)` (Task 4) match the `main.go` call site and every `startDaemon` test call; `runClient(cmd, addr string, timeout time.Duration, out io.Writer) int` (Task 5) matches all three `main_test.go` call sites; `openYetiX(logger *log.Logger) (daemon.Device, error)` and `hideConsoleIfOwned()` have matching windows/non-windows variants (main.go passes the daemon logger via a closure that satisfies `OpenFunc`); the reply strings (`muted`/`unmuted`/`unknown`/`error: ...`) and exit codes (0/1/2) are identical across Tasks 3, 4, 5, 6, 7, 11 and the Global Constraints. ✓


**4. Load-bearing validation pass (2026-08-08):** Assumption ledger (10 verified, 1 falsified, 5 accepted with mitigation, 1 deferred) lives at `.worktrees/.the-usual-logs/yeti-x-mute-daemon/load-bearing-ledger.md`. Falsified A4 (post-handshake silence benign) is fixed by Task 4's handshake liveness gate + `TestRehandshakeAfterSilentHandshake` (and `startDaemon` now waits for `Run` to exit on cleanup, so the test's `handshakeLiveness` override cannot race the daemon goroutine under `-race`). Task 5's ErrTimeout contingency was replaced by the verified fact (the `errors.Is` guard is REQUIRED). Task 7 Step 2 is upgraded to a two-cycle test resolving encoding, new-vs-old echo semantics, and polarity, with an ordered four-branch no-echo diagnosis (liveness → encoding → sibling collection → STOP). Task 8 records the two new findings. Task 11 gains a Defender false-positive contingency. Re-ran the self-review over all edited tasks: every step remains complete (no TBDs; each contingency names exact files, assertions, commands, and commit messages); type consistency holds (`handshakeLiveness`/`errHandshakeSilence` are package-internal to `internal/daemon`; no frozen interface changed — the liveness gate is behavior inside `session`, and the new test uses only existing helpers `newFakeDevice`/`startDaemon`/`waitFor`/`inputReport`); no silent deferrals introduced (the gate is unit-tested; live flakiness observation goes through Task 7's log checks; physical actions remain explicit human questions).

**5. Fresh-eyes plan review pass (iteration 1, 2026-08-08):** An independent cross-model review found four blocking executable defects, all fixed: (1) Task 11 Step 1's deploy invocation now single-quotes the UNC paths (bash double quotes collapsed the leading `\\`, empirically producing "path not found"); (2) Task 11 Step 2's shortcut check now single-quotes the powershell command (bash was expanding the unset `$ws`/`$s` to empty strings); (3) Task 11 Step 3 now seeds known state with a leading `unmute` before recording ORIGINAL (a daemon freshly restarted by Step 1's deploy reports `unknown` until its first command/event, so the old `Expected` was unachievable) and its Expected now names the exact statuses; (4) `openYetiX` now takes the daemon logger as a parameter (`openYetiX(logger *log.Logger)`, both build variants; `main.go` closes over it to satisfy `OpenFunc`), so the `hid collection: ... usage_page=...` enumeration lines reach `mutastic.log` and Task 8's stated evidence source is real. Re-ran the self-review over the edited tasks (5, 7, 8, 11): every step remains complete with no placeholders; type consistency holds (signature updated consistently in the Task 5 interface list, both `hid_windows.go`/`hid_other.go` variants, the `main.go` closure call site, and check 3 above; `hid_windows.go` drops the now-unused `os` import, `hid_other.go` gains `log`; no frozen `internal/daemon` interface changed); no silent deferrals introduced (the acceptance gate is now satisfiable as written and still exercises status/toggle end-to-end; the mic still finishes unmuted).
