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
	if !trayMutedChecked(trayStateMuted) || trayMutedChecked(trayStateUnmuted) {
		t.Error("Muted checkbox must be checked exactly in the muted state")
	}
	if trayActionsEnabled(trayStateDown) {
		t.Error("actions must be disabled while the daemon is unreachable")
	}
	for _, s := range []trayState{trayStateMuted, trayStateUnmuted, trayStateUnknown} {
		if !trayActionsEnabled(s) {
			t.Errorf("actions must be enabled in state %v", s)
		}
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
}

func (s *traySpy) actions() *trayActions {
	return &trayActions{
		ask: func(cmd string) (string, error) {
			s.calls = append(s.calls, "ask:"+cmd)
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

// TestTrayMicToggleIsMuteEverything mirrors the Stream Deck mute key: a
// daemon toggle AND the F24 meeting-app sweep, then a refresh - and the
// sweep must fire even when the daemon round trip fails (a toggle-only
// path would mute the mic while leaving meeting apps live).
func TestTrayMicToggleIsMuteEverything(t *testing.T) {
	spy := &traySpy{}
	spy.actions().onMicToggle()
	if got := spy.order(); got != "ask:toggle,inject,refresh" {
		t.Fatalf("onMicToggle side effects = %q, want %q", got, "ask:toggle,inject,refresh")
	}

	failing := &traySpy{askErr: errors.New("daemon dead")}
	failing.actions().onMicToggle()
	if got := failing.order(); got != "ask:toggle,inject,refresh" {
		t.Fatalf("onMicToggle with a dead daemon = %q, want %q (the sweep must still fire)", got, "ask:toggle,inject,refresh")
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
	a.onMicToggle()
	a.onLight("light toggle")
	if len(levels.levels) != 2 || levels.levels[0] != slog.LevelError || levels.levels[1] != slog.LevelError {
		t.Fatalf("levels = %v, want [ERROR ERROR] (toggle error + per-line fleet error)", levels.levels)
	}
}
