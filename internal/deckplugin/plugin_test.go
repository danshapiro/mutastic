package deckplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// willAppearFrame is the verbatim wire shape for the real deck's
// lower-right key (device sd-X stands in for sd-A00DA6141I07PW).
const willAppearFrame = `{"event":"willAppear","action":"com.danshapiro.mutastic.mute","context":"sd-X.Default.Keypad.5.0","device":"sd-X","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`

// frameFor builds an event frame for an arbitrary context.
func frameFor(event, ctx string) []byte {
	return fmt.Appendf(nil, `{"event":%q,"action":"com.danshapiro.mutastic.mute","context":%q,"device":"sd-X","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`, event, ctx)
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// waitFor polls cond every 5ms for up to 2s (mirrors daemon_test.go).
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

// fakeConn implements Conn: writes are recorded; reads block on channels
// (mirrors daemon_test.go's fakeDevice shape).
type fakeConn struct {
	mu      sync.Mutex
	writes  [][]byte
	frames  chan []byte
	readErr chan error
}

func newFakeConn() *fakeConn {
	return &fakeConn{frames: make(chan []byte, 8), readErr: make(chan error, 1)}
}

func (f *fakeConn) ReadMessage() ([]byte, error) {
	select {
	case fr := <-f.frames:
		return fr, nil
	case err := <-f.readErr:
		return nil, err
	}
}

func (f *fakeConn) WriteMessage(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(data))
	copy(c, data)
	f.writes = append(f.writes, c)
	return nil
}

func (f *fakeConn) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeConn) write(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.writes[i])
}

// fakeDaemon implements DaemonClient with scripted replies per command.
type fakeDaemon struct {
	mu      sync.Mutex
	replies map[string]string
	err     error
	calls   []string
}

func (f *fakeDaemon) Command(cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	if f.err != nil {
		return "", f.err
	}
	return f.replies[cmd], nil
}

func (f *fakeDaemon) setReply(cmd, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[cmd] = reply
}

func (f *fakeDaemon) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeDaemon) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDaemon) call(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// fakeInjector mirrors internal/daemon/daemon_test.go's fakeInjector:
// counts calls, returns err.
type fakeInjector struct {
	calls atomic.Int32
	err   error
}

func (f *fakeInjector) Inject() error {
	f.calls.Add(1)
	return f.err
}

func TestDesiredState(t *testing.T) {
	tests := []struct {
		reply string
		state int
		ok    bool
	}{
		{"muted", stateMuted, true},
		{"unmuted", stateLive, true},
		{"unknown", 0, false},          // normal after daemon restart: keep current icon
		{"error: no device", 0, false}, // daemon error replies carry no state
		{"", 0, false},
	}
	for _, tt := range tests {
		st, ok := desiredState(tt.reply)
		if ok != tt.ok || (ok && st != tt.state) {
			t.Errorf("desiredState(%q) = (%d, %v), want (%d, %v)", tt.reply, st, ok, tt.state, tt.ok)
		}
	}
}

func TestWillAppearPushesKnownState(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	if got := conn.writeCount(); got != 1 {
		t.Fatalf("writes = %d, want 1 setState after willAppear", got)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(0); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestWillAppearUnknownStatusPushesNothing(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unknown"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0: unknown state must leave the icon alone", got)
	}
}

func TestSecondInstanceGetsStateOnAppear(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	p.HandleMessage(frameFor("willAppear", "sd-X.Other.Keypad.2.0"))
	if got := conn.writeCount(); got != 2 {
		t.Fatalf("writes = %d, want 2: each appearing instance gets the known state", got)
	}
	want := `{"event":"setState","context":"sd-X.Other.Keypad.2.0","payload":{"state":1}}`
	if got := conn.write(1); got != want {
		t.Fatalf("second frame = %s, want %s", got, want)
	}
}

func TestWillAppearChangedStatePushesToAllVisible(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame)) // A appears, establishes state 0
	base := conn.writeCount()

	// Daemon flips before the next poll tick; a new instance appears. The
	// change observed by the willAppear probe must reach A too, or the next
	// poll sees st == lastKnown and A stays stale until the NEXT change.
	fd.setReply("status", "muted")
	p.HandleMessage(frameFor("willAppear", "sd-X.Other.Keypad.2.0"))
	if got := conn.writeCount(); got != base+2 {
		t.Fatalf("writes = %d, want %d: state change seen on willAppear must push to every visible instance", got, base+2)
	}
	frames := map[string]bool{conn.write(base): true, conn.write(base + 1): true}
	wantA := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	wantC := `{"event":"setState","context":"sd-X.Other.Keypad.2.0","payload":{"state":1}}`
	if !frames[wantA] || !frames[wantC] {
		t.Fatalf("frames = %v, want both %s and %s", frames, wantA, wantC)
	}

	p.PollOnce() // still muted: the willAppear probe already recorded it
	if got := conn.writeCount(); got != base+2 {
		t.Fatalf("writes after unchanged poll = %d, want %d (no duplicate pushes)", got, base+2)
	}
}

