package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mutastic/internal/light"
	"mutastic/internal/proto"
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

// TestConditionalMuteVerbs pins the ATOMIC conditional mic verbs
// (R6-F2): premise check and action in one step. Match runs the absolute
// verb (one HID write with the right payload byte) AND one F24 sweep
// (inject-count via the fake injector) and replies "ok"; a no-match
// (definitive opposite) or an unknown premise replies "flipped <state>"
// with NOTHING run; malformed forms fall to "error: unknown command".
func TestConditionalMuteVerbs(t *testing.T) {
	// Known-unmuted premise matched by "mute-if unmuted": verb + sweep.
	d := New(testLogger())
	dev := newFakeDevice()
	inj := &fakeInjector{}
	d.SetDevice(dev)
	d.Inject = inj
	d.Track.Set(false) // known unmuted
	if got := d.HandleCommand("mute-if unmuted"); got != "ok" {
		t.Fatalf("mute-if unmuted at known-unmuted = %q, want ok", got)
	}
	if got := dev.writeCount(); got != 1 {
		t.Fatalf("HID writes after matched mute-if = %d, want 1 (the absolute verb)", got)
	}
	if w := dev.write(0); w[4] != 0x20 || w[9] != '1' {
		t.Fatalf("mute-if wrote % x; want op 0x20 payload '1'", w[:12])
	}
	if muted, known := d.Track.Status(); !known || !muted {
		t.Fatalf("tracker after matched mute-if = (%v, %v), want (true, true)", muted, known)
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injector calls after matched mute-if = %d, want 1 (one F24 sweep)", got)
	}

	// The state is now muted: the same premise must refuse with NO new
	// write and NO new sweep.
	if got := d.HandleCommand("mute-if unmuted"); got != "flipped muted" {
		t.Fatalf("mute-if unmuted at muted = %q, want %q", got, "flipped muted")
	}
	if got := dev.writeCount(); got != 1 {
		t.Fatalf("HID writes after flipped mute-if = %d, want 1 (a refusal writes nothing)", got)
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injector calls after flipped mute-if = %d, want 1 (a refusal never injects)", got)
	}

	// Known-muted premise matched by "unmute-if muted": verb('0') + sweep.
	if got := d.HandleCommand("unmute-if muted"); got != "ok" {
		t.Fatalf("unmute-if muted at muted = %q, want ok", got)
	}
	if w := dev.write(1); w[4] != 0x20 || w[9] != '0' {
		t.Fatalf("unmute-if wrote % x; want op 0x20 payload '0'", w[:12])
	}
	if muted, known := d.Track.Status(); !known || muted {
		t.Fatalf("tracker after matched unmute-if = (%v, %v), want (false, true)", muted, known)
	}
	if got := inj.calls.Load(); got != 2 {
		t.Fatalf("injector calls after matched unmute-if = %d, want 2", got)
	}

	// Definitive-opposite refusals carry the CURRENT state verbatim.
	if got := d.HandleCommand("unmute-if muted"); got != "flipped unmuted" {
		t.Fatalf("unmute-if muted at unmuted = %q, want %q", got, "flipped unmuted")
	}
	if got := dev.writeCount(); got != 2 || inj.calls.Load() != 2 {
		t.Fatalf("a flipped refusal ran something: writes=%d injects=%d, want both frozen", dev.writeCount(), inj.calls.Load())
	}

	// Unknown premise (no state ever seen): refuse, nothing runs.
	d2 := New(testLogger())
	dev2 := newFakeDevice()
	inj2 := &fakeInjector{}
	d2.SetDevice(dev2)
	d2.Inject = inj2
	if got := d2.HandleCommand("mute-if unmuted"); got != "flipped unknown" {
		t.Fatalf("mute-if unmuted at unknown = %q, want %q", got, "flipped unknown")
	}
	if got := d2.HandleCommand("unmute-if muted"); got != "flipped unknown" {
		t.Fatalf("unmute-if muted at unknown = %q, want %q", got, "flipped unknown")
	}
	if got := dev2.writeCount(); got != 0 || inj2.calls.Load() != 0 {
		t.Fatalf("unknown-premise conditional ran something: writes=%d injects=%d, want 0/0", got, inj2.calls.Load())
	}

	// Malformed forms are NOT conditional verbs at all.
	for _, bad := range []string{"mute-if sideways", "mute-if", "unmute-if MUTED", "mute-ify muted", "mute-if muted extra"} {
		if got := d2.HandleCommand(bad); got != "error: unknown command" {
			t.Fatalf("HandleCommand(%q) = %q, want %q (malformed conditional forms fail loudly)", bad, got, "error: unknown command")
		}
	}
}

// TestConditionalMuteFailureModes pins the error:-prefixed shapes
// (R6-F2): a failed HID write runs NO sweep (the mic never moved, so
// sweeping the apps alone would desync them), while an injection failure
// AFTER a successful write is still an error reply (the mic DID move -
// the honest reading of a half-failed mute-everything), with the tracker
// left at the new state either way the write landed.
func TestConditionalMuteFailureModes(t *testing.T) {
	// No device: the premise matches but the write fails -> error, NO sweep.
	d := New(testLogger())
	inj := &fakeInjector{}
	d.Inject = inj
	d.Track.Set(false)
	if got := d.HandleCommand("mute-if unmuted"); got != "error: no device" {
		t.Fatalf("mute-if with no device = %q, want %q", got, "error: no device")
	}
	if got := inj.calls.Load(); got != 0 {
		t.Fatalf("injector calls after a failed write = %d, want 0 (a mic that never moved must not sweep apps)", got)
	}

	// Injector wired but failing: write succeeds, sweep fails -> error,
	// tracker still moved (the verb half committed).
	d2 := New(testLogger())
	dev2 := newFakeDevice()
	inj2 := &fakeInjector{err: errors.New("sendinput exploded")}
	d2.SetDevice(dev2)
	d2.Inject = inj2
	d2.Track.Set(false)
	if got := d2.HandleCommand("mute-if unmuted"); got != "error: sendinput exploded" {
		t.Fatalf("mute-if with a failing injector = %q, want the error reply", got)
	}
	if muted, known := d2.Track.Status(); !known || !muted {
		t.Fatalf("tracker after write-ok/inject-failed mute-if = (%v, %v), want (true, true)", muted, known)
	}
	if got := inj2.calls.Load(); got != 1 {
		t.Fatalf("injector calls = %d, want 1 (the sweep was attempted)", got)
	}

	// No injector at all (a platform without SendInput): the write still
	// commits and the reply honestly says the sweep half was unavailable.
	d3 := New(testLogger())
	dev3 := newFakeDevice()
	d3.SetDevice(dev3)
	d3.Track.Set(false)
	if got := d3.HandleCommand("mute-if unmuted"); got != "error: key injection unavailable" {
		t.Fatalf("mute-if with no injector = %q, want %q", got, "error: key injection unavailable")
	}
	if muted, known := d3.Track.Status(); !known || !muted {
		t.Fatalf("tracker after no-injector mute-if = (%v, %v), want (true, true)", muted, known)
	}
}

