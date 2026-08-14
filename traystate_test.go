package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"strings"
	"testing"
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
	calls  []string
	askErr error
	injErr error
}

func (s *traySpy) actions() *trayActions {
	return &trayActions{
		ask:           func(cmd string) (string, error) { s.calls = append(s.calls, "ask:"+cmd); return "", s.askErr },
		openPanel:     func() error { s.calls = append(s.calls, "panel"); return nil },
		injectSweep:   func() error { s.calls = append(s.calls, "inject"); return s.injErr },
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

// TestTrayQuitStopsDaemonThenQuits pins the daemon-quitting Quit semantics
// (the explicit requirement), including when the shutdown ack never arrives.
func TestTrayQuitStopsDaemonThenQuits(t *testing.T) {
	spy := &traySpy{}
	spy.actions().onQuit()
	if got := spy.order(); got != "ask:shutdown,quit" {
		t.Fatalf("onQuit side effects = %q, want %q", got, "ask:shutdown,quit")
	}

	failing := &traySpy{askErr: errors.New("daemon dead")}
	failing.actions().onQuit()
	if got := failing.order(); got != "ask:shutdown,quit" {
		t.Fatalf("onQuit with a dead daemon = %q, want %q (the tray must still quit)", got, "ask:shutdown,quit")
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
