package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestTrayStateFor(t *testing.T) {
	cases := []struct {
		reply string
		err   error
		want  trayState
	}{
		{"muted", nil, trayStateMuted},
		{"unmuted", nil, trayStateUnmuted},
		{"unknown", nil, trayStateUnknown},
		{"", errors.New("no reply"), trayStateDown},
		{"error: no device", nil, trayStateUnknown},
	}
	for _, c := range cases {
		if got := trayStateFor(c.reply, c.err); got != c.want {
			t.Errorf("trayStateFor(%q, %v) = %v, want %v", c.reply, c.err, got, c.want)
		}
	}
}

func TestTrayDisplayDecisions(t *testing.T) {
	if trayTitle(trayStateMuted) != "Mutastic — muted" {
		t.Errorf("muted title = %q", trayTitle(trayStateMuted))
	}
	if trayTitle(trayStateUnmuted) != "Mutastic — live" {
		t.Errorf("unmuted title = %q", trayTitle(trayStateUnmuted))
	}
	if trayTitle(trayStateUnknown) != "Mutastic — mic state unknown" {
		t.Errorf("unknown title = %q", trayTitle(trayStateUnknown))
	}
	if trayTitle(trayStateDown) != "Mutastic — daemon unreachable" {
		t.Errorf("down title = %q", trayTitle(trayStateDown))
	}
	// Log fields use compact state names, not menu display titles.
	for s, want := range map[trayState]string{
		trayStateMuted:   "muted",
		trayStateUnmuted: "unmuted",
		trayStateUnknown: "unknown",
		trayStateDown:    "down",
	} {
		if got := trayStateName(s); got != want {
			t.Errorf("trayStateName(%v) = %q, want %q", s, got, want)
		}
	}
	// The mic action item always displays the OPPOSITE of the last
	// definitive state - the click performs exactly the displayed action -
	// and falls back to the neutral "Mute/Unmute" while indefinite (a
	// disabled item still shows its last-set title; a stale directional
	// verb must never stay on screen).
	if got := trayMuteTitle(trayStateUnmuted); got != "Mute" {
		t.Errorf("mic item title at unmuted = %q, want %q (a click mutes a live mic)", got, "Mute")
	}
	if got := trayMuteTitle(trayStateMuted); got != "Unmute" {
		t.Errorf("mic item title at muted = %q, want %q (a click unmutes a muted mic)", got, "Unmute")
	}
	for _, s := range []trayState{trayStateUnknown, trayStateDown} {
		if got := trayMuteTitle(s); got != "Mute/Unmute" {
			t.Errorf("mic item title at state %v = %q, want neutral %q", s, got, "Mute/Unmute")
		}
	}
	if trayActionsEnabled(trayStateDown) {
		t.Error("actions must be disabled while the daemon is unreachable")
	}
	// Light actions arm on any daemon answer, including unknown (unknown is
	// a mic-state concept, not a reachability one).
	for _, s := range []trayState{trayStateMuted, trayStateUnmuted, trayStateUnknown} {
		if !trayActionsEnabled(s) {
			t.Errorf("actions must be enabled in state %v", s)
		}
	}
	// The mic action arms only on definitive answers; light actions arm on
	// any daemon answer including unknown.
	if trayMuteEnabled(trayStateUnknown) || trayMuteEnabled(trayStateDown) {
		t.Error("mic action must stay disabled at unknown/down (no premise to re-check, no safe verb to fire)")
	}
	if !trayMuteEnabled(trayStateMuted) || !trayMuteEnabled(trayStateUnmuted) {
		t.Error("mic action must be armed at definitive answers")
	}
}

// TestTrayIconOnlyChangesOnDefinitiveAnswers guards the at-a-glance truth
// property: while the daemon cannot report a definitive state, the tray
// must keep the last icon rather than risk painting a muted mic as live.
func TestTrayIconOnlyChangesOnDefinitiveAnswers(t *testing.T) {
	if got := trayIconFor(trayStateMuted); got != trayIconMutedMic {
		t.Errorf("muted icon decision = %v, want trayIconMutedMic", got)
	}
	if got := trayIconFor(trayStateUnmuted); got != trayIconLiveMic {
		t.Errorf("unmuted icon decision = %v, want trayIconLiveMic", got)
	}
	for _, s := range []trayState{trayStateUnknown, trayStateDown} {
		if got := trayIconFor(s); got != trayIconKeep {
			t.Errorf("state %v icon decision = %v, want trayIconKeep (never repaint as live on an indefinite answer)", s, got)
		}
	}
}

// traySpy records a handler's side-effect ORDER.
type traySpy struct {
	calls    []string
	askReply string
	askErr   error
	stopErr  error
	// script overrides askReply/askErr per command (handlers whose verb
	// depends on the armed premise, like muteClick's conditional verb).
	script map[string]scriptOutcome
}

type scriptOutcome struct {
	reply string
	err   error
}