// TestConditionalMuteVerbsOverUDP pins the wire traversal (R6-F2): the
// conditional verbs and all three reply shapes round-trip over real UDP,
// as one atomic step per datagram.
func TestConditionalMuteVerbsOverUDP(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	if got := ask("mute-if unmuted"); got != "flipped unknown" {
		t.Fatalf("mute-if unmuted at unknown over UDP = %q, want %q", got, "flipped unknown")
	}
	// Establish known-unmuted via an absolute verb (no sweep on the plain
	// verbs - only the conditional ones and physical presses inject).
	if got := ask("unmute"); got != "unmuted" {
		t.Fatalf("unmute = %q, want unmuted", got)
	}
	if got := ask("mute-if unmuted"); got != "ok" {
		t.Fatalf("mute-if unmuted at unmuted over UDP = %q, want ok", got)
	}
	waitFor(t, "one sweep injected", func() bool { return inj.calls.Load() == 1 })
	if got := ask("mute-if unmuted"); got != "flipped muted" {
		t.Fatalf("mute-if unmuted at muted over UDP = %q, want %q", got, "flipped muted")
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injector calls after the flipped refusal = %d, want 1", got)
	}
	if got := ask("mute-if sideways"); got != "error: unknown command" {
		t.Fatalf("malformed conditional over UDP = %q, want %q", got, "error: unknown command")
	}
}

func TestHandleCommandShutdownWithoutHook(t *testing.T) {
	d := New(testLogger())
	if got := d.HandleCommand("shutdown"); got != "error: shutdown not supported" {
		t.Fatalf("shutdown without hook = %q, want %q", got, "error: shutdown not supported")
	}
}

func TestHandleCommandShutdownDoesNotFireHook(t *testing.T) {
	// HandleCommand only answers; serveUDP fires the hook after writing
	// the reply, or Run's cancel watcher could close pc before the send.
	d := New(testLogger())
	d.Shutdown = func() { t.Error("Shutdown fired inside HandleCommand; want it fired post-reply by serveUDP") }
	if got := d.HandleCommand("shutdown"); got != "shutting down" {
		t.Fatalf("shutdown = %q, want %q", got, "shutting down")
	}
}

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

type blockingLightHandler struct {
	mu       sync.Mutex
	commands []string
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (h *blockingLightHandler) HandleCommand(cmd string) string {
	h.mu.Lock()
	h.commands = append(h.commands, cmd)
	h.mu.Unlock()
	if cmd == "brightness-delta 5" {
		h.once.Do(func() { close(h.started) })
		<-h.release
		return "delta done"
	}
	return "status done"
}

func (h *blockingLightHandler) commandCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.commands)
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
	return startDaemonAll(t, open, nil, nil)
}

// startDaemonInject is startDaemon with a KeyInjector wired into Run.
func startDaemonInject(t *testing.T, open OpenFunc, inject KeyInjector) (addr string, ask func(cmd string) string) {
	t.Helper()
	return startDaemonAll(t, open, nil, inject)
}

// startDaemonLight is startDaemon with a light CommandHandler wired into
// Run.
func startDaemonLight(t *testing.T, open OpenFunc, light CommandHandler) (addr string, ask func(cmd string) string) {
	t.Helper()
	return startDaemonAll(t, open, light, nil)
}

// startDaemonAll is the single shared body behind the startDaemon helpers:
// it runs Run() with the given OpenFunc (plus optional light handler and
// injector) on an ephemeral UDP port and returns the UDP address plus a
// UDP request helper.
func startDaemonAll(t *testing.T, open OpenFunc, light CommandHandler, inject KeyInjector) (addr string, ask func(cmd string) string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, open, light, inject, nil, pc, testLogger())
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

	// Physical button press (binary value) drives status.
	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "muted status", func() bool { return ask("status") == "muted" })

	// A second physical button press (binary value) drives status again.
	// (0x20 SoftwareMute echoes must NOT drive status -- covered separately
	// by TestSoftwareMuteEchoDoesNotResetOptimisticState.)
	dev.events <- inputReport(0x21, 0x00)
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

func TestShutdownOverUDPStopsDaemon(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	open := func() (Device, error) { return newFakeDevice(), nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, open, nil, nil, cancel, pc, testLogger())
		close(done)
	}()
	addr := pc.LocalAddr().String()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("shutdown")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no reply to shutdown: %v", err)
	}
	// The ack must arrive even though the daemon is tearing itself down:
	// the tray's Quit treats a missing reply as "daemon unreachable".
	if got := string(buf[:n]); got != "shutting down" {
		t.Fatalf("shutdown reply = %q, want %q", got, "shutting down")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the shutdown command")
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

// --- UDP serve-loop resilience (fake PacketConn to inject socket errors) ---

type readResult struct {
	data []byte
	err  error
}

// fakePacketConn scripts ReadFrom results (so tests can inject transient
// socket errors), optionally fails WriteTo calls, and records successful
// replies. Close makes ReadFrom return net.ErrClosed, mirroring a real
// closed listener.
type fakePacketConn struct {
	reads     chan readResult // scripted ReadFrom results, consumed in order
	writeErrs chan error      // scripted WriteTo failures (empty = success)
	replies   chan string     // successfully written replies
	closed    chan struct{}
	once      sync.Once
}

func newFakePacketConn() *fakePacketConn {
	return &fakePacketConn{
		reads:     make(chan readResult, 8),
		writeErrs: make(chan error, 8),
		replies:   make(chan string, 8),
		closed:    make(chan struct{}),
	}
}

func (f *fakePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case r := <-f.reads:
		if r.err != nil {
			return 0, nil, r.err
		}
		return copy(p, r.data), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, nil
	case <-f.closed:
		return 0, nil, net.ErrClosed
	}
}

