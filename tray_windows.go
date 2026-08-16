//go:build windows

package main

import (
	_ "embed"
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

// --- click-time/paint-time native verification (ROUND-6/ROUND-7/ROUND-13) ---
//
// The fork dispatches a menu click by command ID ALONE (WM_COMMAND's
// wParam -> menuItems[id].click): the native row's title text is
// display-only metadata. ROUND-13 (R12-F1) governs the shape: every
// "Saved settings" row is bound to ONE name for the process lifetime -
// its atomic command slot is stored ONCE at creation ("light settings
// apply <name>") and never changed to another name (the fork's immutable
// IDs stay put, so the global ID counter never approaches WM_COMMAND's
// 16-bit truncation point: distinct names ever seen consume one ID
// each). While the row is hidden - its name deleted, or every name
// disarmed behind a placeholder regime - the slot is held CLEARED, so a
// WM_COMMAND queued from the pre-change display and dispatched late hits
// "" and no-ops, and a re-save of the same name re-shows the SAME row
// with the slot restored to the identical command. The read-back below
// remains as the belt to that slot discipline's suspenders (R6-F1's
// "keep the existing native title verification"), now comparing the live
// title against the row's ETERNALLY-bound name, and additionally
// covers the mic item (R6-F3): it reads the row's LIVE native title AND
// disabled bit back from Windows, so the refresh loop stores the mic
// armed premise only when the paint actually landed and a click executes
// only when the row it fired on still shows exactly the loaded snapshot.
// Refusal degrades to a no-op with one WARN.
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

// trayMenuItemInfoW is the full MENUITEMINFOW layout for MIIM_STRING |
// MIIM_STATE reads (same shape the fork uses for its SetMenuItemInfoW
// writes).
type trayMenuItemInfoW struct {
	Size, Mask, Type, State     uint32
	ID                          uint32
	SubMenu, Checked, Unchecked uintptr
	ItemData                    uintptr
	TypeData                    *uint16
	Cch                         uint32
	BMPItem                     uintptr
}

// trayMenuItemNativeState is the SHARED winapi reader behind every
// click-time/paint-time native verification (R6-F1 settings rows and
// R6-F3 mic item): one GetMenuItemInfoW call returns the item's CURRENT
// native menu string (MIIM_STRING) AND its native disabled bit
// (MIIM_STATE, the fork writes MFS_DISABLED=0x3 when disabled). The
// identifier form (fByPosition=0) is keyed by the item's fork command
// ID, against the HMENU the fork recorded for that ID. Any lookup
// failure (unknown item, missing HMENU, API refusal) reports not-ok so
// the caller refuses: the safe direction.
func trayMenuItemNativeState(item *systray.MenuItem) (title string, disabled, ok bool) {
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
		return "", false, false
	}
	trayWinTray.muMenuOf.RLock()
	hMenu, menuOK := trayWinTray.menuOf[id]
	trayWinTray.muMenuOf.RUnlock()
	if !menuOK || hMenu == 0 {
		return "", false, false
	}
	const (
		miimState   = 0x00000001
		miimString  = 0x00000040
		mfsDisabled = 0x00000003 // mirrors the fork's disabled-state bits
	)
	// The native title is always one of OUR OWN strings: an escaped
	// settings name (the name grammar caps raw names at 42 bytes, and
	// escaping at most doubles the "&"s) or a short placeholder/verb, so
	// a 512-unit UTF-16 buffer can never truncate a real one - and if it
	// ever did, the truncated read would fail the exact comparison and
	// refuse, still the safe direction.
	var buf [512]uint16
	mi := trayMenuItemInfoW{Mask: miimString | miimState, TypeData: &buf[0], Cch: uint32(len(buf))}
	mi.Size = uint32(unsafe.Sizeof(mi))
	res, _, _ := trayProcGetMenuItemInfoW.Call(
		hMenu,
		uintptr(id),
		0, // fByPosition=FALSE: uItem is the item identifier, not a position
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 || mi.Cch > uint32(len(buf)) {
		return "", false, false
	}
	return string(utf16.Decode(buf[:mi.Cch])), mi.State&mfsDisabled == mfsDisabled, true
}

// trayVerifySettingsItemTitle is the Windows arm of the click-time check
// wired into traySetSettingsClick: the row's live native title must
// still name the setting the slot command applies. It compares the
// ESCAPED form of the command's raw name against the native title (the
// native title is our escaped display string; Windows stores the literal
// text we SetTitle'ed, "&&" included). Mismatch, an unreadable title, or
// any lookup error all return false - the click no-ops and the next
// rebuild/poll paints the truthful row.
func trayVerifySettingsItemTitle(item *systray.MenuItem, cmd string) bool {
	title, _, ok := trayMenuItemNativeState(item)
	if !ok {
		return false
	}
	return traySettingsCmdMatchesTitle(cmd, title)
}

// trayVerifyArmed is the Windows arm of the R6-F3 armed verification for
// the mic item, used BOTH after painting (the refresh loop stores the
// armed premise ONLY when the live native row reads back the painted
// title AND enablement - the fork reports SetMenuItemInfoW failures only
// through its own log, so without this read-back a failed SetTitle or
// Enable would leave the visible verb diverging from the armed snap) and
// at click time (muteClick refuses when the live row no longer matches
// the loaded snapshot - a mid-paint click can then never perform the
// opposite of the displayed label). The portable stub is true. The
// winapi paths here are covered by the Windows build plus the runbook's
// tray steps; only the pure comparator (trayArmedMatchesNative) is
// unit-tested on Linux.
func trayVerifyArmed(item *systray.MenuItem, snap trayMuteSnapshot) bool {
	title, disabled, ok := trayMenuItemNativeState(item)
	if !ok {
		return false
	}
	return trayArmedMatchesNative(snap, title, disabled)
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
	// the click's armed premise are ONE atomic pair (F7), computed once
	// per tick: the refresh loop paints the native title/enabled bit
	// FIRST, stores the pair LAST, and - ROUND-7, R6-F3 - stores it ONLY
	// when trayVerifyArmed reads the exact painted title AND enablement
	// back from the live native row (a failed native SetTitle/Enable can
	// then never arm the click to the opposite of what is on screen; the
	// next 2 s poll retries the paint). The click handler loads the pair
	// back exactly once and re-VERIFIES the live row against it before
	// asking the daemon. The initial neutral snapshot matches the item's
	// startup title + disabled state, so a click before the first tick
	// reads a premise that cannot fire.
	var muteSnap atomic.Value
	muteSnap.Store(trayMuteSnapshot{Title: trayMuteTitle(trayStateUnknown), Armed: trayStateUnknown})
	loadMuteSnap := func() trayMuteSnapshot { return muteSnap.Load().(trayMuteSnapshot) }
	mic := systray.AddMenuItem(trayMuteTitle(trayStateUnknown), "mute-everything: the displayed mic verb + F24 meeting-app sweep, atomically premised daemon-side (same flow as the Stream Deck mute key)")
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
	// The tray needs no key injector of its own: R6-F2 moved the mic
	// click's F24 meeting-app sweep DAEMON-side (the atomic conditional
	// verbs mute-if/unmute-if perform the absolute verb AND the sweep in
	// one step), and physical-button sweeps were always daemon-side.
	actions := &trayActions{
		ask:           func(cmd string) (string, error) { return askDaemon(cmd, udpAddr, lightClientTimeout) },
		openPanel:     func() error { return openBrowser(trayPanelURL) },
		stopPanel:     func() error { return stopLightPanel(trayPanelURL) },
		requestQuit:   systray.Quit,
		signalRefresh: signalRefresh,
		logger:        logger,
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

	// The mic click verifies the LIVE native row against the loaded
	// snapshot (R6-F3) before its one conditional-verb daemon call; the
	// verify function closes over the mic item.
	//
	// R11-F1: the armed snapshot is captured SYNCHRONOUSLY inside the
	// native click callback (the fork's pump thread, at dispatch time) and
	// passed BY VALUE into the async work - the callback shape is exactly
	// func(){ s := load(); go act(s) }. Spawning the goroutine FIRST and
	// loading inside it (the pre-round-11 glue) raced the refresh loop: a
	// repaint that landed between the click and the goroutine's load would
	// store the OPPOSITE pair, and the late load would serve the NEW
	// premise to the OLD click - the native verify would then validate the
	// NEW paint and the daemon could execute the new conditional verb for
	// a click the user made against the old label. With the capture at
	// dispatch time, any mid-flight divergence (a repaint after the click,
	// a daemon flip) is caught by the click's own live-row verify or the
	// daemon's conditional premise and degrades to a refusal - never to
	// the opposite action. Tests pin the refusal semantics through this
	// capture boundary by injecting a stale snapshot directly (traystate
	// tests).
	mic.Click(func() {
		snap := loadMuteSnap()
		go actions.muteClick(snap, func(s trayMuteSnapshot) bool { return trayVerifyArmed(mic, s) })
	})
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
	// The Saved settings submenu's permanent row<->name IDENTITY map
	// (R12-F1, ROUND-13; the pure discipline lives in traystate.go's
	// trayPlanSettingsSync header): a row, once created for a name, stays
	// bound to that name for the process lifetime - its command slot is
	// stored once at creation and never changed to another name, and
	// hidden rows (deleted names, placeholder regimes) hold their slot
	// cleared so stale clicks no-op. Display order is creation order BY
	// DESIGN - the fork shows visible rows sorted by their immutable IDs
	// and IDs are monotonic in creation - so the tray lists saved
	// settings in first-created order (the web UI sorts alphabetically);
	// the daemon's sorted wire order deliberately does NOT dictate tray
	// order. Distinct names ever seen consume one fork ID each
	// (deletions and re-saves consume zero), which keeps the global
	// counter far below WM_COMMAND's 16-bit truncation point, and the
	// defensive 1024-name identity-map hard bound stalls with one WARN
	// instead of ever growing toward it. The submenu starts childless;
	// the first tick reconciles it to a placeholder.
	savedMenu := newSavedSettingsMenu(savedSettings, lightCmd, logger)
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
		// NO-OP (R3-F2: a definitive-OPPOSITE premise runs no verb and no
		// sweep), ANY paint/premise race degrades to the premised no-op
		// and NEVER an opposite action (R3-F3). History: delta round 4's
		// re-litigation of the paint/store gap (R4-F1) was REJECTED as
		// blocking with recorded analysis (reviews/delta/review-log.md,
		// round 4); ROUND-6 pinned the premised mute rule in the plan's
		// explicit constraints ("Precision amendment (review convergence,
		// ROUND-6)"); ROUND-7 carries it onto the daemon-side ATOMIC
		// conditional verb (R6-F2, see muteClick), so the premise check
		// and the action are one step.
		// ROUND-7, R6-F3: the paint is then READ BACK from the live native
		// row (trayVerifyArmed - title AND disabled bit), and the snapshot
		// is stored ONLY when the read-back matches the paint exactly.
		// The fork reports SetMenuItemInfoW failures only through its own
		// log; without the read-back a failed SetTitle or Enable would
		// leave the visible verb diverging from the armed premise, and a
		// click whose premise matched the daemon would execute the
		// OPPOSITE of the displayed label. On a mismatch the previous
		// verified snapshot stays (the display still matches it - the
		// failed paint never landed), one WARN is logged, and the next 2 s
		// poll retries the paint, so a transient native refusal
		// self-heals. muteClick re-verifies the same pair at click time,
		// so a mid-paint click can only refuse, never act wrong.
		// Linux can't drive the native menu, so this paint/verify/store
		// ordering is review-verified only - no automated test can observe
		// it (the pure comparator trayArmedMatchesNative IS unit-tested).
		snap := trayMuteSnapshot{Title: trayMuteTitle(state), Armed: state}
		mic.SetTitle(snap.Title)
		if trayMuteEnabled(snap.Armed) {
			mic.Enable()
		} else {
			mic.Disable()
		}
		if trayVerifyArmed(mic, snap) {
			muteSnap.Store(snap)
		} else {
			logger.Warn("mute item paint did not verify against the live native row (title/enabled mismatch); the armed premise stays at the last verified paint so the display and the click gate never diverge (the next poll retries)", "title", snap.Title, "armed", trayStateName(snap.Armed))
		}
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
		// latch keeps the steady state quiet in daemon.log) and reconcile
		// the submenu exactly when the rendered spec list changes (an
		// order-only change of the same name SET reconciles to a no-op:
		// tray order is creation order, not daemon order).
		listReply, listErr := askDaemon(traySavedSettingsListCmd, udpAddr, lightClientTimeout)
		savedMenu.sync(traySavedSettings(trayParseSettingsList(listReply, listErr)))
	}
}

// savedMenuChild is one "Saved settings" submenu child: the native item
// plus an ATOMIC command slot that its click closure reads at click time.
// The closure is assigned ONCE at AddSubMenuItem creation and is NEVER
// rebound (R2-F3): writing the fork's click field while the native
// message pump can invoke it is a data race (the pump may run the OLD
// binding while Go assigns the NEW one), so no rebuild ever touches the
// binding - only the slot, and only ever with THIS row's own command.
//
// Under ROUND-13's permanent row<->name identity (R12-F1), a NAME-BOUND
// child's slot holds exactly one command for the process lifetime -
// "light settings apply <name>" for the row's own eternally-bound name:
// stored once at creation, held CLEARED ("") while the row is hidden (its
// name deleted, or every name disarmed behind a placeholder regime), and
// restored to the IDENTICAL command when the same name re-appears. It is
// never set to another name, so a stale WM_COMMAND dispatched from a
// pre-change display can no-op ("") but can NEVER apply a different
// setting than the row displayed - the wrong-apply shape of the old
// positional rebind (R12-F1) is structurally closed. The two PLACEHOLDER
// children are never name-bound: their slots stay "" permanently, so
// their (disabled) rows can never fire.
type savedMenuChild struct {
	item  *systray.MenuItem
	slot  atomic.Value // string: this row's own apply command; "" while hidden (always "" for a placeholder)
	name  string       // the verbatim saved name this row applies for the process lifetime; "" for a placeholder
	shown bool         // visible right now; read/written only on the refresh-loop goroutine
}

// newSavedMenuChild creates the ONE identity row a saved name gets for
// the process lifetime - the only place its stable click closure
// (traySetSettingsClick, traystate.go: reads the slot at click time,
// verifies the row's LIVE native title still names the slotted setting
// via trayVerifySettingsItemTitle, and only then dispatches) and its
// command slot are ever assigned.
//
// R3-F4 ordering discipline: BOTH click-time inputs are assigned around the
// fork's create call, on this same refresh goroutine, with no channel
// hand-off or other deliberate yield between the assignments and the
// item's first revealing update. The command slot is stored FIRST - a
// plain Go atomic write with no native side effect, so the row's FINAL,
// ETERNAL command is bound before the item exists; the fork's
// AddSubMenuItem is its only create call and inserts the item into the
// submenu's HMENU on the spot (there is no create-without-insert API),
// so for a brand-new child that insert is the first revealing update; the
// Click closure is assigned on the statement IMMEDIATELY following the
// create call; Enable follows (the row arrives usable - real names are
// always enabled; only the never-name-bound placeholders stay gray). A
// click dispatched in that one-statement window finds click == nil and
// no-ops (the fork's dispatcher skips a nil binding) - never a wrong
// apply.
//
// The row's name binding is PERMANENT from this point: the slot command
// stored here is the only non-"" value the slot will ever hold, and the
// click-time native-title verification compares the live title against
// THIS name for the row's whole life. No automated test can observe any
// of this on Linux (no native menu, no message turn), so the ordering and
// the native read-back are review-verified; the pure planner and
// comparators are unit-tested in traystate_test.go.
func newSavedMenuChild(parent *systray.MenuItem, spec trayMenuSpec, lightCmd func(string) func(), logger *slog.Logger) *savedMenuChild {
	child := &savedMenuChild{name: spec.raw, shown: true}
	child.slot.Store(traySettingsApplyPrefix + spec.raw) // bound BEFORE the create call, and eternally: never another name
	child.item = parent.AddSubMenuItem(spec.Title, "")
	child.item.Click(traySetSettingsClick(child.item, &child.slot, lightCmd, logger))
	child.item.Enable()
	return child
}

// newSavedSettingsPlaceholder creates one of the two STATIC placeholder
// rows ("(no saved settings)", "(settings unavailable)"): never
// name-bound, permanently "" slot, disabled for life. Its click can never
// fire; the row exists only to be shown/hidden with the submenu's regime
// (a real saved name equal to a placeholder string gets its own ENABLED
// identity row instead - the placeholder is a DISTINCT row).
func newSavedSettingsPlaceholder(parent *systray.MenuItem, title string, lightCmd func(string) func(), logger *slog.Logger) *savedMenuChild {
	child := &savedMenuChild{shown: true}
	child.slot.Store("")
	child.item = parent.AddSubMenuItem(title, "")
	child.item.Click(traySetSettingsClick(child.item, &child.slot, lightCmd, logger))
	child.item.Disable()
	return child
}

// savedSettingsMenu owns the whole "Saved settings" submenu under
// ROUND-13's permanent row<->name identity discipline (R12-F1; the pure
// decision logic is trayPlanSettingsSync in traystate.go): the name->row
// identity map persisting across rebuilds, the rows in creation order (==
// ascending immutable fork-ID order - creation is append-only and the fork
// displays visible rows sorted by ID, so creation order IS the display
// order BY DESIGN), the two static placeholder rows (created lazily on
// first use), the current placeholder regime, and the last reconciled
// spec list as the change gate. All fields are touched only on the
// refresh loop's goroutine, except the children's atomics at click time.
type savedSettingsMenu struct {
	parent        *systray.MenuItem
	rows          []*savedMenuChild // name-bound rows in creation order (== ascending fork-ID order)
	byName        map[string]*savedMenuChild
	phEmpty       *savedMenuChild // lazily created "(no saved settings)" row
	phUnavailable *savedMenuChild // lazily created "(settings unavailable)" row
	phShown       traySettingsPlaceholder
	specs         []trayMenuSpec // last reconciled render (the steady-state change gate)
	lightCmd      func(string) func()
	logger        *slog.Logger
}

func newSavedSettingsMenu(parent *systray.MenuItem, lightCmd func(string) func(), logger *slog.Logger) *savedSettingsMenu {
	return &savedSettingsMenu{
		parent:   parent,
		byName:   map[string]*savedMenuChild{},
		phShown:  trayPhNone,
		lightCmd: lightCmd,
		logger:   logger,
	}
}

// phRow returns the placeholder row for a regime, or nil when it has never
// been needed (placeholder rows are created lazily on first use) - and nil
// for trayPhNone, which has no row.
func (m *savedSettingsMenu) phRow(ph traySettingsPlaceholder) *savedMenuChild {
	switch ph {
	case trayPhEmpty:
		return m.phEmpty
	case trayPhUnavailable:
		return m.phUnavailable
	}
	return nil
}

// ensurePlaceholder lazily creates the placeholder row for a regime on
// first use: static rows are created at most once each, so placeholder
// flips consume zero fork IDs thereafter. trayPhNone has no row and
// returns nil.
func (m *savedSettingsMenu) ensurePlaceholder(ph traySettingsPlaceholder) *savedMenuChild {
	switch ph {
	case trayPhEmpty:
		if m.phEmpty == nil {
			m.phEmpty = newSavedSettingsPlaceholder(m.parent, trayNoSavedSettingsTitle, m.lightCmd, m.logger)
		}
		return m.phEmpty
	case trayPhUnavailable:
		if m.phUnavailable == nil {
			m.phUnavailable = newSavedSettingsPlaceholder(m.parent, traySettingsUnavailableTitle, m.lightCmd, m.logger)
		}
		return m.phUnavailable
	}
	return nil
}

// sync reconciles the submenu to want (called once per poll tick): the
// mechanical executor of trayPlanSettingsSync (traystate.go), running the
// planned action classes on the refresh loop's goroutine:
//
//  1. HIDES, for names no longer present: the slot is cleared FIRST (a
//     stale WM_COMMAND dispatched from the pre-change display loads ""
//     and no-ops from this instant), then the row is disabled, then
//     hidden. The row's name binding is KEPT - it is never re-titled and
//     never slotted to another name.
//  2. the placeholder FLIP: hide the old placeholder row, show the new
//     one (lazily created on first use). Placeholders are never
//     name-bound and never enabled.
//  3. SHOWS, for current names whose identity row already exists: title
//     re-asserted (same name => same escaped title), enabled, shown, and
//     the slot RESTORED LAST, after the paint, to the row's
//     eternally-bound command (before it lands the row can only no-op;
//     after it lands the native title already names it, so the ROUND-6
//     click-time title verification passes only the truthful pair).
//  4. CREATES, for first-seen names: one fresh row per distinct name,
//     eternally bound (see newSavedMenuChild). Distinct names EVER seen
//     consume one fork ID each; deletions, re-saves, reorders, and
//     placeholder flips consume ZERO, so the fork's global ID counter -
//     which must never cross WM_COMMAND's 65536 low-16-bit truncation
//     point - sits at ~21 static IDs + 2 placeholder rows + the
//     distinct-name count (<=100 via the store cap in any one store).
//
// Identical ticks no-op twice over: the traySameMenuSpecs gate skips even
// planning, and the planner's own changed bit makes an order-only change
// of the same name SET a no-op (tray order is creation order, not daemon
// order). A stalled plan (the identity map would exceed
// traySettingsIdentityHardBound - dead code by construction at the
// store's 100-name cap) logs one WARN and touches NOTHING: the previous
// render stays exactly as it was. No automated test can observe the
// native ordering on Linux (no native menu, no message turn), so this
// executor is review-verified; the pure planner (trayPlanSettingsSync) is
// unit-tested in traystate_test.go: rename churn hides the old row and
// creates a new one (IDs strictly increasing, no reuse across names),
// delete+re-save re-shows the same row with zero creates, the 100-name
// list renders fully, the hard bound stalls untouched, and visible order
// is creation order regardless of daemon order.
func (m *savedSettingsMenu) sync(want []trayMenuSpec) {
	if traySameMenuSpecs(m.specs, want) {
		return // identical tick: no churn of any native row
	}
	// The identity model maintains BY CONSTRUCTION: m.rows is creation
	// order, which is ascending immutable fork-ID order (append-only), so
	// the row INDEX doubles as the planner model's ID surrogate. The
	// planner's decisions are per-name, never positional; the surrogate's
	// only job is to make the monotonicity explicit to the tests.
	known := make([]traySettingsIdentityRow, len(m.rows))
	for i, c := range m.rows {
		known[i] = traySettingsIdentityRow{ID: uint32(i), Name: c.name, Shown: c.shown}
	}
	plan := trayPlanSettingsSync(known, m.phShown, want)
	if plan.stall {
		m.logger.Warn("settings submenu reconcile declined: the distinct-name identity map would exceed its defensive hard bound (still far below WM_COMMAND's 16-bit truncation point); leaving the previous render untouched", "knownNames", len(m.rows), "bound", traySettingsIdentityHardBound)
		return
	}
	if plan.changed {
		// Disappearing names FIRST: clear the slot before any native
		// mutation so a stale click dispatched from the pre-change display
		// no-ops from this instant.
		for _, name := range plan.hide {
			c := m.byName[name]
			c.slot.Store("")
			c.item.Disable()
			c.item.Hide()
			c.shown = false
		}
		if plan.phTo != plan.phFrom {
			if ph := m.phRow(plan.phFrom); ph != nil {
				ph.item.Hide()
				ph.shown = false
			}
			if plan.phTo != trayPhNone {
				ph := m.ensurePlaceholder(plan.phTo)
				ph.item.Show()
				ph.shown = true
			}
			m.phShown = plan.phTo
		}
		for _, spec := range plan.show {
			c := m.byName[spec.raw]
			c.item.SetTitle(spec.Title)
			c.item.Enable()
			c.item.Show()
			// LAST: after the paint, and identical to the creation-time
			// binding - the slot is never repointed to another name.
			c.slot.Store(traySettingsApplyPrefix + spec.raw)
			c.shown = true
		}
		for _, spec := range plan.create {
			child := newSavedMenuChild(m.parent, spec, m.lightCmd, m.logger)
			m.rows = append(m.rows, child)
			m.byName[spec.raw] = child
		}
	}
	m.specs = append([]trayMenuSpec(nil), want...)
}
