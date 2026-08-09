package daemon

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
	return startDaemonInject(t, open, nil)
}

// startDaemonInject is startDaemon with a KeyInjector wired into Run.
// It is startDaemon's previous body moved verbatim; the ONLY change is
// the added inject argument in the Run call.
func startDaemonInject(t *testing.T, open OpenFunc, inject KeyInjector) (addr string, ask func(cmd string) string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, open, nil, inject, pc, testLogger())
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