func (f *fakePacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	select {
	case err := <-f.writeErrs:
		return 0, err
	default:
	}
	f.replies <- string(p)
	return len(p), nil
}

func (f *fakePacketConn) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakePacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 42814}
}

func (f *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (f *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

// TestServeUDPSurvivesTransientErrors guards the daemon's core availability
// property: transient socket errors must not kill the command loop. On
// Windows this failure class is realistic, not hypothetical — a reply that
// lands after the one-shot client (1s timeout) already closed its socket
// elicits ICMP Port Unreachable, and the next ReadFrom then fails with
// WSAECONNRESET because Go's net package never disables SIO_UDP_CONNRESET
// (golang/go#5834). Only net.ErrClosed (listener closed on shutdown) may
// terminate the loop.
func TestServeUDPSurvivesTransientErrors(t *testing.T) {
	d := New(testLogger())
	pc := newFakePacketConn()
	done := make(chan struct{})
	go func() {
		d.serveUDP(pc)
		close(done)
	}()

	// 1) A transient read error (the WSAECONNRESET case) must be survived.
	pc.reads <- readResult{err: errors.New("wsarecv: connection reset by peer")}
	// 2) A command whose reply write fails must not kill the loop either.
	pc.writeErrs <- errors.New("wsasendto: connection reset by peer")
	pc.reads <- readResult{data: []byte("status")}
	// 3) The next command must still be answered.
	pc.reads <- readResult{data: []byte("status")}

	select {
	case reply := <-pc.replies:
		if reply != "unknown" {
			t.Fatalf("reply = %q, want %q (fresh daemon, no device)", reply, "unknown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveUDP stopped answering after transient socket errors")
	}

	// 4) Closing the listener is the only way the loop may exit.
	pc.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveUDP did not exit after the listener closed")
	}
}

func TestHandleCommandRoutesLightAtPrefix(t *testing.T) {
	f := &fakeLightHandler{reply: "on 40% 4950K"}
	d := New(testLogger())
	d.Light = f
	if got := d.HandleCommand("light@desk toggle"); got != "on 40% 4950K" {
		t.Fatalf("reply = %q, want pass-through of handler reply", got)
	}
	if len(f.got) != 1 || f.got[0] != "@desk toggle" {
		t.Fatalf("handler received %v, want [\"@desk toggle\"]", f.got)
	}
}

// --- physical mute-button -> key injection (fake HID + fake injector) ---

// fakeInjector implements KeyInjector: counts calls, returns err.
type fakeInjector struct {
	calls atomic.Int32
	err   error
}

func (f *fakeInjector) Inject() error {
	f.calls.Add(1)
	return f.err
}

func TestDeviceMuteEventInjectsSweepKey(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, _ = startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "one injection", func() bool { return inj.calls.Load() == 1 })
}

func TestSoftwareMuteEchoDoesNotInject(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	// The 0x20 echo must not change status (fixed regression) and must
	// never inject. A genuine 0x21 device event, sent right after, is the
	// synchronization barrier: events are read off one channel in order by
	// a single goroutine, so once the 0x21's effect (status == "muted") is
	// visible over UDP, the 0x20 ahead of it has already been fully
	// processed (Apply + the injection check, both no-ops for op 0x20).
	dev.events <- inputReport(0x20, '1')
	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "device event tracked", func() bool { return ask("status") == "muted" })

	// The 0x21 barrier is itself a real physical-press event, so it fires
	// the injector exactly once. Asserting the count stops at exactly 1 --
	// not 0, since a real device event legitimately injects -- proves the
	// preceding 0x20 echo contributed no extra (spurious) injection.
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injector calls = %d, want 1 (only the 0x21 barrier should inject; the 0x20 echo must not)", got)
	}
}

func TestSoftwareMuteEchoDoesNotResetOptimisticState(t *testing.T) {
	dev := newFakeDevice()
	_, ask := startDaemon(t, func() (Device, error) { return dev, nil })
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	if got := ask("mute"); got != "muted" {
		t.Fatalf("mute = %q, want muted", got)
	}
	waitFor(t, "optimistic state set", func() bool { return ask("status") == "muted" })

	// Exact production garbage shape (13:36 incident logs): a 0x20 echo
	// with value byte 0x00, which decodes as "unmuted" if it were ever
	// (wrongly) trusted. A brief wait gives the session goroutine time to
	// consume it off the fake device's buffered event channel (read within
	// its 10ms poll loop) before we check that nothing changed.
	dev.events <- inputReport(0x20, 0x00)
	time.Sleep(50 * time.Millisecond)

	if got := ask("status"); got != "muted" {
		t.Fatalf("status after 0x20 garbage echo = %q, want %q (echo must not reset optimistic state)", got, "muted")
	}
}

func TestDeviceMuteChatterIsDebounced(t *testing.T) {
	// Registered BEFORE startDaemonInject: t.Cleanup is LIFO, so the
	// harness's stop-and-join cleanup runs first and Run's goroutine is
	// gone before the var is restored (commit eebd6c7 discipline).
	old := muteInjectDebounce
	muteInjectDebounce = time.Hour // the window cannot lapse mid-test
	t.Cleanup(func() { muteInjectDebounce = old })

	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "first injection", func() bool { return inj.calls.Load() == 1 })

	dev.events <- inputReport(0x21, 0x00) // chatter, inside the window
	waitFor(t, "chatter tracked", func() bool { return ask("status") == "unmuted" })
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("chatter injected: calls = %d, want 1", got)
	}
}

func TestDeviceMuteFiresAgainAfterDebounceWindow(t *testing.T) {
	old := muteInjectDebounce
	muteInjectDebounce = time.Millisecond
	t.Cleanup(func() { muteInjectDebounce = old })

	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, _ = startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "first injection", func() bool { return inj.calls.Load() == 1 })

	time.Sleep(5 * time.Millisecond) // let the 1ms window lapse
	dev.events <- inputReport(0x21, 0x00)
	waitFor(t, "second injection", func() bool { return inj.calls.Load() == 2 })
}