func (s *traySpy) actions() *trayActions {
	return &trayActions{
		ask: func(cmd string) (string, error) {
			s.calls = append(s.calls, "ask:"+cmd)
			if o, ok := s.script[cmd]; ok {
				return o.reply, o.err
			}
			return s.askReply, s.askErr
		},
		openPanel:     func() error { s.calls = append(s.calls, "panel"); return nil },
		stopPanel:     func() error { s.calls = append(s.calls, "panelstop"); return s.stopErr },
		requestQuit:   func() { s.calls = append(s.calls, "quit") },
		signalRefresh: func() { s.calls = append(s.calls, "refresh") },
		logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func (s *traySpy) order() string { return strings.Join(s.calls, ",") }

// TestNewTrayJSONLoggerWritesJSONL pins the logging instruction for new
// repo code: one JSON object per line, with severity.
func TestNewTrayJSONLoggerWritesJSONL(t *testing.T) {
	var buf bytes.Buffer
	logger := newTrayJSONLogger(&buf)
	logger.Info("hello", "k", "v")
	logger.Error("bad", "err", "boom")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d: %q", len(lines), buf.String())
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not one JSON object: %v", err)
	}
	if first["level"] != "INFO" || first["msg"] != "hello" || first["k"] != "v" {
		t.Fatalf("line 1 = %v, want level/msg/k fields", first)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2 is not one JSON object: %v", err)
	}
	if second["level"] != "ERROR" || second["msg"] != "bad" {
		t.Fatalf("line 2 = %v, want ERROR severity", second)
	}
}

// TestStdlibLogBridgesToJSONL pins the production trick that keeps tray.log
// JSONL-only: installing our slog logger as the slog default routes the
// systray fork's plain stdlib log.Printf records through the same handler
// (the stdlib log->slog bridge), as JSON INFO lines instead of free text.
func TestStdlibLogBridgesToJSONL(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	var buf bytes.Buffer
	slog.SetDefault(newTrayJSONLogger(&buf))
	log.Printf("library-style line")
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("stdlib log line is not one JSON object: %v (%q)", err, buf.String())
	}
	if rec["msg"] != "library-style line" || rec["level"] != "INFO" {
		t.Fatalf("bridged record = %v, want INFO JSONL with the library message", rec)
	}
}

// TestTrayMuteConditionalVerb pins the premise -> conditional-verb mapping
// (R6-F2): the click's ONE daemon call names the ABSOLUTE action the label
// displays, premised on the state the label targets FROM - armed unmuted
// (label "Mute") asks "mute-if unmuted", armed muted (label "Unmute") asks
// "unmute-if muted". Indefinite premises have no truthful direction; they
// map to the mute-if form only as a placeholder - muteClick declines them
// BEFORE asking and never sends the result to the daemon.
func TestTrayMuteConditionalVerb(t *testing.T) {
	if got := trayMuteConditionalVerb(trayStateUnmuted); got != "mute-if unmuted" {
		t.Errorf("trayMuteConditionalVerb(unmuted) = %q, want %q", got, "mute-if unmuted")
	}
	if got := trayMuteConditionalVerb(trayStateMuted); got != "unmute-if muted" {
		t.Errorf("trayMuteConditionalVerb(muted) = %q, want %q", got, "unmute-if muted")
	}
}

