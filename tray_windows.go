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

	muted := systray.AddMenuItemCheckbox("Muted", "mute-everything: mic toggle + F24 meeting-app sweep (same flow as the Stream Deck mute key)", false)
	systray.AddSeparator()

	lights := systray.AddMenuItem("Toggle lights", "if any light is on, turn all off; otherwise turn all on")
	brightness := systray.AddMenuItem("Brightness", "set brightness on all lights")
	preset := systray.AddMenuItem("Light preset", "apply a preset on all lights")

	systray.AddSeparator()
	panel := systray.AddMenuItem("Light panel…", "open the full light controller in the browser")
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
	systray.SetOnRClick(func(m systray.IMenu) { _ = m.ShowMenu() })

	muted.Click(func() { go actions.onMicToggle() })
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

	go trayRefreshLoop(logger, refreshCh, header, muted, lights, brightness, preset)
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
// traystate.go mapping, and updates are applied only on state transitions,
// so the native calls (and the log line) happen exactly when the display
// changes. The icon switches only on definitive answers (trayIconFor):
// unknown or unreachable keeps the last icon.
func trayRefreshLoop(logger *slog.Logger, refreshCh <-chan struct{}, header, muted, lights, brightness, preset *systray.MenuItem) {
	first := true
	last := trayStateUnknown
	for range refreshCh {
		reply, err := askDaemon("status", udpAddr, lightClientTimeout)
		state := trayStateFor(reply, err)
		if !first && state == last {
			continue
		}
		first = false
		last = state
		logger.Info("status display", "title", trayTitle(state))
		switch trayIconFor(state) {
		case trayIconMutedMic:
			systray.SetIcon(trayIconMuted)
		case trayIconLiveMic:
			systray.SetIcon(trayIconLive)
		} // trayIconKeep: not a definitive answer - leave the current icon
		systray.SetTooltip(trayTitle(state))
		header.SetTitle(trayTitle(state))
		if trayMutedChecked(state) {
			muted.Check()
		} else {
			muted.Uncheck()
		}
		enabled := trayActionsEnabled(state)
		for _, it := range []*systray.MenuItem{muted, lights, brightness, preset} {
			if enabled {
				it.Enable()
			} else {
				it.Disable()
			}
		}
	}
}
