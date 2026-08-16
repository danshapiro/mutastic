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
	// trayPanelURL is the mutastic control panel (lights + mic + saved
	// settings) served by `mutastic ui` (ui.go); left-clicking the tray
	// icon opens it.
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
	// the click's armed premise are ONE atomic pair (F7), computed once per
	// tick: the refresh loop paints the native title/enabled bit FIRST and
	// stores the pair LAST (see trayRefreshLoop - store-last keeps a racing
	// click on a stale-or-current premise, degrading to the probe-gated
	// no-op instead of executing the opposite of the just-painted verb).
	// The click handler loads the pair back exactly once. The initial
	// neutral snapshot matches the item's startup title + disabled state,
	// so a click before the first tick reads a premise that cannot fire.
	var muteSnap atomic.Value
	muteSnap.Store(trayMuteSnapshot{Title: trayMuteTitle(trayStateUnknown), Armed: trayStateUnknown})
	loadMuteSnap := func() trayMuteSnapshot { return muteSnap.Load().(trayMuteSnapshot) }
	mic := systray.AddMenuItem(trayMuteTitle(trayStateUnknown), "mute-everything: the displayed mic verb + F24 meeting-app sweep (same flow as the Stream Deck mute key)")
	systray.AddSeparator()

	lights := systray.AddMenuItem("Toggle lights", "if any light is on, turn all off; otherwise turn all on")
	brightness := systray.AddMenuItem("Brightness", "set brightness on all lights")
	preset := systray.AddMenuItem("Light preset", "apply a preset on all lights")

	// "Saved settings" is a live view of the daemon's store, NOT a static
	// action item: it is deliberately left out of the startup Disable loop
	// below AND the refresh loop's trayActionsEnabled gating - the parent
	// stays clickable so its grayed placeholder children remain visible
	// when the daemon or the store is down; the CHILDREN carry the enabled
	// state. It starts childless; the refresh loop's first tick reconciles
	// it to a placeholder.
	savedSettings := systray.AddMenuItem("Saved settings", "saved named light settings (polled every 2 s); click a name to apply it")

	// Action items start disabled; the refresh loop arms them (the light
	// actions on the first reachable poll, the mic item on the first
	// DEFINITIVE mic answer, per trayMuteEnabled) - so no click can fire in
	// the startup window.
	for _, it := range []*systray.MenuItem{mic, lights, brightness, preset} {
		it.Disable()
	}

	systray.AddSeparator()
	panel := systray.AddMenuItem("Panel…", "open the mutastic control panel in the browser")
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

	go trayRefreshLoop(logger, refreshCh, header, mic, lights, brightness, preset, savedSettings, &muteSnap, lightCmd)
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
func trayRefreshLoop(logger *slog.Logger, refreshCh <-chan struct{}, header, mic, lights, brightness, preset, savedSettings *systray.MenuItem, muteSnap *atomic.Value, lightCmd func(string) func()) {
	first := true
	last := trayStateUnknown
	tick := 0
	// The Saved settings submenu's retained children (shown first, in menu
	// order, then the hidden reuse pool) and the spec list the shown prefix
	// renders. Empty until the first tick reconciles; two nil specs compare
	// equal, so only a real poll difference triggers a rebuild.
	var savedChildren []*savedMenuChild
	var savedSpecs []trayMenuSpec
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
		// ONE atomic pair, computed once per tick and drawn from the same
		// value - so what the user reads and what a click re-checks can
		// never be a mixture of ticks (F7). The NATIVE updates land FIRST
		// (SetTitle, then Enable/Disable per the computed pair) and the
		// premise snapshot is stored LAST: a click executes concurrently in
		// its own goroutine, and store-first could arm the click to the NEW
		// pair while the menu still shows the OLD verb. Stored last, the
		// premise is never FRESHER than the painted title; and with flip =>
		// NO-OP (R3-F2: a definitive-OPPOSITE probe runs no verb and no
		// sweep), ANY paint/premise race degrades to the probe-gated no-op
		// and NEVER an opposite action (R3-F3): a click holding a stale
		// premise meets a fresh probe that mismatches it (a flip or a
		// decline), and a mismatching probe runs nothing by rule. The
		// residual sub-poll window - the PAINTED title itself lagging the
		// daemon - is structurally unkillable without a read-back of what
		// the user saw; the probe gate bounds it (any state change after
		// premise capture mismatches the probe and the click no-ops).
		// Linux can't drive the native menu, so this paint/store ordering
		// is review-verified only - no automated test can observe it.
		snap := trayMuteSnapshot{Title: trayMuteTitle(state), Armed: state}
		mic.SetTitle(snap.Title)
		if trayMuteEnabled(snap.Armed) {
			mic.Enable()
		} else {
			mic.Disable()
		}
		muteSnap.Store(snap)
		// Mic vs. lights get different gates: the mic acts only on definitive
		// answers (see trayMuteEnabled), light actions only need a reachable
		// daemon (unknown is a mic-state concept). The savedSettings submenu
		// is NOT gated here: the parent stays clickable so grayed
		// placeholder children stay visible while the daemon is down.
		for _, it := range []*systray.MenuItem{lights, brightness, preset} {
			if trayActionsEnabled(state) {
				it.Enable()
			} else {
				it.Disable()
			}
		}
		// Poll the settings store once per tick (the daemon's logCommand
		// latch keeps the steady state quiet in daemon.log) and rebuild the
		// submenu children exactly when the rendered spec list changes.
		listReply, listErr := askDaemon(traySavedSettingsListCmd, udpAddr, lightClientTimeout)
		want := traySavedSettings(trayParseSettingsList(listReply, listErr))
		if !traySameMenuSpecs(savedSpecs, want) {
			savedChildren, savedSpecs = syncSavedSettingsMenu(savedSettings, savedChildren, savedSpecs, want, lightCmd)
		}
	}
}