// TestMuteClickRevalidates pins the action-time premise re-check of the
// dynamic Mute/Unmute item in its ATOMIC form (R6-F2): the click fires ONE
// conditional daemon verb (premise and action in a single serveUDP step -
// the separate probe->verb pair's hardware-event double-sweep window is
// gone) and the tray performs no sweep of its own (the daemon-side inject
// covers it; the old tray-side inject request is removed from this path).
// The R3-F2 FINAL RULE (superseding r2's sweep-on-flip ruling) carries
// over daemon-side: a "flipped <state>" reply means the label's target is
// already true (or unknown), and the flip's cause is unknowable per click
// - made by a sweeping path (physical/tray/deck, the frequent case) the
// apps were already carried and sweeping again would UNDO them; made by a
// mic-only path (panel/CLI) the apps were never moved. The refusal keeps
// the sweeping-path case correct, so a flip runs NO verb and NO sweep -
// the (b) app desync is the deliberate documented limitation with the
// manual resync as its recovery. Declines get one WARN (a failed
// click is an ERROR) and an immediate refresh so the redrawn truthful verb
// does not wait for the next poll.
func TestMuteClickRevalidates(t *testing.T) {
	armedMute := trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted}
	armedUnmute := trayMuteSnapshot{Title: "Unmute", Armed: trayStateMuted}
	load := func(snap trayMuteSnapshot) func() trayMuteSnapshot {
		return func() trayMuteSnapshot { return snap }
	}
	verifyOK := func(trayMuteSnapshot) bool { return true }

	// ok: the premise matched, the daemon ran the absolute verb AND the
	// F24 sweep in the same step. ONE ask + refresh, one INFO.
	matchingMute := &traySpy{script: map[string]scriptOutcome{
		"mute-if unmuted": {reply: "ok"},
	}}
	levelsMute := &levelRecorder{}
	aMute := matchingMute.actions()
	aMute.logger = slog.New(levelsMute)
	aMute.muteClick(load(armedMute), verifyOK)
	if got := matchingMute.order(); got != "ask:mute-if unmuted,refresh" {
		t.Fatalf("muteClick armed Mute with a matching premise = %q, want %q", got, "ask:mute-if unmuted,refresh")
	}
	if len(levelsMute.levels) != 1 || levelsMute.levels[0] != slog.LevelInfo {
		t.Fatalf("muteClick ok path levels = %v, want [INFO]", levelsMute.levels)
	}

	matchingUnmute := &traySpy{script: map[string]scriptOutcome{
		"unmute-if muted": {reply: "ok"},
	}}
	matchingUnmute.actions().muteClick(load(armedUnmute), verifyOK)
	if got := matchingUnmute.order(); got != "ask:unmute-if muted,refresh" {
		t.Fatalf("muteClick armed Unmute with a matching premise = %q, want %q", got, "ask:unmute-if muted,refresh")
	}

	// Flipped premises (R3-F2): the daemon found the mic already at the
	// label's TARGET state (or unknown), so NO mic verb fired and NO
	// sweep ran daemon-side. Spy order ask:<conditional>,refresh plus
	// exactly one WARN. Pinned for BOTH flip directions and the unknown
	// shape.
	flips := []struct {
		name    string
		snap    trayMuteSnapshot
		cmd     string
		outcome scriptOutcome
	}{
		{"flipped premise: armed Mute, daemon already muted (label's target already true)", armedMute, "mute-if unmuted", scriptOutcome{reply: "flipped muted"}},
		{"flipped premise: armed Unmute, daemon already unmuted (label's target already true)", armedUnmute, "unmute-if muted", scriptOutcome{reply: "flipped unmuted"}},
		{"unknown premise at the daemon (no truthful direction exists)", armedMute, "mute-if unmuted", scriptOutcome{reply: "flipped unknown"}},
	}
	for _, c := range flips {
		levels := &levelRecorder{}
		spy := &traySpy{script: map[string]scriptOutcome{c.cmd: c.outcome}}
		a := spy.actions()
		a.logger = slog.New(levels)
		a.muteClick(load(c.snap), verifyOK)
		if got := spy.order(); got != "ask:"+c.cmd+",refresh" {
			t.Fatalf("muteClick with %s = %q, want %q (one conditional ask, then refresh; nothing else ran)", c.name, got, "ask:"+c.cmd+",refresh")
		}
		if len(levels.levels) != 1 || levels.levels[0] != slog.LevelWarn {
			t.Fatalf("muteClick with %s levels = %v, want [WARN] (a flipped premise-check is not an error)", c.name, levels.levels)
		}
	}

	// Failure shapes - a dead daemon (transport error) or an
	// error:-prefixed reply (verb/inject failed daemon-side): one ask +
	// refresh, logged at ERROR.
	failures := []struct {
		name    string
		outcome scriptOutcome
	}{
		{"daemon down (transport error)", scriptOutcome{err: fmt.Errorf("%w: %w", errNoReply, syscall.ECONNREFUSED)}},
		{"daemon refused (error reply)", scriptOutcome{reply: "error: no device"}},
	}
	for _, c := range failures {
		levels := &levelRecorder{}
		spy := &traySpy{script: map[string]scriptOutcome{"mute-if unmuted": c.outcome}}
		a := spy.actions()
		a.logger = slog.New(levels)
		a.muteClick(load(armedMute), verifyOK)
		if got := spy.order(); got != "ask:mute-if unmuted,refresh" {
			t.Fatalf("muteClick with %s = %q, want %q (one conditional ask, then refresh)", c.name, got, "ask:mute-if unmuted,refresh")
		}
		if len(levels.levels) != 1 || levels.levels[0] != slog.LevelError {
			t.Fatalf("muteClick with %s levels = %v, want [ERROR]", c.name, levels.levels)
		}
	}

	// Declined clicks that never reach the daemon at all - an unarmed
	// premise (the neutral gray label; no truthful direction) and a failed
	// armed-verify (R6-F3: the live native row diverges from the loaded
	// snapshot; the click must never perform the opposite of what is on
	// screen). Spy order is refresh ONLY, plus exactly one WARN.
	levels := &levelRecorder{}
	spy := &traySpy{}
	a := spy.actions()
	a.logger = slog.New(levels)
	a.muteClick(load(trayMuteSnapshot{Title: "Mute/Unmute", Armed: trayStateUnknown}), verifyOK)
	if got := spy.order(); got != "refresh" {
		t.Fatalf("muteClick with an unarmed premise = %q, want %q (no daemon call at all)", got, "refresh")
	}
	if len(levels.levels) != 1 || levels.levels[0] != slog.LevelWarn {
		t.Fatalf("unarmed-premise click levels = %v, want [WARN]", levels.levels)
	}

	levels = &levelRecorder{}
	spy = &traySpy{}
	a = spy.actions()
	a.logger = slog.New(levels)
	a.muteClick(load(armedMute), func(trayMuteSnapshot) bool { return false })
	if got := spy.order(); got != "refresh" {
		t.Fatalf("muteClick with a failed armed-verify = %q, want %q (no daemon call at all - the display and the premise disagreed)", got, "refresh")
	}
	if len(levels.levels) != 1 || levels.levels[0] != slog.LevelWarn {
		t.Fatalf("armed-verify-declined click levels = %v, want [WARN]", levels.levels)
	}
}

// TestMuteClickSingleFlight pins the R2-F2 guard: clicks run on their own
// goroutines, and a second click arriving while one is still in flight -
// here, parked inside its conditional-verb call - is DROPPED with exactly
// one WARN, yielding ONE full spy sequence, not a doubled verb/sweep; and
// the guard releases at the end, so a later click runs the full sequence
// again.
func TestMuteClickSingleFlight(t *testing.T) {
	armedMute := trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted}
	load := func() trayMuteSnapshot { return armedMute }
	verifyOK := func(trayMuteSnapshot) bool { return true }
	spy := &traySpy{script: map[string]scriptOutcome{
		"mute-if unmuted": {reply: "ok"},
	}}
	levels := &levelRecorder{}
	a := spy.actions()
	a.logger = slog.New(levels)

	// The first conditional-verb call parks until released, so the second
	// click below provably enters muteClick while the first still holds
	// the guard.
	baseAsk := a.ask
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	a.ask = func(cmd string) (string, error) {
		if cmd == "mute-if unmuted" {
			once.Do(func() { close(entered); <-release })
		}
		return baseAsk(cmd)
	}

	first := make(chan struct{})
	go func() { a.muteClick(load, verifyOK); close(first) }()
	<-entered // the first click now holds the flight guard inside its ask
	second := make(chan struct{})
	go func() { a.muteClick(load, verifyOK); close(second) }()
	<-second // dropped with one WARN; it must never reach the spy
	if got := spy.order(); got != "" {
		t.Fatalf("while the first click is in flight the spy = %q, want no recorded calls yet", got)
	}
	close(release)
	<-first
	want := "ask:mute-if unmuted,refresh"
	if got := spy.order(); got != want {
		t.Fatalf("two overlapping clicks = %q, want exactly ONE full sequence %q", got, want)
	}
	// Exactly one record is the dropped click's WARN (it is logged while the
	// first click is parked, so it lands FIRST); the second is the first
	// click's own success INFO - no second verb, no doubled sweep.
	if len(levels.levels) != 2 || levels.levels[0] != slog.LevelWarn || levels.levels[1] != slog.LevelInfo {
		t.Fatalf("levels = %v, want [WARN INFO] (the dropped click, then the in-flight click's success)", levels.levels)
	}

	// The guard releases: a later click runs the full sequence again.
	a.muteClick(load, verifyOK)
	if got := spy.order(); got != want+","+want {
		t.Fatalf("after the in-flight click released, a later click = %q, want the full sequence twice", got)
	}
}