func TestPollOncePushesOnlyOnChange(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame)) // pushes state 0
	base := conn.writeCount()

	p.PollOnce() // unchanged: no push
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes after unchanged poll = %d, want %d (setState persists the profile; only push on change)", got, base)
	}

	fd.setReply("status", "muted")
	p.PollOnce()
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after changed poll = %d, want %d", got, base+1)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}

	p.PollOnce() // still muted: no new push
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after second unchanged poll = %d, want %d", got, base+1)
	}
}

func TestPollOnceUnreachableDaemonLeavesIcon(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	base := conn.writeCount()
	fd.setErr(fmt.Errorf("no reply from daemon"))
	p.PollOnce()
	p.PollOnce()
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes = %d, want %d: unreachable daemon must not change the icon", got, base)
	}
}

func TestPollOnceSkipsWhenNothingVisible(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.PollOnce()
	if got := fd.callCount(); got != 0 {
		t.Fatalf("daemon calls = %d, want 0: no visible instance means no polling", got)
	}
}

func TestWillDisappearStopsPushes(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	p.HandleMessage(frameFor("willDisappear", "sd-X.Default.Keypad.5.0"))
	base := conn.writeCount()
	fd.setReply("status", "muted")
	p.PollOnce()
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes = %d, want %d: no visible instances, nothing to push", got, base)
	}
}

func TestUnknownEventsAreIgnored(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{}}
	p := New(conn, fd, nil, testLogger())
	// titleParametersDidChange follows every willAppear; garbage must not crash.
	p.HandleMessage([]byte(`{"event":"titleParametersDidChange","context":"sd-X.Default.Keypad.5.0","payload":{}}`))
	p.HandleMessage([]byte(`not json at all`))
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0", got)
	}
}

func TestKeyDownTogglesAndInjects(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted", "toggle": "muted"}}
	inj := &fakeInjector{}
	p := New(conn, fd, inj, testLogger())
	p.HandleMessage([]byte(willAppearFrame)) // establishes state 0
	base := conn.writeCount()

	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))

	if got := fd.call(fd.callCount() - 1); got != "toggle" {
		t.Fatalf("last daemon command = %q, want toggle", got)
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injections = %d, want exactly 1 F24 sweep per keyDown", got)
	}
	// The toggle reply IS the new state: the icon updates immediately,
	// without waiting for the next poll.
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes = %d, want %d (one setState from the toggle reply)", got, base+1)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestKeyDownInjectsEvenWhenDaemonDown(t *testing.T) {
	// mute-everything.cmd runs its two lines unconditionally (not &&);
	// the plugin mirrors that: a dead daemon must not stop the app sweep.
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{}}
	fd.setErr(errors.New("no reply from daemon"))
	inj := &fakeInjector{}
	p := New(conn, fd, inj, testLogger())
	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injections = %d, want 1 even with the daemon down", got)
	}
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0: no state to show", got)
	}
}

func TestKeyDownNilInjectorStillToggles(t *testing.T) {
	// Non-Windows: newKeyInjector() returns nil. The daemon toggle must
	// still run; only the F24 sweep is skipped.
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"toggle": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))
	if got := fd.callCount(); got != 1 || fd.call(0) != "toggle" {
		t.Fatalf("daemon calls = %d, want exactly one toggle call", got)
	}
}

