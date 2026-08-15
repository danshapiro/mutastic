//go:build windows

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/energye/systray"
)

//go:embed deck/icons/tray-mic.ico
var trayIconLive []byte

//go:embed deck/icons/tray-mic-muted.ico
var trayIconMuted []byte

//go:embed deck/icons/tray-mic-unknown.ico
var trayIconUnknown []byte

const (
	// trayPanelURL is the light controller served by `mutastic ui`
	// (ui.go); left-clicking the tray icon opens it.
	trayPanelURL = "http://127.0.0.1:42815/"
	// trayInstanceAddr is a loopback TCP listen that doubles as the tray's
	// single-instance lock - the same trick as the daemon's UDP bind.
	trayInstanceAddr = "127.0.0.1:42816"
	trayPollEvery    = 2 * time.Second
	// trayIconReapplyEvery paces the self-heal heartbeat. The fork caches
	// loaded HICONs by the icon's content-hash temp path (verified: its
	// loadIconFrom reuses handles), so reapplying one of our three assets
	// costs nothing extra - the heartbeat exists purely to re-drive
	// NIM_MODIFY after a transient failure. Tooltip, header, menu titles,
	// and enabled states carry no handle cost and converge every tick.
	trayIconReapplyEvery = 300
)

// runTray puts the mutastic icon in the notification area and blocks in the
// systray message loop until Quit. The tray is a pure CLIENT of the daemon
// (UDP, like the deck plugin): it owns no hardware, so quitting - or
// crashing - leaves the mute path intact. It logs to its OWN file: two
// processes racing the rename-rotation of one log is a real hazard (the
// same reason daemon and deckplugin log separately), and with the console
// hidden at login, tray.log is the only place icon/startup failures can
// surface. The systray fork logs its OWN failures (NIM_ADD and friends)
// through the stdlib default logger; installing our JSONL slog logger as
// the slog DEFAULT routes those stdlib records through the same JSON
// handler (the stdlib log->slog bridge, Go 1.21+), so tray.log stays
// JSONL-only even for library-level errors like "Windows refused the icon".
func runTray() int {
	hideConsoleIfOwned()
	logw, logPath, err := openNamedLogFile("tray.log")
	if err != nil {
		logw = nopWriteCloser{}
		logPath = "(unavailable)"
	}
	defer logw.Close()
	logger := newTrayJSONLogger(io.MultiWriter(logw, os.Stderr))
	slog.SetDefault(logger) // library log.Printf records become JSONL INFO lines

	logger.Info("mutastic tray starting", "log", logPath)
	lock, err := net.Listen("tcp", trayInstanceAddr)
	if err != nil {
		// Another tray process owns the icon; a second one adds nothing.
		logger.Error("cannot bind tray instance lock", "addr", trayInstanceAddr, "err", errString(err))
		return 1
	}
	defer lock.Close()
	ready := make(chan struct{})
	// Watchdog: if Windows refuses the icon, the fork logs the failure,
	// never calls onReady, and still enters its blocking message loop -
	// leaving an invisible process holding the single-instance lock. Exit
	// instead: process death releases the lock, so the documented recovery
	// (rerun the startup shortcut) actually works.
	go func() {
		select {
		case <-ready:
		case <-time.After(15 * time.Second):
			logger.Error("tray icon was never installed within the startup window; exiting so a rerun can reclaim the instance lock")
			os.Exit(1)
		}
	}()
	systray.Run(
		func() { trayOnReady(logger); close(ready) },
		func() { logger.Info("mutastic tray exiting") },
	)
	logger.Info("mutastic tray stopped")
	return 0
}