func TestInjectFailureIsNonFatal(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{err: errors.New("sendinput exploded")}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "failed injection attempted", func() bool { return inj.calls.Load() == 1 })
	// The daemon must keep tracking and serving after the failure.
	waitFor(t, "daemon still serves status", func() bool { return ask("status") == "muted" })
}

// TestLogCommandDedupesRepeatedStatus guards the daemon-side log-growth
// bound: a resident poller (the OpenDeck plugin asks "status" every ~750ms)
// must not grow the log when nothing changed, because rotation runs only at
// daemon start. Contract: a "status" command logs only when its reply
// differs from the previously LOGGED status reply; the first status after
// start always logs; every non-status command always logs, even repeats
// with identical replies.
func TestLogCommandDedupesRepeatedStatus(t *testing.T) {
	var buf bytes.Buffer
	d := New(log.New(&buf, "", 0))

	// take returns everything logged since the last call and resets the
	// buffer, so each assertion sees only its own command's output.
	take := func() string {
		s := buf.String()
		buf.Reset()
		return s
	}

	// 1) The first status after start always logs.
	d.logCommand("status", "muted")
	if got, want := take(), "command \"status\" -> \"muted\"\n"; got != want {
		t.Fatalf("first status logged %q, want %q (first status after start must always log)", got, want)
	}

	// 2) An unchanged status reply is suppressed.
	d.logCommand("status", "muted")
	if got := take(); got != "" {
		t.Fatalf("repeated identical status logged %q, want no output (unchanged status must be suppressed)", got)
	}

	// 3) A changed status reply logs.
	d.logCommand("status", "unmuted")
	if got, want := take(), "command \"status\" -> \"unmuted\"\n"; got != want {
		t.Fatalf("changed status logged %q, want %q (a changed status reply must log)", got, want)
	}

	// 4) Non-status commands ALWAYS log, even repeated with identical
	// replies (each command sent twice with the same reply).
	for _, c := range []struct{ cmd, reply string }{
		{"toggle", "muted"}, {"toggle", "muted"},
		{"mute", "muted"}, {"mute", "muted"},
		{"unmute", "unmuted"}, {"unmute", "unmuted"},
		{"light toggle", "on 64% 4950K"}, {"light toggle", "on 64% 4950K"},
	} {
		d.logCommand(c.cmd, c.reply)
		want := fmt.Sprintf("command %q -> %q\n", c.cmd, c.reply)
		if got := take(); got != want {
			t.Fatalf("non-status %q logged %q, want %q (non-status commands must always log, even identical repeats)", c.cmd, got, want)
		}
	}

	// 5) After the non-status commands above, an unchanged status reply is
	// still suppressed: dedupe keys on the last LOGGED status reply
	// ("unmuted", from step 3), not on the last command.
	d.logCommand("status", "unmuted")
	if got := take(); got != "" {
		t.Fatalf("status after non-status commands logged %q, want no output (dedupe must key on the last status reply, not the last command)", got)
	}

	// A genuinely changed status still logs after that suppression.
	d.logCommand("status", "muted")
	if got, want := take(), "command \"status\" -> \"muted\"\n"; got != want {
		t.Fatalf("changed status after suppression logged %q, want %q", got, want)
	}
}

