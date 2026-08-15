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
	injErr   error
	stopErr  error
	// script overrides askReply/askErr per command (handlers that ask more
	// than one distinct command, like muteClick's status probe + verb).
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
		injectSweep:   func() error { s.calls = append(s.calls, "inject"); return s.injErr },
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

// TestTrayMicToggleIsMuteEverything mirrors the Stream Deck mute key: the
// ABSOLUTE daemon verb the label displays AND the F24 meeting-app sweep,
// then a refresh - and the sweep must fire even when the daemon round trip
// fails (a verb-only path would move the mic while leaving meeting apps
// live).
func TestTrayMicToggleIsMuteEverything(t *testing.T) {
	for _, verb := range []string{"mute", "unmute"} {
		want := "ask:" + verb + ",inject,refresh"

		spy := &traySpy{}
		spy.actions().onMicSet(verb)
		if got := spy.order(); got != want {
			t.Fatalf("onMicSet(%q) side effects = %q, want %q", verb, got, want)
		}

		failing := &traySpy{askErr: errors.New("daemon dead")}
		failing.actions().onMicSet(verb)
		if got := failing.order(); got != want {
			t.Fatalf("onMicSet(%q) with a dead daemon = %q, want %q (the sweep must still fire)", verb, got, want)
		}
	}
}

// TestMuteClickRevalidates pins the action-time premise re-check of the
// dynamic Mute/Unmute item. The item's title and enabled bit are only the
// last completed poll's snapshot, and a click fires the ABSOLUTE verb the
// displayed label names - so before asking for anything, the click re-probes
// the daemon and fires ONLY when the probe reproduces the snapshot's armed
// premise (the state the label's verb targets). A definitive-OPPOSITE
// probe means the label's target is already true (flipped premise), and an
// unknown or dead-daemon probe means no premise at all: each declines with
// exactly one WARN, no mic verb, no blind F24 sweep, and an immediate
// refresh so the redrawn truthful verb does not wait for the next poll.
func TestMuteClickRevalidates(t *testing.T) {
	armedMute := trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted}
	armedUnmute := trayMuteSnapshot{Title: "Unmute", Armed: trayStateMuted}
	load := func(snap trayMuteSnapshot) func() trayMuteSnapshot {
		return func() trayMuteSnapshot { return snap }
	}

	// Premise reproduced: the click fires the label's absolute verb, then
	// the sweep, then the refresh - the full mute-everything pair exactly
	// once.
	matchingMute := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "unmuted"},
		"mute":   {reply: "muted"},
	}}
	matchingMute.actions().muteClick(load(armedMute))
	if got := matchingMute.order(); got != "ask:status,ask:mute,inject,refresh" {
		t.Fatalf("muteClick armed Mute with a matching probe = %q, want %q", got, "ask:status,ask:mute,inject,refresh")
	}

	matchingUnmute := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "muted"},
		"unmute": {reply: "unmuted"},
	}}
	matchingUnmute.actions().muteClick(load(armedUnmute))
	if got := matchingUnmute.order(); got != "ask:status,ask:unmute,inject,refresh" {
		t.Fatalf("muteClick armed Unmute with a matching probe = %q, want %q", got, "ask:status,ask:unmute,inject,refresh")
	}

	// Declined clicks: probe + refresh only, plus exactly one WARN. The
	// flipped-premise case is pinned in BOTH flip directions: a click must
	// never fire its verb when the mic already sits in the label's target
	// state, whichever direction the label named.
	declines := []struct {
		name    string
		snap    trayMuteSnapshot
		outcome scriptOutcome
	}{
		{"flipped premise: armed Mute, probe already muted (label's target already true)", armedMute, scriptOutcome{reply: "muted"}},
		{"flipped premise: armed Unmute, probe already unmuted (label's target already true)", armedUnmute, scriptOutcome{reply: "unmuted"}},
		{"unknown probe", armedMute, scriptOutcome{reply: "unknown"}},
		{"daemon down", armedMute, scriptOutcome{err: fmt.Errorf("%w: %w", errNoReply, syscall.ECONNREFUSED)}},
	}
	for _, c := range declines {
		levels := &levelRecorder{}
		spy := &traySpy{script: map[string]scriptOutcome{"status": c.outcome}}
		a := spy.actions()
		a.logger = slog.New(levels)
		a.muteClick(load(c.snap))
		if got := spy.order(); got != "ask:status,refresh" {
			t.Fatalf("muteClick with %s = %q, want %q (probe + refresh only: no verb, no sweep)", c.name, got, "ask:status,refresh")
		}
		if len(levels.levels) != 1 || levels.levels[0] != slog.LevelWarn {
			t.Fatalf("muteClick with %s levels = %v, want [WARN] (a declined premise-check is not an error)", c.name, levels.levels)
		}
	}
}

// TestMuteClickLoadsSnapshotOnce pins the F7 contract: the click loads the
// {title, armed} snapshot EXACTLY ONCE - the displayed verb and the premise
// the click re-checks are a single read, never a mixture of a fresh title
// with a stale premise.
func TestMuteClickLoadsSnapshotOnce(t *testing.T) {
	loads := 0
	load := func() trayMuteSnapshot {
		loads++
		return trayMuteSnapshot{Title: "Mute", Armed: trayStateUnmuted}
	}
	spy := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "unmuted"},
		"mute":   {reply: "muted"},
	}}
	spy.actions().muteClick(load)
	if got := spy.order(); got != "ask:status,ask:mute,inject,refresh" {
		t.Fatalf("muteClick with a matching probe = %q, want %q", got, "ask:status,ask:mute,inject,refresh")
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
// including the per-line fleet shape the old prefix check missed.
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
		injectSweep:   func() error { return nil },
		stopPanel:     func() error { return nil },
		requestQuit:   func() {},
		signalRefresh: func() {},
		logger:        slog.New(levels),
	}
	a.onMicSet("mute")
	a.onLight("light toggle")
	if len(levels.levels) != 2 || levels.levels[0] != slog.LevelError || levels.levels[1] != slog.LevelError {
		t.Fatalf("levels = %v, want [ERROR ERROR] (mute error + per-line fleet error)", levels.levels)
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