func trayOnReady(logger *slog.Logger) {
	// Start from the neutral "unknown" icon: claiming "live" before the
	// daemon has EVER given a definitive answer could display a live mic
	// while the hardware is muted (keep-last-icon does the rest).
	systray.SetIcon(trayIconUnknown)
	systray.SetTooltip("Mutastic")

	header := systray.AddMenuItem("Mutastic", "daemon status")
	header.Disable()
	systray.AddSeparator()

	// The mic item is an ACTION item (never checkable) that always displays
	// the verb its click performs: "Mute" when live, "Unmute" when muted,
	// the neutral "Mute/Unmute" while indefinite. Its displayed title and
	// the click's armed premise are ONE atomic snapshot (F7): the refresh
	// loop stores the whole pair once per tick and draws the item's title
	// and enabled bit from the stored value; the click handler loads it
	// back exactly once. The initial neutral snapshot matches the item's
	// startup title + disabled state, so a click before the first tick
	// reads a premise that cannot fire.
	var muteSnap atomic.Value
	muteSnap.Store(trayMuteSnapshot{Title: trayMuteTitle(trayStateUnknown), Armed: trayStateUnknown})
	loadMuteSnap := func() trayMuteSnapshot { return muteSnap.Load().(trayMuteSnapshot) }
	mic := systray.AddMenuItem(trayMuteTitle(trayStateUnknown), "mute-everything: the displayed mic verb + F24 meeting-app sweep (same flow as the Stream Deck mute key)")
	systray.AddSeparator()

	lights := systray.AddMenuItem("Toggle lights", "if any light is on, turn all off; otherwise turn all on")
	brightness := systray.AddMenuItem("Brightness", "set brightness on all lights")
	preset := systray.AddMenuItem("Light preset", "apply a preset on all lights")

	// Action items start disabled; the refresh loop arms them (the light
	// actions on the first reachable poll, the mic item on the first
	// DEFINITIVE mic answer, per trayMuteEnabled) - so no click can fire in
	// the startup window.
	for _, it := range []*systray.MenuItem{mic, lights, brightness, preset} {
		it.Disable()
	}

	systray.AddSeparator()
	panel := systray.AddMenuItem("Panel…", "open the full light controller in the browser")
	quit := systray.AddMenuItem("Quit", "stop everything mutastic runs (daemon, light panel) and exit")

	// All tray-visible state is refreshed by one goroutine fed by refreshCh:
	// the ticker and the menu handlers only signal. (systray methods may be
	// called from any goroutine; the serialization protects OUR last-known
	// state, not the library.)
	refreshCh := make(chan struct{}, 1)
	signalRefresh := func() {
		select {
		case refreshCh <- struct{}{}:
		default:
		}
	}

	// The handlers' ordering and failure semantics live in the testable
	// platform-neutral trayActions; here only the real dependencies are
	// wired. Every ask uses lightClientTimeout, including mic verbs:
	// serveUDP is serial, so any command can queue behind a wedged light
	// call (~light.CallTimeout); a 1s budget would intermittently mask the
	// daemon as unreachable while it is alive.
	actions := &trayActions{
		ask:           func(cmd string) (string, error) { return askDaemon(cmd, udpAddr, lightClientTimeout) },
		openPanel:     func() error { return openBrowser(trayPanelURL) },
		stopPanel:     func() error { return stopLightPanel(trayPanelURL) },
		requestQuit:   systray.Quit,
		signalRefresh: signalRefresh,
		logger:        logger,
	}
	if inj := newKeyInjector(); inj != nil {
		actions.injectSweep = inj.Inject
	} else {
		actions.injectSweep = func() error { return errors.New("key injection unavailable") }
	}

	// Light menu commands are serialized through one drained channel. The
	// sends happen DIRECTLY in the click handlers (which all run on the
	// library's single message-pump thread), so the enqueue order IS the
	// click order; sending from per-click goroutines would let the
	// scheduler reorder them. The 16-deep buffer absorbs any human click
	// burst; it fills only if 16 wedged light calls (~lightClientTimeout
	// each) are already queued, and a brief menu stall is the honest price
	// of ordered delivery.
	lightCmdCh := make(chan string, 16)
	go func() {
		for cmd := range lightCmdCh {
			actions.onLight(cmd)
		}
	}()

	// Left click opens the light panel; right click shows the menu. The
	// fork's default is "menu hidden once handlers are set", so ask for it
	// explicitly. The icon-click and mic/quit/panel handlers run on the
	// pump thread, so they spawn goroutines rather than block it (the light
	// handlers only enqueue, which is non-blocking in practice).
	systray.SetOnClick(func(systray.IMenu) { go actions.onOpenPanel() })
	systray.SetOnRClick(func(m systray.IMenu) {
		if err := m.ShowMenu(); err != nil {
			logger.Error("show tray menu failed", "err", errString(err))
		}
	})

	mic.Click(func() { go actions.muteClick(loadMuteSnap) })
	lightCmd := func(command string) func() {
		return func() { lightCmdCh <- command }
	}
	lights.Click(lightCmd("light toggle"))
	for _, pct := range []int{10, 25, 50, 75, 100} {
		brightness.AddSubMenuItem(fmt.Sprintf("%d%%", pct), "").Click(lightCmd(fmt.Sprintf("light brightness %d", pct)))
	}
	for _, name := range []string{"cold", "sunlight", "afternoon", "sunset", "candle"} {
		preset.AddSubMenuItem(name, "").Click(lightCmd("light preset " + name))
	}
	panel.Click(func() { go actions.onOpenPanel() })
	quit.Click(func() { go actions.onQuit() })

	go trayRefreshLoop(logger, refreshCh, header, mic, lights, brightness, preset, &muteSnap)
	go func() {
		for range time.Tick(trayPollEvery) {
			signalRefresh()
		}
	}()
	signalRefresh()
	logger.Info("tray ready")
}