// TestLogCommandSuppressesRepeatedLightStatus: the lights key polls
// "light status" every ~750ms, exactly like the mute key polls
// "status" — both need the repeated-reply latch or the log grows
// unbounded (rotation runs only at daemon start).
func TestLogCommandSuppressesRepeatedLightStatus(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{Logger: log.New(&buf, "", 0)}
	d.logCommand("light status", "COM4: off")
	d.logCommand("light status", "COM4: off")          // identical: suppressed
	d.logCommand("light status", "COM4: on 30% 2900K") // changed: logs
	d.logCommand("status", "muted")                    // separate latch, separate bookkeeping
	d.logCommand("light toggle", "COM4: off")          // non-poll verbs always log
	if got := strings.Count(buf.String(), `"light status"`); got != 2 {
		t.Fatalf("light status logged %d times, want 2 (first + change):\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), `"light toggle"`); got != 1 {
		t.Fatalf("light toggle logged %d times, want 1:\n%s", got, buf.String())
	}
}

// TestLogCommandSuppressesRepeatedSettingsList: the tray reconciles its
// Saved-settings menu by polling "light settings list" every 2 s — the
// same log-growth bound as the status pollers (rotation runs only at
// daemon start), so the repeated-reply latch applies exactly like
// "status"/"light status". Non-poll settings verbs (save/apply/delete)
// always log, even identical repeats, and each latch stays independent.
func TestLogCommandSuppressesRepeatedSettingsList(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{Logger: log.New(&buf, "", 0)}
	d.logCommand("light settings list", "alpha") // first: logs
	d.logCommand("light settings list", "alpha") // identical: suppressed
	d.logCommand("light settings list", "beta")  // changed: logs
	d.logCommand("light settings list", "beta")  // identical: suppressed
	d.logCommand("light settings save movie", `saved "movie" (2 lights)`)
	d.logCommand("light settings save movie", `saved "movie" (2 lights)`) // repeat: still logs
	d.logCommand("status", "muted")                                       // separate latch, first logs
	d.logCommand("status", "muted")                                       // suppressed
	if got := strings.Count(buf.String(), `"light settings list"`); got != 2 {
		t.Fatalf("light settings list logged %d times, want 2 (first + change):\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), `"light settings save movie"`); got != 2 {
		t.Fatalf("settings save logged %d times, want 2 (non-poll verbs always log, even identical repeats):\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), `"status"`); got != 1 {
		t.Fatalf("status logged %d times, want 1 (latches are independent):\n%s", got, buf.String())
	}
}

// scriptedSettingsFleet is a scripted stand-in for the light fleet's
// settings verbs: it answers from a fixed reply script and records the
// exact commands it received, proving datagrams traverse the daemon's
// "light"-prefix routing verbatim.
type scriptedSettingsFleet struct {
	mu      sync.Mutex
	replies map[string]string
	got     []string
}

func (f *scriptedSettingsFleet) HandleCommand(cmd string) string {
	f.mu.Lock()
	f.got = append(f.got, cmd)
	f.mu.Unlock()
	return f.replies[cmd]
}

func (f *scriptedSettingsFleet) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// TestLightSettingsVerbsTraverseDaemonOverUDP is characterization coverage
// for the settings verbs' route through the daemon: "light settings ..."
// datagrams reach the light handler unchanged (a colliding top-level
// "settings" verb must never shadow them) and the replies round-trip
// byte-for-byte over real UDP — including the empty-string list reply of
// an empty store, a zero-length UDP datagram.
func TestLightSettingsVerbsTraverseDaemonOverUDP(t *testing.T) {
	fleet := &scriptedSettingsFleet{replies: map[string]string{
		"settings save movie":   `saved "movie" (2 lights)`,
		"settings list":         "",
		"settings apply movie":  "COM4: on 47% 2900K\nCOM7: off",
		"settings apply nope":   `error: unknown setting "nope"`,
		"settings delete movie": `deleted "movie"`,
	}}
	open := func() (Device, error) { return newFakeDevice(), nil }
	_, ask := startDaemonLight(t, open, fleet)

	for _, c := range []struct{ cmd, want string }{
		{"light settings save movie", `saved "movie" (2 lights)`},
		{"light settings list", ""}, // empty store: the zero-length datagram
		{"light settings apply movie", "COM4: on 47% 2900K\nCOM7: off"},
		{"light settings apply nope", `error: unknown setting "nope"`},
		{"light settings delete movie", `deleted "movie"`},
	} {
		if got := ask(c.cmd); got != c.want {
			t.Errorf("ask(%q) = %q, want %q (byte-for-byte round trip)", c.cmd, got, c.want)
		}
	}
	want := []string{
		"settings save movie",
		"settings list",
		"settings apply movie",
		"settings apply nope",
		"settings delete movie",
	}
	if got := fleet.commands(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("handler received %v, want the verbatim command sequence %v", got, want)
	}
}

// --- R7-F1: opMu makes conditional verbs atomic vs the event goroutine ---

// blockingInjector blocks inside Inject until released (only the FIRST call
// announces itself on entered), so a test can hold a compound operation
// open exactly mid-sweep.
type blockingInjector struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func newBlockingInjector() *blockingInjector {
	return &blockingInjector{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingInjector) Inject() error {
	b.calls.Add(1)
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

// TestConditionalVerbHoldsOpMuAcrossCompound pins R7-F1 structurally: the
// conditional verb's whole stretch (premise read -> HID write -> tracker
// update -> F24 sweep) runs under d.opMu, and the session goroutine's
// event compound (tracker Apply -> sweep) takes the SAME mutex, so a
// physical 0x21 press can never interleave with the verb's stretch. Pre
// R7-F1 the event could flip the tracked state AND sweep while a matched
// conditional, already past its premise read, swept again - one real
// transition, two sweeps (apps toggled twice = desynced from the mic).
// Pin: while a matched conditional blocks MID-SWEEP (inside Inject), the
// event path on the same daemon MUST block; after the sweep releases, the
// event applies normally, exactly one sweep per acting path.
func TestConditionalVerbHoldsOpMuAcrossCompound(t *testing.T) {
	d := New(testLogger())
	dev := newFakeDevice()
	inj := newBlockingInjector()
	d.SetDevice(dev)
	d.Inject = inj
	d.Track.Set(false) // known unmuted: the "mute-if unmuted" premise

	replyCh := make(chan string, 1)
	go func() { replyCh <- d.HandleCommand("mute-if unmuted") }()
	select {
	case <-inj.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("conditional verb never reached the sweep")
	}

	// The verb is parked mid-sweep holding opMu; processing a physical
	// 0x21 press right now MUST block (pre-R7-F1 it would have applied
	// and swept concurrently - the double-sweep straddle).
	eventDone := make(chan struct{})
	go func() {
		d.handleEvent(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01})
		close(eventDone)
	}()
	select {
	case <-eventDone:
		t.Fatal("the 0x21 event was processed while the conditional verb's compound was mid-sweep (opMu does not span premise->...->sweep)")
	case <-time.After(100 * time.Millisecond):
	}

	close(inj.release)
	if got := <-replyCh; got != "ok" {
		t.Fatalf("conditional reply = %q, want ok", got)
	}
	select {
	case <-eventDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the 0x21 event stayed blocked after the verb released its sweep")
	}
	if got := inj.calls.Load(); got != 2 {
		t.Fatalf("injects = %d, want 2 (one for the matched verb, one for the physical press - each acting path swept exactly once)", got)
	}
	if muted, known := d.Track.Status(); !known || !muted {
		t.Fatalf("tracker = (%v, %v), want (true, true): the serialized event applied after the verb", muted, known)
	}
}

// TestConcurrentConditionalsAndEventsSweepCountConsistent is the stress
// half of R7-F1, meant to run under -race: concurrent conditional-verb
// goroutines PLUS a stream of injected physical 0x21 events must never
// produce an unaccounted sweep. With every compound serialized by opMu
// the sweep count is exactly (#ok conditionals) + (#processed 0x21
// events) - every "ok" moved the mic once and swept once; every physical
// press swept once; a refusal swept never. The HID write count likewise
// tracks only matched conditionals (plus handshake + setup).
func TestConcurrentConditionalsAndEventsSweepCountConsistent(t *testing.T) {
	// Registered BEFORE startDaemonInject: t.Cleanup is LIFO, so the
	// harness's stop-and-join cleanup runs first and Run's goroutines are
	// gone before the var is restored.
	old := muteInjectDebounce
	muteInjectDebounce = 0 // every 0x21 press injects; none are debounce-suppressed
	t.Cleanup(func() { muteInjectDebounce = old })

	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	// Establish a known state; plain "unmute" never sweeps.
	if got := ask("unmute"); got != "unmuted" {
		t.Fatalf("setup unmute = %q, want unmuted", got)
	}

	const workers = 4
	const rounds = 30
	var oks atomic.Int32
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				cmd := "mute-if unmuted"
				if (w+i)%2 == 1 {
					cmd = "unmute-if muted"
				}
				switch reply := ask(cmd); {
				case reply == "ok":
					oks.Add(1)
				case strings.HasPrefix(reply, "flipped "):
				default:
					t.Errorf("conditional reply %q, want ok or flipped <state> (the fake device/injector never fail)", reply)
					return
				}
			}
		}(w)
	}

	// Concurrent physical presses, alternating directions, known count.
	const presses = 40
	for i := 0; i < presses; i++ {
		val := byte(0x01)
		if i%2 == 1 {
			val = 0x00
		}
		dev.events <- inputReport(0x21, val)
	}
	wg.Wait()

	// Barrier: every press must have been processed (each injects exactly
	// once post-opMu; the count cannot overshoot: only matched verbs and
	// processed presses ever inject).
	wantInjects := int(oks.Load()) + presses
	waitFor(t, "all presses processed", func() bool { return inj.calls.Load() == int32(wantInjects) })
	if got := inj.calls.Load(); got != int32(wantInjects) {
		t.Fatalf("sweeps = %d, want exactly %d (%d matched verbs + %d presses): an unaccounted sweep is the R7-F1 double-sweep", got, wantInjects, oks.Load(), presses)
	}
	if got, want := dev.writeCount(), 3+int(oks.Load()); got != want {
		t.Fatalf("HID writes = %d, want %d (2 handshake + 1 setup + %d matched verbs; a refusal never writes)", got, want, oks.Load())
	}
	// Final state consistency: after all compounds the tracker holds a
	// definitive state (some serialized last op won), and the harassment
	// above never left it unknown or half-written.
	if got := ask("status"); got != "muted" && got != "unmuted" {
		t.Fatalf("final status = %q, want a definitive muted|unmuted", got)
	}
}

// --- R7-F3: 128-byte receive buffer vs oversized settings deletes ---

// TestOversizedSettingsDeleteRejectsWholeOnWire pins R7-F3 over a REAL
// socket pair with the REAL light store handler wired in: the largest
// legal command is 64 bytes (22-byte "light settings delete " prefix +
// the 42-byte name cap), so while 63/64-byte deletes act normally, the
// 65-byte delete of a 43-byte name arrives WHOLE in the 128-byte buffer
// and the store's own byte cap rejects it with
// "error: settings name too long (max 42 bytes)" - on every platform.
// Pre-R7-F3 (64-byte buffer) that datagram TRUNCATED to 64 bytes on Unix
// and deleted the existing 42-byte prefix-named setting; the test pins
// that the prefix-named setting SURVIVES the rejected oversized delete.
func TestOversizedSettingsDeleteRejectsWholeOnWire(t *testing.T) {
	dir := t.TempDir()
	name41 := strings.Repeat("a", 41)
	name42 := strings.Repeat("b", 42)
	entry := light.SavedSetting{Lights: map[string]light.SavedLightState{
		"COM4": {On: true, Brightness: 30, TempByte: 0},
	}}
	seed := map[string]light.SavedSetting{name41: entry, name42: entry}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "light-settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	storeHandler := light.NewMultiManager(testLogger(), dir, light.NewRegistry(""), func() ([]string, error) { return nil, nil }, nil)
	open := func() (Device, error) { return newFakeDevice(), nil }
	_, ask := startDaemonLight(t, open, storeHandler)

	const deletePrefix = "light settings delete " // 22 bytes
	if len(deletePrefix) != 22 {
		t.Fatalf("test premise broken: prefix is %d bytes, want 22", len(deletePrefix))
	}
	// 1) The destructive case first, while the 42-byte name still exists:
	// a 65-byte delete (43-byte name) must be REJECTED, not truncated.
	cmd65 := deletePrefix + name42 + "c"
	if len(cmd65) != 65 {
		t.Fatalf("test premise broken: cmd65 is %d bytes, want 65", len(cmd65))
	}
	if got, want := ask(cmd65), "error: settings name too long (max 42 bytes)"; got != want {
		t.Fatalf("65-byte delete = %q, want %q (it must arrive WHOLE and be rejected by the store's byte cap)", got, want)
	}
	// The 42-byte prefix-named setting MUST still exist: pre-R7-F3 the
	// truncated datagram deleted it. The full store lists both names
	// sorted ("aaa..." < "bbb...").
	if got, want := ask("light settings list"), name41+"\n"+name42; got != want {
		t.Fatalf("list after the rejected oversized delete = %q, want %q (the prefix-named setting must SURVIVE)", got, want)
	}
	// 2) 63-byte and 64-byte deletes act normally through the whole pipe.
	cmd63 := deletePrefix + name41
	cmd64 := deletePrefix + name42
	if len(cmd63) != 63 || len(cmd64) != 64 {
		t.Fatalf("test premise broken: cmd63/cmd64 = %d/%d bytes, want 63/64", len(cmd63), len(cmd64))
	}
	if got, want := ask(cmd63), `deleted "`+name41+`"`; got != want {
		t.Fatalf("63-byte delete = %q, want %q", got, want)
	}
	if got, want := ask(cmd64), `deleted "`+name42+`"`; got != want {
		t.Fatalf("64-byte delete = %q, want %q", got, want)
	}
	if got := ask("light settings list"); got != "" {
		t.Fatalf("list after both legal deletes = %q, want empty (none saved)", got)
	}
}

// --- R8-F1: full-buffer datagrams are refused, never dispatched ---

// TestFullBufferDatagramRefusedOnWire pins R8-F1 (Critical) over a REAL
// socket pair with the REAL light store handler: a read that FILLS the
// 128-byte receive buffer can never be a legal command (the largest legal
// command is 64 bytes), so it is definitionally truncated or hostile and
// is answered "error: command too long" WITHOUT dispatch. The pinned
// attack: "light settings delete " + 100 spaces + "target" + a >128-byte
// suffix truncates on Unix at 128 bytes into exactly "delete ... target"
// with the padding in between - TrimSpace and the handler's raw-suffix
// name re-read collapse
// the padding, so pre-R8-F1 the daemon DELETED the existing "target"
// setting (a hostile command manufactured from a legal one plus pure
// whitespace). Post-fix the attack is refused and "target" SURVIVES; an
// exactly-128-byte padded "status" (not even truncated - just hostile
// size) is refused by the same size rule; 63/64-byte deletes still act
// normally end-to-end; and the 65-byte delete of a 43-byte name still
// draws the store's documented 42-byte validation error.
func TestFullBufferDatagramRefusedOnWire(t *testing.T) {
	dir := t.TempDir()
	name42 := strings.Repeat("b", 42)
	entry := light.SavedSetting{Lights: map[string]light.SavedLightState{
		"COM4": {On: true, Brightness: 30, TempByte: 0},
	}}
	seed := map[string]light.SavedSetting{"target": entry, name42: entry}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "light-settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	storeHandler := light.NewMultiManager(testLogger(), dir, light.NewRegistry(""), func() ([]string, error) { return nil, nil }, nil)
	open := func() (Device, error) { return newFakeDevice(), nil }
	_, ask := startDaemonLight(t, open, storeHandler)

	// 1) The 100-spaces pad attack (Critical): 22 + 100 + 6 + 64 = 192
	// bytes, truncated by the 128-byte buffer - refused, NEVER dispatched.
	attack := "light settings delete " + strings.Repeat(" ", 100) + "target" + strings.Repeat("s", 64)
	if len(attack) <= 128 {
		t.Fatalf("test premise broken: attack is %d bytes, want > 128", len(attack))
	}
	if got, want := ask(attack), "error: command too long"; got != want {
		t.Fatalf("padded delete attack = %q, want %q", got, want)
	}
	// "target" MUST still exist: pre-fix the truncated head parsed as a
	// valid delete of the shortest name after the padding. ("b..."b < "t...")
	if got, want := ask("light settings list"), name42+"\ntarget"; got != want {
		t.Fatalf("list after the refused attack = %q, want %q (the padded delete must NOT have dispatched)", got, want)
	}

	// 2) An exactly-128-byte datagram: not even truncated, just hostile
	// size - the size rule refuses it too (nothing legal is 128 bytes).
	exact := "status" + strings.Repeat(" ", 122)
	if len(exact) != 128 {
		t.Fatalf("test premise broken: exact is %d bytes, want 128", len(exact))
	}
	if got, want := ask(exact), "error: command too long"; got != want {
		t.Fatalf("exactly-128B datagram = %q, want %q (a dispatched status would answer muted|unmuted|unknown)", got, want)
	}

	// 3) Legal sizes still work: the 64-byte delete (largest legal
	// command) deletes the 42-byte name for real...
	const deletePrefix = "light settings delete " // 22 bytes
	cmd64 := deletePrefix + name42
	if len(cmd64) != 64 {
		t.Fatalf("test premise broken: cmd64 is %d bytes, want 64", len(cmd64))
	}
	if got, want := ask(cmd64), `deleted "`+name42+`"`; got != want {
		t.Fatalf("64-byte delete = %q, want %q", got, want)
	}
	// ...and the 65-byte delete of a 43-byte name still arrives WHOLE and
	// draws the store's documented byte-cap error (R7-F3, kept green).
	cmd65 := deletePrefix + "target" + strings.Repeat("x", 37)
	if len(cmd65) != 65 {
		t.Fatalf("test premise broken: cmd65 is %d bytes, want 65", len(cmd65))
	}
	if got, want := ask(cmd65), "error: settings name too long (max 42 bytes)"; got != want {
		t.Fatalf("65-byte delete = %q, want %q", got, want)
	}
	if got, want := ask("light settings list"), "target"; got != want {
		t.Fatalf("list at the end = %q, want %q", got, want)
	}
}

// --- R8-F2: tracker staleness resets to unknown ---

// TestUndecodableDeviceMuteEventResetsTrackedState pins R8-F2a: a PHYSICAL
// 0x21 press whose value byte does not decode leaves the mic's true state
// unreadable, so the tracker drops to UNKNOWN (conditional verbs refuse
// with "flipped unknown" instead of acting on stale belief) - while the
// legacy F24 sweep STILL fires on that same press (the button's
// meeting-app behavior never depended on the value byte). A DECODABLE
// 0x21 press (the control) keeps tracking and injecting normally.
func TestUndecodableDeviceMuteEventResetsTrackedState(t *testing.T) {
	d := New(testLogger())
	inj := &fakeInjector{}
	d.Inject = inj
	d.Track.Set(true) // known muted: believable premise pre-reset
	d.handleEvent(proto.Event{Op: proto.EvtDeviceMute, Value: 0x0b})
	if _, known := d.Track.Status(); known {
		t.Fatal("an undecodable 0x21 value must reset the tracked state to unknown (premise safety, R8-F2)")
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injects = %d, want 1: the legacy F24 sweep still runs on an undecodable press", got)
	}
	if got := d.HandleCommand("mute-if muted"); got != "flipped unknown" {
		t.Fatalf("mute-if muted after the reset = %q, want %q (the stale belief must NOT drive the verb)", got, "flipped unknown")
	}

	// Control, fresh daemon: a decodable 0x21 press tracks AND injects.
	d2 := New(testLogger())
	inj2 := &fakeInjector{}
	d2.Inject = inj2
	d2.handleEvent(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01})
	if muted, known := d2.Track.Status(); !known || !muted {
		t.Fatalf("tracker after a decodable 0x21 = (%v, %v), want (true, true)", muted, known)
	}
	if got := inj2.calls.Load(); got != 1 {
		t.Fatalf("injects after the decodable press = %d, want 1", got)
	}
}

