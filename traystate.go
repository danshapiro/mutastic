package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"syscall"
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

// trayStateName is the compact state token used in structured log fields:
// the menu display titles (trayTitle) are for humans, but JSONL queries
// want stable one-word values (muted/unmuted/unknown/down).
func trayStateName(s trayState) string {
	switch s {
	case trayStateMuted:
		return "muted"
	case trayStateUnmuted:
		return "unmuted"
	case trayStateUnknown:
		return "unknown"
	default:
		return "down"
	}
}

// trayMuteTitle is the mic action item's displayed verb per state - the
// click performs exactly the displayed action, so the label is always the
// OPPOSITE of the last definitive mic state. Unknown-or-down reads the
// neutral "Mute/Unmute": a disabled item still displays its last-set
// title, and the neutral text keeps a stale directional verb off the
// screen.
func trayMuteTitle(s trayState) string {
	switch s {
	case trayStateMuted:
		return "Unmute"
	case trayStateUnmuted:
		return "Mute"
	default:
		return "Mute/Unmute"
	}
}

// trayActionsEnabled reports whether the light menu actions are usable;
// they are daemon round trips, so a dead daemon disables them (the
// Light panel and Quit items stay enabled: the panel is served by the
// separate `mutastic ui` process, and Quit must always work). The
// Mute/Unmute action is gated separately by trayMuteEnabled: reachability
// alone does not make a mic click safe.
func trayActionsEnabled(s trayState) bool { return s != trayStateDown }

// trayMuteEnabled arms the dynamic Mute/Unmute action item. The click fires
// the ABSOLUTE verb the label names after re-checking the premise; only
// definitive daemon answers give it a premise (and a direction), so unknown
// and down both gate the item out. Light actions stay gated separately by
// trayActionsEnabled: unknown is a mic-state concept, not a reachability
// one.
func trayMuteEnabled(s trayState) bool { return s == trayStateMuted || s == trayStateUnmuted }

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

// trayMenuSpec is one "Saved settings" submenu entry as a {title, enabled}
// PAIR: a real saved name renders enabled (click applies it); the two
// placeholder rows render disabled. The pair is compared whole by
// traySameMenuSpecs because the settings-name grammar permits a literal
// saved name equal to a placeholder string - title-only comparison would
// leave a real setting grayed (or a placeholder clickable) across a
// placeholder<->real transition.
type trayMenuSpec struct {
	Title   string
	Enabled bool
}

// traySavedSettingsListCmd is the daemon verb the tray's refresh loop polls
// once per tick to keep the "Saved settings" submenu in sync with the
// store (the daemon's logCommand latch keeps the steady-state poll quiet in
// daemon.log).
const traySavedSettingsListCmd = "light settings list"

// traySavedSettings renders one poll result as submenu specs in three
// regimes: daemon not-ok (transport down OR store broken) -> one DISABLED
// "(settings unavailable)" placeholder (one wording covers both honestly);
// ok with no names -> one DISABLED "(no saved settings)" placeholder;
// otherwise one {name, ENABLED} per name in input order (the daemon emits
// them sorted). Both placeholder strings are tray-side UI text, NEVER sent
// by the daemon.
func traySavedSettings(names []string, daemonOK bool) []trayMenuSpec {
	if !daemonOK {
		return []trayMenuSpec{{Title: "(settings unavailable)"}}
	}
	if len(names) == 0 {
		return []trayMenuSpec{{Title: "(no saved settings)"}}
	}
	specs := make([]trayMenuSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, trayMenuSpec{Title: name, Enabled: true})
	}
	return specs
}

// trayParseSettingsList parses one "light settings list" round trip. The
// daemon's wire contract: "" means "none saved" (daemon OK, NOT an error),
// names are newline-joined in order, and a disabled/corrupt store answers a
// single-line "error:" refusal. ANY ask error - or an error:-prefixed
// reply - is NOT-OK: without the reply guard the refusal text would render
// as an ENABLED menu item whose click always fails.
func trayParseSettingsList(reply string, err error) (names []string, daemonOK bool) {
	if err != nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(reply)
	if strings.HasPrefix(trimmed, "error:") {
		return nil, false
	}
	if trimmed == "" {
		return nil, true
	}
	return strings.Split(trimmed, "\n"), true
}