// savedMenuChild is one retained "Saved settings" submenu child: the native
// item plus an ATOMIC command slot that its click closure reads at click
// time. The closure is assigned ONCE at AddSubMenuItem creation and is
// NEVER rebound (R2-F3): writing the fork's click field while the native
// message pump can invoke it is a data race (the pump may run the OLD
// binding while Go assigns the NEW one), so rebuilds never touch the
// binding - they only SetTitle/Enable/Disable/Show and store the slot. The
// slot holds the current "light settings apply <name>" command; "" means
// unbound (a placeholder or a hidden pool orphan - a click is then a no-op,
// doubly safe because such items are also disabled/hidden).
type savedMenuChild struct {
	item *systray.MenuItem
	slot atomic.Value // string: current apply command; "" = unbound
}

// newSavedMenuChild creates one submenu child with its STABLE click closure
// - the only place Click is ever assigned: the closure reads the slot at
// click time and dispatches whatever command is current then.
//
// R3-F4 ordering discipline: BOTH click-time inputs are assigned around the
// fork's create call, on this same refresh goroutine, with no channel
// hand-off or other deliberate yield between the assignments and the
// item's first revealing update. The command slot is stored FIRST - a
// plain Go atomic write with no native side effect, so the FINAL command
// is bound before the item exists; the fork's AddSubMenuItem is its only
// create call and inserts the item into the submenu's HMENU on the spot
// (there is no create-without-insert API), so for a brand-new child that
// insert is the first revealing update; the Click closure is assigned on
// the statement IMMEDIATELY following the create call. A click dispatched
// in that one-statement window finds click == nil and no-ops (the fork's
// dispatcher skips a nil binding) - never a wrong apply. From then on the
// closure is NEVER rebound; rebuilds only re-store the slot (before each
// revealing update, see syncSavedSettingsMenu).
//
// Recorded residual (R3-F4, verbatim): a click queued in the same Win32
// message turn as a name-set rebuild applies the slot's new name. The
// window is single-turn and user-action-triggered (a click must be queued
// natively in the exact millisecond an external name-set change rebuilds
// the menu), and the consequence is reversible - apply the intended name
// again. No automated test can observe this: on Linux neither the fork's
// native HMENU nor its message-turn dispatch exists, so this discipline is
// review-verified only.
func newSavedMenuChild(parent *systray.MenuItem, title, command string, lightCmd func(string) func()) *savedMenuChild {
	child := &savedMenuChild{}
	child.slot.Store(command) // bound BEFORE the create call: never an uninitialized slot behind a revealed item
	child.item = parent.AddSubMenuItem(title, "")
	child.item.Click(func() {
		if cmd, _ := child.slot.Load().(string); cmd != "" {
			lightCmd(cmd)()
		}
	})
	return child
}