// trayRefreshLoop owns all tray-visible state. Each signal triggers one
// daemon status round trip; every display decision comes from the pure
// traystate.go mapping. Convergence is intentional: tooltip, header, menu
// titles, and enabled states carry no handle cost and are re-asserted on
// every signal, so a transient failure heals on the next tick - the
// transition gate only decides logging and icon reapplies (SetIcon leaks a
// GDI handle per call; see trayIconReapplyEvery). The icon switches only
// on definitive answers (trayIconFor): unknown or unreachable keeps the
// last icon.
func trayRefreshLoop(logger *slog.Logger, refreshCh <-chan struct{}, header, mic, lights, brightness, preset *systray.MenuItem, muteSnap *atomic.Value) {
	first := true
	last := trayStateUnknown
	tick := 0
	// lastShown is the icon currently displayed; it starts as the neutral
	// unknown asset (also set at ready) and changes only on definitive
	// answers. Change ticks AND the heartbeat reapply lastShown, so a
	// transient SetIcon failure - including a failed initial set before the
	// first definitive answer - self-heals; the early ticks (1-3) cover the
	// startup window the watchdog can't see (the fork reports readiness
	// even when the initial SetIcon failed).
	lastShown := trayIconUnknown
	for range refreshCh {
		tick++
		reply, err := askDaemon("status", udpAddr, lightClientTimeout)
		state := trayStateFor(reply, err)
		change := first || state != last
		if change {
			first = false
			last = state
			logger.Info("status display", "title", trayTitle(state))
		}
		if change || tick <= 3 || tick%trayIconReapplyEvery == 0 {
			switch trayIconFor(state) {
			case trayIconMutedMic:
				lastShown = trayIconMuted
			case trayIconLiveMic:
				lastShown = trayIconLive
			} // trayIconKeep: reassert the currently displayed icon
			systray.SetIcon(lastShown)
		}
		systray.SetTooltip(trayTitle(state))
		header.SetTitle(trayTitle(state))
		// The mic item's displayed verb and the click's armed premise are
		// ONE atomic pair: compute the snapshot once per tick, store it
		// before any visible mutation, and draw BOTH the title and the
		// enabled bit from the stored value - so what the user reads and
		// what a click re-checks can never be a mixture of ticks (F7).
		snap := trayMuteSnapshot{Title: trayMuteTitle(state), Armed: state}
		muteSnap.Store(snap)
		mic.SetTitle(snap.Title)
		if trayMuteEnabled(snap.Armed) {
			mic.Enable()
		} else {
			mic.Disable()
		}
		// Mic vs. lights get different gates: the mic acts only on definitive
		// answers (see trayMuteEnabled), light actions only need a reachable
		// daemon (unknown is a mic-state concept).
		for _, it := range []*systray.MenuItem{lights, brightness, preset} {
			if trayActionsEnabled(state) {
				it.Enable()
			} else {
				it.Disable()
			}
		}
	}
}