func TestRunSendsRegisterFirst(t *testing.T) {
	conn := newFakeConn()
	p := New(conn, &fakeDaemon{replies: map[string]string{}}, nil, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, "registerPlugin", "com.danshapiro.mutastic.sdPlugin") }()

	waitFor(t, "register frame", func() bool { return conn.writeCount() >= 1 })
	want := `{"event":"registerPlugin","uuid":"com.danshapiro.mutastic.sdPlugin"}`
	if got := conn.write(0); got != want {
		t.Fatalf("first frame = %s, want %s (register MUST be the very first frame)", got, want)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run after cancel = nil, want context error")
	}
	conn.readErr <- errors.New("unblock the reader goroutine")
}

func TestRunReturnsNilOnSocketClose(t *testing.T) {
	conn := newFakeConn()
	p := New(conn, &fakeDaemon{replies: map[string]string{}}, nil, testLogger())
	conn.readErr <- errors.New("connection closed by OpenDeck")
	if err := p.Run(context.Background(), "registerPlugin", "x.sdPlugin"); err != nil {
		t.Fatalf("Run = %v, want nil: a closed socket is the normal end of life", err)
	}
}

func TestRunHandlesEventsAndPolls(t *testing.T) {
	old := PollInterval
	PollInterval = 5 * time.Millisecond
	t.Cleanup(func() { PollInterval = old })

	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background(), "registerPlugin", "com.danshapiro.mutastic.sdPlugin") }()

	conn.frames <- []byte(willAppearFrame)
	// write 0 = register, write 1 = setState(1) from willAppear.
	waitFor(t, "setState from willAppear", func() bool { return conn.writeCount() >= 2 })

	// Flip the daemon state out-of-band (models the physical mic button);
	// the ticker poll must observe it and push the change.
	fd.setReply("status", "unmuted")
	waitFor(t, "setState from poll", func() bool { return conn.writeCount() >= 3 })
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":0}}`
	if got := conn.write(2); got != want {
		t.Fatalf("poll frame = %s, want %s", got, want)
	}

	conn.readErr <- errors.New("closing")
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil on socket close", err)
	}
}

// TestLightAnyOn pins the light-reply -> state mapping against the
// daemon's REAL output strings (fixtures copied verbatim from
// internal/light/multi_test.go and main_test.go). ok=false means "no
// usable state: hold the current icon" — same contract as desiredState.
func TestLightAnyOn(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		state int
		ok    bool
	}{
		{"single on", "COM4: on 30% 2900K", stateLightsOn, true},
		{"single named on", "COM4 desk-right: on 30% 2900K", stateLightsOn, true},
		{"single off", "COM4: off", stateLightsOff, true},
		{"all off", "COM4: off\nCOM7: off", stateLightsOff, true},
		{"mixed off and on", "COM4: off\nCOM7: on 100% 4950K", stateLightsOn, true},
		{"all on", "COM4: on 50% 4950K\nCOM7: on 50% 4950K\nCOM12: on 50% 4950K", stateLightsOn, true},
		{"on plus wedged light", "COM4: on 40% 4950K\nCOM7: error: timeout", stateLightsOn, true},
		{"off plus wedged light", "COM4: off\nCOM7: error: timeout", stateLightsOff, true},
		{"off plus unknown counts as off", "COM4: off\nCOM7: unknown", stateLightsOff, true},
		{"zero lights attached", "error: no light", stateLightsOff, true},
		{"single unknown holds", "COM4: unknown", 0, false},
		{"all unknown holds", "COM4: unknown\nCOM7: unknown", 0, false},
		{"all wedged holds", "COM4: error: timeout", 0, false},
		{"no light support holds", "error: no light support", 0, false},
		{"unknown command holds", "error: unknown light command", 0, false},
		{"empty reply holds", "", 0, false},
		{"mic reply is not a light reply", "muted", 0, false},
	}
	for _, c := range cases {
		st, ok := lightAnyOn(c.reply)
		if ok != c.ok || (ok && st != c.state) {
			t.Errorf("%s: lightAnyOn(%q) = (%d, %v), want (%d, %v)", c.name, c.reply, st, ok, c.state, c.ok)
		}
	}
}