// TestReconnectResetsTrackedState pins R8-F2b end-to-end through Run's
// reconnect machinery: a fresh session/handshake must NOT inherit the
// dead session's tracked mute state (the mic may have been toggled while
// disconnected, and there is no state query), so after the new device
// binds, status is "unknown" and conditional verbs refuse with "flipped
// unknown" - until a real event re-establishes truth and the verbs act
// again.
func TestReconnectResetsTrackedState(t *testing.T) {
	dev1 := newFakeDevice()
	dev2 := newFakeDevice()
	var opens atomic.Int32
	open := func() (Device, error) {
		if opens.Add(1) == 1 {
			return dev1, nil
		}
		return dev2, nil
	}
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, open, inj)
	waitFor(t, "first handshake", func() bool { return dev1.writeCount() >= 2 })
	if got := ask("unmute"); got != "unmuted" {
		t.Fatalf("setup unmute = %q, want unmuted (establishes a known tracked state on session 1)", got)
	}

	dev1.readErr <- errors.New("device unplugged")
	// The reset happens when the NEW device binds, BEFORE its handshake
	// writes - so observing the handshake proves the reset already ran.
	// Reconnect delay is 2s; allow up to 5s.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && dev2.writeCount() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if dev2.writeCount() < 2 {
		t.Fatal("daemon did not reconnect and re-handshake after a read error")
	}
	if got := ask("status"); got != "unknown" {
		t.Fatalf("status after reconnect = %q, want unknown (the dead session's belief must be dropped)", got)
	}
	if got := ask("mute-if unmuted"); got != "flipped unknown" {
		t.Fatalf("mute-if unmuted after reconnect = %q, want %q (conditional verbs refuse on unknown)", got, "flipped unknown")
	}
	if got := inj.calls.Load(); got != 0 {
		t.Fatalf("injects = %d, want 0 (a refused conditional never injects)", got)
	}

	// A real event on the new session re-establishes truth and the
	// conditional verbs act again (one press sweep + one verb sweep).
	dev2.events <- inputReport(0x21, 0x01)
	waitFor(t, "status after press on the new session", func() bool { return ask("status") == "muted" })
	if got := ask("unmute-if muted"); got != "ok" {
		t.Fatalf("unmute-if muted after the press = %q, want ok", got)
	}
	waitFor(t, "press sweep + verb sweep", func() bool { return inj.calls.Load() == 2 })
}