// TestMuteClickLoadsSnapshotOnce pins the F7 contract: the click loads the
// {title, armed} snapshot EXACTLY ONCE - the displayed verb and the premise
// the click's conditional verb is premised on are a single read, never a
// mixture of a fresh title with a stale premise.
func TestMuteClickLoadsSnapshotOnce(t *testing.T) {
	loads := 0
	load := func() trayMuteSnapshot {
		loads++
		return trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted}
	}
	spy := &traySpy{script: map[string]scriptOutcome{
		"mute-if unmuted": {reply: "ok"},
	}}
	spy.actions().muteClick(load, func(trayMuteSnapshot) bool { return true })
	if got := spy.order(); got != "ask:mute-if unmuted,refresh" {
		t.Fatalf("muteClick with a matching premise = %q, want %q", got, "ask:mute-if unmuted,refresh")
	}
	if loads != 1 {
		t.Fatalf("snapshot loads per click = %d, want exactly 1", loads)
	}
}

// TestTrayQuitStopsEverythingThenQuits pins the Quit cascade: daemon
// shutdown AND light-panel shutdown, then the tray exits - but ONLY when
// each stop is confirmed or already the goal state. An unreachable daemon
// already satisfies "stopped"; a live daemon or panel that REFUSES to stop
// keeps the tray alive (the display refreshes, the next Quit retries).
func TestTrayQuitStopsEverythingThenQuits(t *testing.T) {
	spy := &traySpy{askReply: "shutting down"}
	spy.actions().onQuit()
	if got := spy.order(); got != "ask:shutdown,panelstop,quit" {
		t.Fatalf("onQuit side effects = %q, want %q", got, "ask:shutdown,panelstop,quit")
	}

	// A refused daemon port proves the daemon is gone: goal state.
	refused := &traySpy{askErr: fmt.Errorf("%w: %w", errNoReply, syscall.ECONNREFUSED)}
	refused.actions().onQuit()
	if got := refused.order(); got != "ask:shutdown,panelstop,quit" {
		t.Fatalf("onQuit with a refused daemon port = %q, want %q", got, "ask:shutdown,panelstop,quit")
	}

	// The Windows production shape: a raw Winsock code in the chain.
	wsaRefused := &traySpy{askErr: fmt.Errorf("%w: %w", errNoReply, syscall.Errno(10061))}
	wsaRefused.actions().onQuit()
	if got := wsaRefused.order(); got != "ask:shutdown,panelstop,quit" {
		t.Fatalf("onQuit with a Winsock-refused daemon = %q, want %q", got, "ask:shutdown,panelstop,quit")
	}

	// A timeout says nothing: the daemon may be live and wedged. The tray
	// must stay so the user can retry.
	timedOut := &traySpy{askErr: fmt.Errorf("%w", errNoReply)}
	timedOut.actions().onQuit()
	if got := timedOut.order(); got != "ask:shutdown,panelstop,refresh" {
		t.Fatalf("onQuit with a timed-out daemon = %q, want %q (unconfirmed means keep the tray)", got, "ask:shutdown,panelstop,refresh")
	}

	// A daemon that REFUSES (live, answers with an error) also keeps the tray.
	refusingDaemon := &traySpy{askReply: "error: x"}
	refusingDaemon.actions().onQuit()
	if got := refusingDaemon.order(); got != "ask:shutdown,panelstop,refresh" {
		t.Fatalf("onQuit with a refusing daemon = %q, want %q", got, "ask:shutdown,panelstop,refresh")
	}

	refusingPanel := &traySpy{askReply: "shutting down", stopErr: errors.New("panel refused")}
	refusingPanel.actions().onQuit()
	if got := refusingPanel.order(); got != "ask:shutdown,panelstop,refresh" {
		t.Fatalf("onQuit with a refusing panel = %q, want %q (a live panel that refused keeps the tray; the retry refreshes the display)", got, "ask:shutdown,panelstop,refresh")
	}
}