// traySameMenuSpecs reports whether two rendered spec lists are identical,
// comparing {Title, Enabled} pairs element-wise (struct equality on
// trayMenuSpec is exactly the pair comparison, never titles only). The
// refresh loop rebuilds the submenu children exactly when this reports
// false; two nils compare equal (the state before the first poll).
func traySameMenuSpecs(a, b []trayMenuSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// trayActions holds every side-effecting dependency of the tray's click
// handlers as an injected function, so the handlers' ordering and failure
// semantics are unit-testable on Linux. tray_windows.go wires the real
// dependencies (askDaemon, openBrowser, newKeyInjector().Inject,
// systray.Quit). The handlers never block the library's message-pump
// thread: the Windows glue calls each one in its own goroutine.
type trayActions struct {
	ask           func(command string) (string, error) // one daemon round trip (production budget: lightClientTimeout)
	openPanel     func() error                         // open/focus the browser control panel (mic + lights + settings)
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

// udpNoListenerErrnos classify "nothing is listening" for the daemon's UDP
// port: an unheard datagram earns an ICMP port-unreachable, which Windows
// surfaces as WSAECONNRESET and Linux as ECONNREFUSED. New Winsock codes
// are matched numerically because Go's syscall package intentionally
// defines ECONNREFUSED/ECONNRESET as invented APPLICATION_ERROR values on
// Windows (never equal to real Winsock codes) and does not define
// WSAECONNREFUSED at all - keep these in sync with winsock.h
// (WSAECONNREFUSED=10061, WSAECONNRESET=10054).
var udpNoListenerErrnos = []syscall.Errno{
	syscall.ECONNREFUSED, syscall.ECONNRESET,
	syscall.Errno(10061), syscall.Errno(10054),
}

// tcpNoListenerErrnos classify "nothing is listening" for the light panel's
// TCP listener. A RST (ECONNRESET/10054) must NOT be here: on TCP a reset
// can be produced by a LIVE, wedged listener; only a refusal proves the
// port is closed.
var tcpNoListenerErrnos = []syscall.Errno{
	syscall.ECONNREFUSED, syscall.Errno(10061),
}

func anyErrno(err error, list []syscall.Errno) bool {
	for _, errno := range list {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// udpNoListener reports whether an error proves no daemon is listening.
func udpNoListener(err error) bool { return anyErrno(err, udpNoListenerErrnos) }

// tcpNoListener reports whether an error proves no light panel is listening.
func tcpNoListener(err error) bool { return anyErrno(err, tcpNoListenerErrnos) }

// trayMuteSnapshot is the mic item's displayed title and the click's armed
// premise (the state that title's verb targets) as ONE atomic unit: the
// refresh loop stores the whole pair once per tick and the click handler
// loads it back exactly once, so the two can never diverge (separate
// writes would allow a fresh title to meet a stale premise).
type trayMuteSnapshot struct {
	Title string
	Armed trayState
}

// onMicSet is the tray's mute-everything path for one ABSOLUTE direction,
// mirroring the Stream Deck mute key (README): the daemon verb AND one F24
// meeting-app sweep, both attempted even when the other fails, then a
// refresh. (The daemon injects F24 only for physical button presses, so a
// verb-only tray item would move the mic while leaving meeting apps live.)
// Loop-free for the documented reasons: the AHK sweep never calls a mic
// verb, and the mic's host-command echo (0x20) is ignored by the daemon's
// injector gate.
func (a *trayActions) onMicSet(verb string) {
	reply, askErr := a.ask(verb)
	sweepErr := a.injectSweep()
	if askErr != nil || sweepErr != nil || strings.HasPrefix(reply, "error:") {
		a.logger.Error("mic set failed", "verb", verb, "daemon_reply", reply, "ask_err", errString(askErr), "sweep_err", errString(sweepErr))
	} else {
		a.logger.Info("mic set", "verb", verb, "daemon_reply", reply)
	}
	// The daemon applied its state change before replying; on failure the
	// refresh restores the truthful display within one poll.
	a.signalRefresh()
}

// muteClick is the dynamic Mute/Unmute item's click entry point: the click
// performs exactly the action the displayed label names. It loads the
// {title, armed} snapshot EXACTLY ONCE (one read of what the user saw: the
// menu's title and enabled bit are only the last completed poll's
// rendering), then revalidates the premise AT ACTION TIME with a fresh
// status probe. Only when the probe reproduces the snapshot's armed state
// (and that state is definitive) does the click fire the label's absolute
// verb - armed unmuted ("Mute") fires "mute", armed muted ("Unmute") fires
// "unmute" - followed by the sweep and a refresh.
//
// Everything else DECLINES: a definitive-but-opposite probe (the mic
// flipped between the last poll and the click), an unknown probe, and a
// dead daemon all produce one WARN, NO mic verb, NO sweep, and a refresh so
// the redrawn truthful verb does not wait for the next 2 s poll. Declining
// is the correct convergence, not a breach of "click performs the displayed
// action": when the premise has flipped, the label's TARGET state is
// already true (a "Mute" label targets muted; a muted probe means the
// click's job is done), and the F24 sweep is a blind per-app toggle that
// must never fire without the matching mic move. The native menu title can
// go cosmetically stale between polls, but that staleness can never cause
// a wrong action BECAUSE this probe gates every firing - this is a
// documented rendering limitation, not a behavioral residual. Desync
// recovery for the apps stays the documented manual procedure (toggle them
// once by hand, then every sweeping path keeps them in sync).
func (a *trayActions) muteClick(load func() trayMuteSnapshot) {
	snap := load()
	probe := trayStateFor(a.ask("status"))
	if !trayMuteEnabled(probe) || probe != snap.Armed {
		a.logger.Warn("mute click declined: mic state no longer matches the menu premise", "armed", trayStateName(snap.Armed), "probe", trayStateName(probe))
		a.signalRefresh()
		return
	}
	verb := "mute"
	if snap.Armed == trayStateMuted {
		verb = "unmute"
	}
	a.onMicSet(verb)
}

// onOpenPanel brings up the control panel (the icon's left-click action);
// a failed open is logged, not swallowed.
func (a *trayActions) onOpenPanel() {
	if err := a.openPanel(); err != nil {
		a.logger.Error("open control panel failed", "err", errString(err))
	}
}

// onLight sends one fire-and-forget light command and logs the outcome,
// so a wedged or unreachable light is visible in tray.log instead of
// failing silently. Ordering across rapid menu clicks is the caller's
// concern (the Windows glue serializes these through one channel whose
// sends happen on the click handlers' thread). Multi-light fleet replies
// are per-line ("COM4: on 30% 2900K\nCOM7: error: timeout"), so a
// per-line "error:" ANYWHERE in the reply - not just at the start - is a
// failure worth an ERROR line.
func (a *trayActions) onLight(command string) {
	reply, err := a.ask(command)
	if err != nil || strings.Contains(reply, "error:") {
		a.logger.Error("light command failed", "cmd", command, "reply", reply, "err", errString(err))
	} else {
		a.logger.Info("light command", "cmd", command, "reply", reply)
	}
}

// onQuit stops everything mutastic runs and exits the tray ONLY when each
// stop is confirmed or already absent (a daemon or panel that cannot be
// reached at all already satisfies the goal state; a live one that REFUSES
// is a failure). On failure the tray stays: the display refreshes and the
// next Quit retries. Quietly exiting while leaving live mutastic processes
// behind would break the one-click-stops-everything contract.
func (a *trayActions) onQuit() {
	daemonOK := true
	reply, err := a.ask("shutdown")
	switch {
	case err == nil && reply != "shutting down":
		daemonOK = false
		a.logger.Error("quit: daemon refused shutdown", "reply", reply)
	case err != nil && udpNoListener(err):
		// The daemon's UDP port answered the shutdown datagram with an ICMP
		// port-unreachable (or refused/reset outright): nothing is
		// listening, which is already the goal state.
		a.logger.Info("quit: daemon port refuses connections, treated as stopped", "err", errString(err))
	case err != nil:
		// Anything else - above all a timeout (errNoReply), which a live but
		// wedged or backlogged daemon also produces - is UNCONFIRMED. The
		// tray stays so Quit can be retried.
		daemonOK = false
		a.logger.Error("quit: daemon shutdown unconfirmed", "reply", reply, "err", errString(err))
	default:
		a.logger.Info("quit: daemon shutdown", "reply", reply)
	}
	panelOK := true
	if err := a.stopPanel(); err != nil {
		panelOK = false
		a.logger.Error("quit: light panel shutdown failed", "err", errString(err))
	}
	if daemonOK && panelOK {
		a.requestQuit()
		return
	}
	a.signalRefresh()
}

// stopLightPanel asks a running `mutastic ui` server to stop (Task 4's
// endpoint). ECONNREFUSED proves nothing is listening - dead or never
// started, which is already Quit's goal state - so only an ALIVE panel (a
// non-OK status, or a wedged listener that never answers) is an error
// worth logging.
func stopLightPanel(baseURL string) error {
	req, err := http.NewRequest(http.MethodPost, baseURL+"api/shutdown", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// ECONNREFUSED proves nothing is listening: the panel is gone, which
		// is Quit's goal state. Any other transport failure (timeout,
		// reset) may be a live but wedged panel - that is a real error, and
		// onQuit logs it at ERROR. On Windows the refusal instead arrives
		// as a raw Winsock code in a syscall.Errno inside the net.OpError
		// chain (WSAECONNREFUSED=10061, never Go's invented ECONNREFUSED),
		// so tcpNoListener matches both shapes.
		if tcpNoListener(err) {
			return nil
		}
		return err
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