// --- R9-F1: device bind + tracker reset are ONE atomic opMu compound ---

// TestConditionalVerbBlockedDuringBindRefusesUnknown pins the blocked-verb
// half of R9-F1: a conditional verb that arrives while Run's session bind
// holds opMu (device published AND tracker reset as one step) must park
// until the bind completes, then observe tracked-state UNKNOWN and refuse
// with "flipped unknown" - NEVER act on the NEW device under the OLD
// session's premise. The test is in-package so it can hold opMu across
// the locked bind body exactly like Run does (bindDeviceLocked).
func TestConditionalVerbBlockedDuringBindRefusesUnknown(t *testing.T) {
	d := New(testLogger())
	dev1, dev2 := newFakeDevice(), newFakeDevice()
	inj := &fakeInjector{}
	d.Inject = inj
	d.SetDevice(dev1)
	d.Track.Set(false) // old session's belief: known unmuted

	// Hold opMu exactly as Run's bind does and run the locked bind body:
	// dev2 is published and the tracker reset, atomically w.r.t. verbs.
	d.opMu.Lock()
	d.bindDeviceLocked(dev2)

	// A conditional verb arriving NOW parks on opMu (the bind is mid-step).
	replyCh := make(chan string, 1)
	go func() { replyCh <- d.HandleCommand("mute-if unmuted") }()
	select {
	case r := <-replyCh:
		t.Fatalf("conditional replied %q while the bind held opMu - it must park until the bind completes", r)
	case <-time.After(100 * time.Millisecond):
	}
	d.opMu.Unlock()

	if got := <-replyCh; got != "flipped unknown" {
		t.Fatalf("conditional after the bind = %q, want %q (the reset leaves no premise to act on)", got, "flipped unknown")
	}
	if got := dev2.writeCount(); got != 0 {
		t.Fatalf("dev2 writes = %d, want 0: a verb blocked on the bind must never act on the NEW device under the OLD premise", got)
	}
	if got := inj.calls.Load(); got != 0 {
		t.Fatalf("injects = %d, want 0 (a refused conditional never sweeps)", got)
	}
}