func TestStopLightPanel(t *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeUIJSON(w, http.StatusOK, struct {
			OK bool `json:"ok"`
		}{OK: true})
	}))
	defer server.Close()
	if err := stopLightPanel(server.URL + "/"); err != nil {
		t.Fatalf("stopLightPanel = %v, want nil", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/shutdown" {
		t.Fatalf("panel received %s %s, want POST /api/shutdown", gotMethod, gotPath)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeUIJSON(w, http.StatusMethodNotAllowed, uiResponse{Error: "method not allowed"})
	}))
	defer refusing.Close()
	if err := stopLightPanel(refusing.URL + "/"); err == nil || !strings.Contains(err.Error(), "405") {
		t.Fatalf("stopLightPanel against a refusing panel = %v, want a status error", err)
	}

	// Unreachable panel (dead listener): the goal state of Quit already
	// holds, so this is NOT an error.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + listener.Addr().String() + "/"
	_ = listener.Close()
	if err := stopLightPanel(dead); err != nil {
		t.Fatalf("stopLightPanel against a dead panel = %v, want nil (already the goal state)", err)
	}

	// Wedged panel: it accepts and hangs past the client timeout, so the
	// transport error is NOT ECONNREFUSED and must surface as an error.
	// The same goes for a reset: a live wedged listener can RST an accepted
	// connection, so a reset/timeout stays an error (only refusal counts
	// as goal state).
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer wedged.Close()
	err = stopLightPanel(wedged.URL + "/")
	if err == nil {
		t.Fatal("stopLightPanel against a wedged panel = nil, want a transport error (only ECONNREFUSED counts as goal state)")
	}
}

// TestNoListenerClassificationByTransport pins the per-transport "nothing
// is listening" classification. For the daemon's UDP port, an unheard
// datagram earns an ICMP port-unreachable which surfaces as ECONNRESET on
// Windows and ECONNREFUSED on Linux, so BOTH prove "no listener". For the
// panel's TCP listener, only a refusal proves the port is closed: a RST
// can be produced by a LIVE, wedged listener. Both the unix errno shape
// and the raw Winsock code Windows surfaces in production must count,
// while a bare timeout sentinel must not.
func TestNoListenerClassificationByTransport(t *testing.T) {
	wrap := func(e syscall.Errno) error { return fmt.Errorf("%w: %w", errNoReply, e) }
	// UDP (daemon): refused OR reset both prove "no listener".
	if !udpNoListener(wrap(syscall.ECONNREFUSED)) || !udpNoListener(wrap(syscall.ECONNRESET)) ||
		!udpNoListener(wrap(syscall.Errno(10061))) || !udpNoListener(wrap(syscall.Errno(10054))) {
		t.Fatal("UDP no-listener must cover refused+reset on both errno shapes")
	}
	// TCP (panel): only refusal proves "no listener"; a reset can come from
	// a live wedged listener.
	if !tcpNoListener(wrap(syscall.ECONNREFUSED)) || !tcpNoListener(wrap(syscall.Errno(10061))) {
		t.Fatal("TCP no-listener must cover refusals on both errno shapes")
	}
	if tcpNoListener(wrap(syscall.ECONNRESET)) || tcpNoListener(wrap(syscall.Errno(10054))) {
		t.Fatal("TCP reset must NOT classify as no-listener (a wedged live panel can RST)")
	}
	// The bare timeout sentinel classifies as nothing in both.
	if udpNoListener(fmt.Errorf("%w", errNoReply)) || tcpNoListener(fmt.Errorf("%w", errNoReply)) {
		t.Fatal("bare timeouts are unconfirmed, not unreachable")
	}
}

// TestTrayOpenPanelBringsUpUI pins the left-click behavior (the other
// explicit requirement).
func TestTrayOpenPanelBringsUpUI(t *testing.T) {
	spy := &traySpy{}
	spy.actions().onOpenPanel()
	if got := spy.order(); got != "panel" {
		t.Fatalf("onOpenPanel side effects = %q, want %q", got, "panel")
	}
}

// levelRecorder captures slog levels for handler tests.
type levelRecorder struct {
	levels []slog.Level
}

func (r *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *levelRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.levels = append(r.levels, rec.Level)
	return nil
}
func (r *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *levelRecorder) WithGroup(string) slog.Handler      { return r }

// TestLogSeverityClassifiesFailures pins ERROR for failed daemon actions —
// including the per-line fleet shape the old prefix check missed and the
// conditional mute verb's transport/reply failure shapes.
func TestLogSeverityClassifiesFailures(t *testing.T) {
	levels := &levelRecorder{}
	a := &trayActions{
		ask: func(cmd string) (string, error) {
			if cmd == "light toggle" {
				return "COM4 desk: on 30% 2900K\nCOM7: error: timeout", nil
			}
			return "", errors.New("x")
		},
		openPanel:     func() error { return nil },
		stopPanel:     func() error { return nil },
		requestQuit:   func() {},
		signalRefresh: func() {},
		logger:        slog.New(levels),
	}
	load := func() trayMuteSnapshot { return trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted} }
	a.muteClick(load, func(trayMuteSnapshot) bool { return true })
	a.onLight("light toggle")
	if len(levels.levels) != 2 || levels.levels[0] != slog.LevelError || levels.levels[1] != slog.LevelError {
		t.Fatalf("levels = %v, want [ERROR ERROR] (mute-click error + per-line fleet error)", levels.levels)
	}
}

