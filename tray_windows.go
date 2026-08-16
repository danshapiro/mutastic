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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

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

// --- click-time native title verification (ROUND-6: kills R4-F2/R5-F2) ---
//
// The fork dispatches a menu click by command ID ALONE (WM_COMMAND's
// wParam -> menuItems[id].click): the native row's title text is
// display-only metadata. And because the fork offers no remove/insert
// API, the "Saved settings" submenu recycles rows IN PLACE
// (syncSavedSettingsMenu re-stores the command slot, then repaints the
// native title), so a WM_COMMAND queued from a stale native row - one the
// user read as the OLD name - can otherwise execute the row's NEW slot
// command. The verification below reads the row's LIVE native title back
// from Windows at click time and executes the slotted command only when
// the native title still names that setting (traySettingsCmdMatchesTitle,
// the pure comparator in traystate.go). Refusal degrades to a no-op with
// one WARN (traySetSettingsClick logs it); the just-finished rebuild has
// already painted the truthful row for the next click.
//
// The fork exports neither the item's command ID nor its containing
// HMENU, so both are read from the fork's own bookkeeping via go:linkname:
// the package-level menuItems map (id <-> item; walked by pointer
// identity, guarded by its package-level RWMutex) and winTray's menuOf
// map (item id -> containing HMENU, populated by addOrUpdateMenuItem and
// guarded by muMenuOf). The fork's go.mod predates Go 1.23, so pull
// linknames into its unexported symbols remain permitted; a fork update
// that renames or reshapes these symbols fails at BUILD time (an
// unresolved linkname is a compile error), never silently at runtime.
// trayWinTrayPrefix mirrors the fork's winTray field-for-field up through
// menuOf/muMenuOf so menuOf sits at its true offset (later fields are not
// needed and are never touched).

//go:linkname traySystrayMenuItems github.com/energye/systray.menuItems
var traySystrayMenuItems map[uint32]*systray.MenuItem

//go:linkname traySystrayMenuItemsLock github.com/energye/systray.menuItemsLock
var traySystrayMenuItemsLock sync.RWMutex

// trayWinTrayPrefix mirrors the PREFIX of the fork's unexported winTray
// struct (systray_windows.go: Handle fields, loadedImages map, then
// menus/menuOf with their RWMutex guards) exactly, so the linknamed
// variable exposes menuOf/muMenuOf at their true offsets.
type trayWinTrayPrefix struct {
	instance, icon, cursor, window uintptr
	loadedImages                   map[string]uintptr
	muLoadedImages                 sync.RWMutex
	menus                          map[uint32]uintptr
	muMenus                        sync.RWMutex
	menuOf                         map[uint32]uintptr
	muMenuOf                       sync.RWMutex
}

//go:linkname trayWinTray github.com/energye/systray.wt
var trayWinTray trayWinTrayPrefix

var trayProcGetMenuItemInfoW = syscall.NewLazyDLL("user32.dll").NewProc("GetMenuItemInfoW")

// trayMenuItemInfoW is the full MENUITEMINFOW layout for MIIM_STRING
// reads (same shape the fork uses for its SetMenuItemInfoW writes).
type trayMenuItemInfoW struct {
	Size, Mask, Type, State     uint32
	ID                          uint32
	SubMenu, Checked, Unchecked uintptr
	ItemData                    uintptr
	TypeData                    *uint16
	Cch                         uint32
	BMPItem                     uintptr
}