// TestConditionalVerbEntirelyBeforeBindUnaffected pins the other half of
// R9-F1: a conditional verb that completes BEFORE the bind is a serialized
// decision on the OLD device under the OLD premise - it runs its HID
// write and sweep exactly once, and the later bind neither undoes nor
// duplicates it; only AFTER the bind does the dropped belief make
// follow-up conditionals refuse.
func TestConditionalVerbEntirelyBeforeBindUnaffected(t *testing.T) {
	d := New(testLogger())
	dev1, dev2 := newFakeDevice(), newFakeDevice()
	inj := &fakeInjector{}
	d.Inject = inj
	d.SetDevice(dev1)
	d.Track.Set(false) // old session's belief: known unmuted

	// Entirely before the bind: the premise matches the old session's
	// belief, so the verb acts on the OLD device and sweeps once.
	if got := d.HandleCommand("mute-if unmuted"); got != "ok" {
		t.Fatalf("pre-bind conditional = %q, want ok", got)
	}
	if got := dev1.writeCount(); got != 1 {
		t.Fatalf("dev1 writes = %d, want 1 (the matched verb wrote the OLD device)", got)
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injects = %d, want 1 (one matched verb, one sweep)", got)
	}

	// The real public bind path: publishes dev2 AND drops the old belief in
	// one opMu step. A follow-up conditional premised on the dead session's
	// belief (muted) now refuses; the pre-bind verb's work is untouched.
	d.bindDevice(dev2)
	if got := d.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status after bind = %q, want unknown", got)
	}
	if got := d.HandleCommand("unmute-if muted"); got != "flipped unknown" {
		t.Fatalf("post-bind conditional premised on the old belief = %q, want %q", got, "flipped unknown")
	}
	if got := dev2.writeCount(); got != 0 {
		t.Fatalf("dev2 writes = %d, want 0", got)
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injects after the refused conditional = %d, want 1 (the pre-bind sweep stands; the refusal adds none)", got)
	}
}

func TestServeUDPSerializesDeltaBeforeNextDatagram(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	handler := &blockingLightHandler{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	d := New(testLogger())
	d.Light = handler
	done := make(chan struct{})
	go func() {
		d.serveUDP(pc)
		close(done)
	}()

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(handler.release) }) }
	defer func() {
		release()
		_ = pc.Close()
		<-done
	}()

	serverAddr := pc.LocalAddr().(*net.UDPAddr)
	first, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	_ = second.SetReadDeadline(time.Now().Add(time.Second))

	if _, err := first.Write([]byte("light brightness-delta 5")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.started:
	case <-time.After(time.Second):
		t.Fatal("delta handler did not start")
	}
	if _, err := second.Write([]byte("light status")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if got := handler.commandCount(); got != 1 {
		t.Fatalf("commands entered while delta was blocked = %d, want 1", got)
	}

	release()
	readReply := func(conn *net.UDPConn) string {
		buf := make([]byte, 128)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return string(buf[:n])
	}
	if got := readReply(first); got != "delta done" {
		t.Fatalf("delta reply = %q, want %q", got, "delta done")
	}
	if got := readReply(second); got != "status done" {
		t.Fatalf("second reply = %q, want %q", got, "status done")
	}
}