// TestTraySavedSettingsMenuSpec pins the three rendering regimes of the
// "Saved settings" submenu: a not-ok poll (transport down OR store broken -
// one disabled "(settings unavailable)" placeholder covers both honestly),
// an ok-but-empty store ("(no saved settings)"), and one ENABLED item per
// saved name in the reply's input order. Both placeholders are tray-side UI
// text the daemon never sends.
func TestTraySavedSettingsMenuSpec(t *testing.T) {
	down := traySavedSettings(nil, false)
	if want := []trayMenuSpec{{Title: "(settings unavailable)", Enabled: false}}; !reflect.DeepEqual(down, want) {
		t.Errorf("traySavedSettings(nil, false) = %+v, want %+v", down, want)
	}
	for _, names := range [][]string{nil, {}} {
		want := []trayMenuSpec{{Title: "(no saved settings)", Enabled: false}}
		if got := traySavedSettings(names, true); !reflect.DeepEqual(got, want) {
			t.Errorf("traySavedSettings(%v, true) = %+v, want %+v", names, got, want)
		}
	}
	saved := traySavedSettings([]string{"work", "movie mode"}, true)
	wantSaved := []trayMenuSpec{{Title: "work", raw: "work", Enabled: true}, {Title: "movie mode", raw: "movie mode", Enabled: true}}
	if !reflect.DeepEqual(saved, wantSaved) {
		t.Errorf("traySavedSettings([work movie mode], true) = %+v, want %+v (input order PRESERVED - the pair is deliberately non-sorted so a sorting mutation fails this)", saved, wantSaved)
	}
	// A "&" in a name must NOT mangle on screen: the Title escapes "&" as
	// "&&" (Windows' mnemonic marker) while raw keeps the VERBATIM name -
	// the glue builds the apply command from raw by index (never from the
	// escaped Title), and both placeholder strings contain no "&".
	esc := traySavedSettings([]string{"A&B", "R&D dept"}, true)
	wantEsc := []trayMenuSpec{{Title: "A&&B", raw: "A&B", Enabled: true}, {Title: "R&&D dept", raw: "R&D dept", Enabled: true}}
	if !reflect.DeepEqual(esc, wantEsc) {
		t.Errorf("traySavedSettings([A&B R&D dept], true) = %+v, want %+v (escaped Title zipped with the verbatim raw name by index)", esc, wantEsc)
	}
}

// TestTrayParseSettingsList pins the "light settings list" wire contract as
// a parse: "" is the daemon's contract for "none saved" - (no names,
// daemonOK) - NOT an error and NOT unreachable; newline-joined names split
// in order; ANY ask error marks the poll not-ok; and an error:-prefixed
// reply (the disabled/corrupt store's single-line refusal) is treated as
// NOT-OK so refusal text can never render as an enabled menu item whose
// click always fails (LB-4).
func TestTrayParseSettingsList(t *testing.T) {
	names, ok := trayParseSettingsList("", nil)
	if !ok || len(names) != 0 {
		t.Errorf("trayParseSettingsList(empty, nil) = (%v, %v), want (empty, true) - the empty reply is the wire contract for none saved", names, ok)
	}
	names, ok = trayParseSettingsList("movie mode\nwork", nil)
	if !ok || !reflect.DeepEqual(names, []string{"movie mode", "work"}) {
		t.Errorf("trayParseSettingsList(names, nil) = (%v, %v), want ([movie mode work], true)", names, ok)
	}
	names, ok = trayParseSettingsList("", errors.New("no reply"))
	if ok || names != nil {
		t.Errorf("trayParseSettingsList(_, err) = (%v, %v), want (nil, false) - any ask error is not-ok", names, ok)
	}
	for _, refusal := range []string{"error: settings persistence disabled", "error: settings store corrupt or unreadable: C:\\x"} {
		names, ok = trayParseSettingsList(refusal, nil)
		if ok || names != nil {
			t.Errorf("trayParseSettingsList(%q, nil) = (%v, %v), want (nil, false) - a refusal renders as the unavailable placeholder, never as enabled items", refusal, names, ok)
		}
	}
}

// TestTraySameMenuSpecs pins the change gate on the submenu rebuild:
// {Title, Enabled} PAIRS are compared element-wise, not titles only - the
// settings-name grammar permits literal saved names equal to the
// placeholder strings, so a placeholder<->real-item transition with an
// IDENTICAL title must still compare different and force the rebuild
// (otherwise a real setting stays grayed or a placeholder stays clickable).
func TestTraySameMenuSpecs(t *testing.T) {
	if !traySameMenuSpecs(nil, nil) {
		t.Error("two nils must compare equal (the state before the first poll)")
	}
	saved := []trayMenuSpec{{Title: "a", Enabled: true}, {Title: "b", Enabled: true}}
	same := []trayMenuSpec{{Title: "a", Enabled: true}, {Title: "b", Enabled: true}}
	if !traySameMenuSpecs(saved, same) {
		t.Error("equal lists must compare equal (no rebuild churn in the steady state)")
	}
	if traySameMenuSpecs(saved, saved[:1]) {
		t.Error("a length change must trigger a rebuild")
	}
	titleChange := []trayMenuSpec{{Title: "a", Enabled: true}, {Title: "c", Enabled: true}}
	if traySameMenuSpecs(saved, titleChange) {
		t.Error("a title change must trigger a rebuild")
	}
	placeholder := []trayMenuSpec{{Title: "(settings unavailable)", Enabled: false}}
	real := []trayMenuSpec{{Title: "(settings unavailable)", raw: "(settings unavailable)", Enabled: true}}
	if traySameMenuSpecs(placeholder, real) || traySameMenuSpecs(real, placeholder) {
		t.Error("placeholder<->real with identical titles must compare DIFFERENT via the enabled bit (the pair differs, so the menu rebuilds)")
	}
	// The raw field participates in whole-struct equality: an escaped
	// Title glued to the WRONG raw name (a glue zip bug) must never
	// compare equal to the correct pairing.
	if traySameMenuSpecs([]trayMenuSpec{{Title: "A&&B", raw: "A&B", Enabled: true}}, []trayMenuSpec{{Title: "A&&B", raw: "A&&B", Enabled: true}}) {
		t.Error("a raw-name mismatch behind an identical escaped Title must compare DIFFERENT (the click command uses raw)")
	}
}