// syncSavedSettingsMenu rebuilds the "Saved settings" submenu's children to
// render want (called only when traySameMenuSpecs reported a change). The
// fork has no remove/insert API, so it recycles: every currently-SHOWN
// child (the children[:len(cached)] prefix; the tail is already hidden) is
// hidden back into a reuse pool and its slot cleared, then each wanted spec
// reuses a hidden orphan first and only creates a NEW child (stable closure
// bound at creation, newSavedMenuChild) when the pool is empty. Per item
// the ordering is slot FIRST (a recycled orphan re-stores it; a NEW child
// arrives from newSavedMenuChild with slot AND closure already assigned),
// then SetTitle, then Enable/Disable, then Show LAST - the slot store is a
// plain atomic word with no native side effect, while every
// update()-backed call (SetTitle, Enable, Disable, and Show itself all
// route through addOrUpdateMenuItem, which inserts any item missing from
// the visible list) reveals the item on the spot; both assignments happen
// in this same goroutine with no yield between them and the item's first
// revealing update, so the command and the binding are final before the
// item's reveal renders it (R3-F4). The display/command split is
// raw-vs-Title: spec.Title is the menu-ESCAPED display string ("&" -> "&&"),
// spec.raw the VERBATIM saved name - the command must use raw, because the
// daemon's store keys names raw and an escaped title would apply nothing.
// Unused orphans stay hidden (never displayed, slot "" so unbound) and are
// returned after the shown prefix for the next reconcile: every reconcile
// is change-gated and orphans are recycled BEFORE any new item is created,
// so retained items stay bounded by the menu size plus one change-width
// even with delete churn. The returned spec copy is the new cached render.
//
// Recorded residual (R3-F4, verbatim): a click queued in the same Win32
// message turn as a name-set rebuild applies the slot's new name - a
// single-turn, user-action-triggered window whose consequence is reversible
// (apply the intended name again); beyond that one turn the on-screen title
// and the slot command agree. No automated test can observe the ordering on
// Linux (no native menu, no message turn), so it is review-verified only.
func syncSavedSettingsMenu(parent *systray.MenuItem, children []*savedMenuChild, cached, want []trayMenuSpec, lightCmd func(string) func()) ([]*savedMenuChild, []trayMenuSpec) {
	for _, child := range children[:len(cached)] {
		child.item.Hide()
		child.slot.Store("") // a pooled orphan is unbound: no stale command can fire from the pool
	}
	pool := children
	reused := min(len(pool), len(want))
	shown := make([]*savedMenuChild, 0, len(want))
	for i, spec := range want {
		// command is the child's new slot content: an ENABLED real entry's
		// apply verb on the VERBATIM raw name; "" for placeholders (an
		// unbound, disabled row can never fire).
		command := ""
		if spec.Enabled {
			command = "light settings apply " + spec.raw
		}
		var child *savedMenuChild
		if i < reused {
			child = pool[i]
			// R3-F4: the slot is re-stored BEFORE this item's first
			// revealing update of the reconcile (the SetTitle below) -
			// same goroutine, no yield between; the stable closure has
			// been bound since creation.
			child.slot.Store(command)
		} else {
			// A NEW child arrives with its slot AND its stable closure
			// already assigned (see newSavedMenuChild) before any
			// SetTitle/Enable/Show below.
			child = newSavedMenuChild(parent, spec.Title, command, lightCmd)
		}
		child.item.SetTitle(spec.Title)
		if spec.Enabled {
			child.item.Enable()
		} else {
			child.item.Disable()
		}
		child.item.Show()
		shown = append(shown, child)
	}
	// Retain the unused pool orphans (hidden, unbound) after the shown prefix.
	retained := append(shown, pool[reused:]...)
	newCached := make([]trayMenuSpec, len(want))
	copy(newCached, want)
	return retained, newCached
}
