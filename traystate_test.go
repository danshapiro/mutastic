package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	calls   []string
	askErr  error
	injErr  error
	stopErr error
}

func (s *traySpy) actions() *trayActions {
	return &trayActions{
		ask:           func(cmd string) (string, error) { s.calls = append(s.calls, "ask:"+cmd); return "", s.askErr },
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
// shutdown AND light-panel shutdown, then the tray exits — and each stop
// failing must never strand the tray or skip the other stop.
func TestTrayQuitStopsEverythingThenQuits(t *testing.T) {
	spy := &traySpy{}
	spy.actions().onQuit()
	if got := spy.order(); got != "ask:shutdown,panelstop,quit" {
		t.Fatalf("onQuit side effects = %q, want %q", got, "ask:shutdown,panelstop,quit")
	}

	deadDaemon := &traySpy{askErr: errors.New("daemon dead")}
	deadDaemon.actions().onQuit()
	if got := deadDaemon.order(); got != "ask:shutdown,panelstop,quit" {
		t.Fatalf("onQuit with a dead daemon = %q, want %q (panel stop and tray quit must still run)", got, "ask:shutdown,panelstop,quit")
	}

	deadPanel := &traySpy{stopErr: errors.New("panel refused")}
	deadPanel.actions().onQuit()
	if got := deadPanel.order(); got != "ask:shutdown,panelstop,quit" {
		t.Fatalf("onQuit with a failing panel stop = %q, want %q (the tray must still quit)", got, "ask:shutdown,panelstop,quit")
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
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer wedged.Close()
	err = stopLightPanel(wedged.URL + "/")
	if err == nil {
		t.Fatal("stopLightPanel against a wedged panel = nil, want a transport error (only ECONNREFUSED counts as goal state)")
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