// TestTraySettingsCmdMatchesTitle pins the pure comparator behind the
// ROUND-6 click-time native title check: the command slot carries the
// VERBATIM raw name ("light settings apply <name>") and the native title
// carries the menu-ESCAPED display form ("&" -> "&&"), so a match
// requires exact equality after re-escaping the raw name. The mismatched
// case is the reassigned-row race the check exists to refuse: a WM_COMMAND
// dispatched from a stale native row whose slot now names a DIFFERENT
// setting must not execute. Malformed commands (missing prefix, empty
// name, wrong verb, empty everything) never match: refusal is the safe
// direction.
func TestTraySettingsCmdMatchesTitle(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string
		native string
		want   bool
	}{
		{"plain match", "light settings apply focus", "focus", true},
		{"ampersand name vs escaped title", "light settings apply fish & chips", "fish && chips", true},
		{"unescaped native title must not match", "light settings apply fish & chips", "fish & chips", false},
		{"double ampersand name", "light settings apply a && b", "a &&&& b", true},
		{"reassigned row (the race the check refuses)", "light settings apply dark", "party", false},
		{"prefix-of-another name", "light settings apply tom", "tomato", false},
		{"malformed: missing arg after prefix", "light settings apply ", "x", false},
		{"malformed: prefix without trailing space", "light settings apply", "apply", false},
		{"malformed: wrong verb", "light toggle", "toggle", false},
		{"malformed: empty command", "", "", false},
		{"native title empty against real command", "light settings apply focus", "", false},
	}
	for _, c := range cases {
		if got := traySettingsCmdMatchesTitle(c.cmd, c.native); got != c.want {
			t.Errorf("%s: traySettingsCmdMatchesTitle(%q, %q) = %v, want %v", c.name, c.cmd, c.native, got, c.want)
		}
	}
}

// TestTraySetSettingsClick pins the stable settings closure's semantics on
// the portable path (the tray gate runs Linux-only; the Windows arm's
// native read-back is review-verified): an UNBOUND slot ("" - a
// placeholder or hidden pool orphan) dispatches nothing, and a bound slot
// dispatches exactly its command. The portable default
// trayVerifySettingsItemTitle always verifies true, which the bound case
// pins transitively - and pins directly below so a regression on the
// non-Windows path cannot silently drop every settings click.
func TestTraySetSettingsClick(t *testing.T) {
	if !trayVerifySettingsItemTitle(nil, "light settings apply focus") {
		t.Fatal("portable trayVerifySettingsItemTitle default must always verify true (this test runs on the non-Windows path)")
	}
	var slot atomic.Value
	var fired []string
	lightCmd := func(c string) func() { return func() { fired = append(fired, c) } }
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	click := traySetSettingsClick(nil, &slot, lightCmd, logger)
	click() // unbound slot: a placeholder/pool-orphan click is a no-op
	if len(fired) != 0 {
		t.Fatalf("unbound slot dispatched %v, want nothing", fired)
	}
	slot.Store("light settings apply focus")
	click()
	if len(fired) != 1 || fired[0] != "light settings apply focus" {
		t.Fatalf("bound slot dispatched %v, want exactly [light settings apply focus]", fired)
	}
	// Stale-click pin (R7-F2): on any name-set change EVERY old row is
	// retired - slot cleared, disabled, hidden - so a click dispatched
	// from the pre-rebuild display (the fork dispatches by immutable
	// command ID) loads the cleared "" slot and no-ops: the stale row can
	// never apply anything.
	slot.Store("") // the retirement hygiene's first step
	click()
	if len(fired) != 1 {
		t.Fatalf("stale click on a retired (cleared-slot) row dispatched %v, want no new dispatch", fired)
	}
	// The closure still reads the slot at click time (R2-F3): a slot
	// stored at CREATION (R7-F2 rows are never re-bound after creation -
	// a changed set renders fresh rows) dispatches exactly that command.
	slot.Store("light settings apply party")
	click()
	if len(fired) != 2 || fired[1] != "light settings apply party" {
		t.Fatalf("creation-stored slot dispatched %v, want the stored command last", fired)
	}
}

// TestTrayArmedMatchesNative pins the pure comparison behind trayVerifyArmed
// (R6-F3): the armed premise may be stored (refresh loop) or fired (click)
// ONLY when the live native row reads back exactly the snapshot's painted
// title AND its disabled bit is the inverse of the snapshot's arming - an
// armed snap needs a natively ENABLED row, a disarmed snap a natively
// DISABLED one. Every divergence refuses (the safe direction).
func TestTrayArmedMatchesNative(t *testing.T) {
	armedMute := trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted}
	armedUnmute := trayMuteSnapshot{Title: "Unmute", Armed: trayStateMuted}
	neutral := trayMuteSnapshot{Title: "Mute/Unmute", Armed: trayStateUnknown}
	cases := []struct {
		name           string
		snap           trayMuteSnapshot
		title          string
		nativeDisabled bool
		want           bool
	}{
		{"armed Mute verified", armedMute, "Mute", false, true},
		{"armed Unmute verified", armedUnmute, "Unmute", false, true},
		{"neutral snap verified (natively disabled)", neutral, "Mute/Unmute", true, true},
		{"native SetTitle failed: stale title behind a fresh snap", armedMute, "Unmute", false, false}, // the R6-F3 hazard: visible verb is the OPPOSITE of the armed premise
		{"native SetTitle failed: neutral title behind an armed snap", armedMute, "Mute/Unmute", false, false},
		{"native Enable failed: row stayed disabled behind an armed snap", armedMute, "Mute", true, false},
		{"native Disable failed: row stayed enabled behind a neutral snap", neutral, "Mute/Unmute", false, false},
		{"empty native title never matches an armed snap", armedMute, "", false, false},
	}
	for _, c := range cases {
		if got := trayArmedMatchesNative(c.snap, c.title, c.nativeDisabled); got != c.want {
			t.Errorf("%s: trayArmedMatchesNative(%+v, %q, %v) = %v, want %v", c.name, c.snap, c.title, c.nativeDisabled, got, c.want)
		}
	}
}