// trayMenuItemNativeTitle reads the item's CURRENT native menu string via
// GetMenuItemInfoW with MIIM_STRING: the identifier form (fByPosition=0)
// keyed by the item's fork command ID, against the HMENU the fork
// recorded for that ID. Any lookup failure (unknown item, missing HMENU,
// API refusal) reports not-ok so the caller refuses: the safe direction.
func trayMenuItemNativeTitle(item *systray.MenuItem) (string, bool) {
	traySystrayMenuItemsLock.RLock()
	id, found := uint32(0), false
	for itemID, menuItem := range traySystrayMenuItems {
		if menuItem == item {
			id, found = itemID, true
			break
		}
	}
	traySystrayMenuItemsLock.RUnlock()
	if !found {
		return "", false
	}
	trayWinTray.muMenuOf.RLock()
	hMenu, ok := trayWinTray.menuOf[id]
	trayWinTray.muMenuOf.RUnlock()
	if !ok || hMenu == 0 {
		return "", false
	}
	const miimString = 0x00000040
	// The native title is always one of OUR OWN strings: an escaped
	// settings name (the name grammar caps raw names at 42 bytes, and
	// escaping at most doubles the "&"s) or a short placeholder, so a
	// 512-unit UTF-16 buffer can never truncate a real one - and if it
	// ever did, the truncated read would fail the exact comparison and
	// refuse, still the safe direction.
	var buf [512]uint16
	mi := trayMenuItemInfoW{Mask: miimString, TypeData: &buf[0], Cch: uint32(len(buf))}
	mi.Size = uint32(unsafe.Sizeof(mi))
	res, _, _ := trayProcGetMenuItemInfoW.Call(
		hMenu,
		uintptr(id),
		0, // fByPosition=FALSE: uItem is the item identifier, not a position
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 || mi.Cch > uint32(len(buf)) {
		return "", false
	}
	return string(utf16.Decode(buf[:mi.Cch])), true
}

// trayVerifySettingsItemTitle is the Windows arm of the ROUND-6
// click-time check wired into traySetSettingsClick: the row's live native
// title must still name the setting the slot command applies. It compares
// the ESCAPED form of the command's raw name against the native title
// (the native title is our escaped display string; Windows stores the
// literal text we SetTitle'ed, "&&" included). Mismatch, an unreadable
// title, or any lookup error all return false - the click no-ops and the
// next rebuild/poll paints the truthful row.
func trayVerifySettingsItemTitle(item *systray.MenuItem, cmd string) bool {
	title, ok := trayMenuItemNativeTitle(item)
	if !ok {
		return false
	}
	return traySettingsCmdMatchesTitle(cmd, title)
}

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
		// Delta round 4 re-litigated this exact point (R4-F1: a click in
		// the paint/store gap no-ops) and the submenu slot rebuild race
		// (R4-F2, see syncSavedSettingsMenu), and REJECTED both as blocking
		// with recorded analysis: after the R3-F2 flip => strict no-op
		// reversion the divergence can only produce a probe-gated no-op,
		// NEVER a wrong action, and the next 2 s poll repaints the premise
		// from fresh truth so the retried click performs the displayed
		// action; driving the click from the probe instead was evaluated
		// and rejected because it reintroduces the R3-F2 physical-press
		// double-sweep hazard (reviews/delta/review-log.md, round 4).
		// ROUND-6 then pinned the probe-gated mute rule in the plan's
		// explicit constraints ("Precision amendment (review convergence,
		// ROUND-6)") and eliminated the submenu half outright: the
		// settings closure now re-reads the row's live native title
		// (trayVerifySettingsItemTitle) and refuses unless it still names
		// the slot's setting.
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
			savedChildren, savedSpecs = syncSavedSettingsMenu(savedSettings, savedChildren, savedSpecs, want, lightCmd, logger)
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
// - the only place Click is ever assigned: the closure
// (traySetSettingsClick, traystate.go) reads the slot at click time,
// verifies the row's LIVE native title still names the slotted setting
// (trayVerifySettingsItemTitle, ROUND-6), and only then dispatches.
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
// ROUND-6 closes the recorded R3-F4/R4-F2/R5-F2 residual ("a click queued
// in the same Win32 message turn as a name-set rebuild applies the slot's
// new name"): the closure's native-title verification reads the row's
// ESCAPED title back out of Windows and compares it against the slot's
// name, so a WM_COMMAND dispatched from a stale native row can now only
// REFUSE (one WARN, traySetSettingsClick) - never execute the row's new
// command. The remaining residual is inertness only: a refusal on a
// legitimate click when its dispatch raced the rebuild by one turn. No
// automated test can observe any of this on Linux (no native menu, no
// message turn), so the ordering and the native read-back are
// review-verified; the pure comparator is unit-tested in traystate_test.go.
func newSavedMenuChild(parent *systray.MenuItem, title, command string, lightCmd func(string) func(), logger *slog.Logger) *savedMenuChild {
	child := &savedMenuChild{}
	child.slot.Store(command) // bound BEFORE the create call: never an uninitialized slot behind a revealed item
	child.item = parent.AddSubMenuItem(title, "")
	child.item.Click(traySetSettingsClick(child.item, &child.slot, lightCmd, logger))
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
// Historical residual (R3-F4/R4-F2/R5-F2, verbatim): "a click queued in
// the same Win32 message turn as a name-set rebuild applies the slot's new
// name". ROUND-6 eliminates the wrong-apply outcome outright: the stable
// closure (traySetSettingsClick) re-reads the row's LIVE native title via
// GetMenuItemInfoW and compares it to the slot's name
// (trayVerifySettingsItemTitle/traySettingsCmdMatchesTitle), so a WM_COMMAND
// dispatched from a stale native row can NEVER execute the row's new
// command - it refuses with one WARN, and this very rebuild has already
// painted the truthful row for the retried click. What remains is at most
// a refused click in that single-turn window (inert, never wrong). The
// slot-first / title-second ordering is retained deliberately: combined
// with verification, slot-first means the window's native title is always
// the STALE one, so the check refuses; reversing the order would instead
// arm a click on the row's OLD name against a title already repainted -
// again refused, but with a window where title and slot point at the new
// name while the displayed-menu-a-repaint-ago said otherwise. No automated
// test can observe the ordering on Linux (no native menu, no message
// turn), so it remains review-verified; only the pure comparator is
// unit-tested.
func syncSavedSettingsMenu(parent *systray.MenuItem, children []*savedMenuChild, cached, want []trayMenuSpec, lightCmd func(string) func(), logger *slog.Logger) ([]*savedMenuChild, []trayMenuSpec) {
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
			child = newSavedMenuChild(parent, spec.Title, command, lightCmd, logger)
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
