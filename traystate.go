package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// trayState is the collapsed tray display state derived from one daemon
// "status" round trip. It maps the daemon's replies ("muted", "unmuted",
// "unknown") plus the transport failure case ("down") onto everything the
// systray glue (Windows) needs, so the display decisions are unit-testable
// on Linux. All mapping functions are pure.
type trayState int

const (
	trayStateDown trayState = iota
	trayStateMuted
	trayStateUnmuted
	trayStateUnknown
)

// trayStateFor collapses one status round trip into a display state. Any
// transport failure is "down" (the daemon's UDP port doubles as its liveness
// signal); any non-error reply other than a definitive state - including the
// daemon's literal "unknown" - is the unknown-mic-state display rather than
// "daemon gone".
func trayStateFor(reply string, err error) trayState {
	if err != nil {
		return trayStateDown
	}
	switch reply {
	case "muted":
		return trayStateMuted
	case "unmuted":
		return trayStateUnmuted
	default:
		return trayStateUnknown
	}
}

// trayTitle is the status line shown as the disabled menu header and the
// icon tooltip.
func trayTitle(s trayState) string {
	switch s {
	case trayStateMuted:
		return "Mutastic — muted"
	case trayStateUnmuted:
		return "Mutastic — live"
	case trayStateUnknown:
		return "Mutastic — mic state unknown"
	default:
		return "Mutastic — daemon unreachable"
	}
}

// trayMutedChecked reports the Muted menu item's check state.
func trayMutedChecked(s trayState) bool { return s == trayStateMuted }

// trayActionsEnabled reports whether mic/light menu actions are usable;
// they are all daemon round trips, so a dead daemon disables them (the
// Light panel and Quit items stay enabled: the panel is served by the
// separate `mutastic ui` process, and Quit must always work).
func trayActionsEnabled(s trayState) bool { return s != trayStateDown }

// trayIcon is an icon decision for one display state. trayIconKeep leaves
// the current icon untouched: when the daemon's answer is "unknown" or the
// daemon is unreachable, the mic's true hardware state is not known, and
// switching to the white "live" icon could display a LIVE mic while the
// hardware is actually muted (a daemon crash while muted would do exactly
// that). Only definitive answers switch the icon - the same convention the
// Stream Deck plugin documents ("unknown keeps the last icon", README).
type trayIcon int

const (
	trayIconKeep trayIcon = iota
	trayIconLiveMic
	trayIconMutedMic
)

// trayIconFor maps a display state to an icon decision.
func trayIconFor(s trayState) trayIcon {
	switch s {
	case trayStateMuted:
		return trayIconMutedMic
	case trayStateUnmuted:
		return trayIconLiveMic
	default:
		return trayIconKeep
	}
}

// trayActions holds every side-effecting dependency of the tray's click
// handlers as an injected function, so the handlers' ordering and failure
// semantics are unit-testable on Linux. tray_windows.go wires the real
// dependencies (askDaemon, openBrowser, newKeyInjector().Inject,
// systray.Quit). The handlers never block the library's message-pump
// thread: the Windows glue calls each one in its own goroutine.
type trayActions struct {
	ask           func(command string) (string, error) // one daemon round trip (production budget: lightClientTimeout)
	openPanel     func() error                         // open/focus the browser light panel
	injectSweep   func() error                         // one synthetic F24 (meeting-app sweep)
	stopPanel     func() error                         // POST the light panel's /api/shutdown (Task 4 endpoint)
	requestQuit   func()                               // leave the systray message loop
	signalRefresh func()                               // ask the display loop to repoll
	logger        *slog.Logger
}

// newTrayJSONLogger builds the tray's JSONL logger (one JSON object per
// line with "level" and "msg", per the logging instruction covering new
// repo code). Kept platform-neutral so the line format is unit-tested.
func newTrayJSONLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// onMicToggle is the tray's mute-everything path, mirroring the Stream Deck
// mute key (README): a hardware toggle to the daemon AND one F24 meeting-app
// sweep, both attempted even when the other fails. (The daemon injects F24
// only for physical button presses, so a toggle-only tray would mute the mic
// while leaving meeting apps live.) Loop-free for the documented reasons:
// the AHK sweep never calls toggle, and the mic's host-command echo (0x20)
// is ignored by the daemon's injector gate.
func (a *trayActions) onMicToggle() {
	reply, askErr := a.ask("toggle")
	sweepErr := a.injectSweep()
	a.logger.Info("mic toggle", "daemon_reply", reply, "ask_err", errString(askErr), "sweep_err", errString(sweepErr))
	// The daemon applied its state change before replying; on failure the
	// refresh restores the truthful display within one poll.
	a.signalRefresh()
}

// onOpenPanel brings up the light-panel UI (the icon's left-click action);
// a failed open is logged, not swallowed.
func (a *trayActions) onOpenPanel() {
	if err := a.openPanel(); err != nil {
		a.logger.Error("open light panel failed", "err", errString(err))
	}
}

// onLight sends one fire-and-forget light command and logs the outcome,
// so a wedged or unreachable light is visible in tray.log instead of
// failing silently. Ordering across rapid menu clicks is the caller's
// concern (the Windows glue serializes these through one channel whose
// sends happen on the click handlers' thread).
func (a *trayActions) onLight(command string) {
	reply, err := a.ask(command)
	a.logger.Info("light command", "cmd", command, "reply", reply, "err", errString(err))
}

// onQuit stops everything mutastic runs - the daemon, the light-panel
// server, and finally the tray itself. Every stop is best-effort: a dead
// daemon or missing panel must never strand the tray or skip the other
// stop.
func (a *trayActions) onQuit() {
	reply, err := a.ask("shutdown")
	a.logger.Info("quit: daemon shutdown", "reply", reply, "err", errString(err))
	if err := a.stopPanel(); err != nil {
		a.logger.Error("quit: light panel shutdown failed", "err", errString(err))
	}
	a.requestQuit()
}

// stopLightPanel asks a running `mutastic ui` server to stop (Task 4's
// endpoint). A transport failure means the panel is unreachable - dead or
// never started, which is already Quit's goal state - so only an ALIVE but
// refusing panel is an error worth logging.
func stopLightPanel(baseURL string) error {
	req, err := http.NewRequest(http.MethodPost, baseURL+"api/shutdown", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil // unreachable = not running = goal state
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("light panel shutdown answered status %d", resp.StatusCode)
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