// TestTrayPlanSettingsSync pins the pure full-rebuild reconcile (R7-F2):
// an identical set is a NO-OP (identical ticks skip - no churn, no
// retires); ANY difference retires EVERY cached row and renders the
// wanted list FRESH in exact daemon order. There is deliberately no
// retired-pool input: retired rows are never reused, because the fork
// displays rows sorted by their IMMUTABLE command ID and a reused row's
// old ID could never sort into the new daemon order - only fresh rows
// created in daemon order display in daemon order.
func TestTrayPlanSettingsSync(t *testing.T) {
	spec := func(title, raw string, enabled bool) trayMenuSpec {
		return trayMenuSpec{Title: title, raw: raw, Enabled: enabled}
	}
	realA := spec("a", "a", true)
	realB := spec("b", "b", true)
	realC := spec("c", "c", true)
	realX := spec("x", "x", true)
	placeholder := spec(trayNoSavedSettingsTitle, "", false)

	t.Run("steady state is a no-op: identical ticks skip everything", func(t *testing.T) {
		for _, same := range [][2][]trayMenuSpec{
			{nil, nil}, // before the first poll difference
			{{realA, realB}, {realA, realB}},
		} {
			plan := trayPlanSettingsSync(same[0], same[1])
			if plan.changed {
				t.Errorf("changed = true, want false (identical ticks no-op - the steady-state poll must never churn native rows)")
			}
			if len(plan.retires) != 0 || len(plan.fresh) != 0 {
				t.Errorf("retires/fresh = %v/%v, want both empty (nothing leaves, nothing is created)", plan.retires, plan.fresh)
			}
		}
	})

	// Every change class behaves identically: retire ALL, render FRESH.
	// Order-sensitivity is pinned by "pure reorder" and "insert kept names"
	// (the ROUND-7 keep-the-identical-rows scheme could not reorder; the
	// full rebuild can, because fresh IDs track the new list position).
	for _, c := range []struct {
		name         string
		cached, want []trayMenuSpec
	}{
		{"rename at the same position", []trayMenuSpec{realA}, []trayMenuSpec{realB}},
		{"removal beyond the new length retires the whole old set too", []trayMenuSpec{realA, realB, realC}, []trayMenuSpec{realA}},
		{"insert mid-list: kept names retire with the rest", []trayMenuSpec{realA, realB, realC}, []trayMenuSpec{realA, realX, realC}},
		{"pure reorder of an identical name set", []trayMenuSpec{realA, realB}, []trayMenuSpec{realB, realA}},
		{"placeholder-to-real with an identical title", []trayMenuSpec{placeholder}, []trayMenuSpec{spec(trayNoSavedSettingsTitle, trayNoSavedSettingsTitle, true)}},
		{"real-to-placeholder with an identical title", []trayMenuSpec{spec(trayNoSavedSettingsTitle, trayNoSavedSettingsTitle, true)}, []trayMenuSpec{placeholder}},
		{"everything vanishes (store corrupted mid-poll)", []trayMenuSpec{realA, realB}, []trayMenuSpec{spec(traySettingsUnavailableTitle, "", false)}},
		{"first render from empty", nil, []trayMenuSpec{realA}},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := trayPlanSettingsSync(c.cached, c.want)
			if !plan.changed {
				t.Fatal("changed = false, want true (any whole-spec difference forces the full rebuild)")
			}
			wantRetires := make([]int, len(c.cached))
			for i := range c.cached {
				wantRetires[i] = i
			}
			if !reflect.DeepEqual(plan.retires, wantRetires) {
				t.Errorf("retires = %v, want %v (EVERY current row retires - the executor then does the click-safety hygiene on each: slot cleared, placeholder retitle, disabled, hidden)", plan.retires, wantRetires)
			}
			if !reflect.DeepEqual(plan.fresh, c.want) {
				t.Errorf("fresh = %+v, want the wanted list VERBATIM in daemon order %+v (the executor creates fresh rows in exactly this order)", plan.fresh, c.want)
			}
			// The fresh list must be executable in order and produce that
			// order on screen: fresh immutable IDs ascend with creation.
			for i := 1; i < len(plan.fresh); i++ {
				if plan.fresh[i] == plan.fresh[i-1] && plan.fresh[i] != c.want[i] {
					t.Fatalf("fresh drifted from want at %d (the whole point is exact daemon order)", i)
				}
			}
		})
	}

	t.Run("removal to empty retire to the placeholder render", func(t *testing.T) {
		plan := trayPlanSettingsSync([]trayMenuSpec{realA, realB}, []trayMenuSpec{placeholder})
		if !plan.changed || !reflect.DeepEqual(plan.retires, []int{0, 1}) {
			t.Fatalf("changed/retires = %v/%v, want true/[0 1] (both names deleted: the one placeholder row renders fresh)", plan.changed, plan.retires)
		}
		// A CHANGE to an empty want list (impossible from traySavedSettings,
		// which always renders at least a placeholder) still plans cleanly.
		plan = trayPlanSettingsSync([]trayMenuSpec{realA}, nil)
		if !plan.changed || !reflect.DeepEqual(plan.retires, []int{0}) || len(plan.fresh) != 0 {
			t.Fatalf("changed/retires/fresh = %v/%v/%v, want true/[0]/empty", plan.changed, plan.retires, plan.fresh)
		}
	})
}
