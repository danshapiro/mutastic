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

// --- click-time/paint-time native verification (ROUND-6/ROUND-7/ROUND-12) ---
//
// The fork dispatches a menu click by command ID ALONE (WM_COMMAND's
// wParam -> menuItems[id].click): the native row's title text is
// display-only metadata. ROUND-12 (R11-F2) governs the shape: a "Saved
// settings" row's NAME binding is re-bound IN PLACE by every name-set
// change (bounded ID-positional pool - the row's immutable command ID
// stays put so the global ID counter never approaches WM_COMMAND's 16-bit
// truncation point), but syncSavedSettingsMenu clears EVERY row's atomic
// command slot FIRST, so a WM_COMMAND queued from a stale native row -
// one the user read as the OLD name - that dispatches anywhere inside the
// rebind window hits a cleared slot and no-ops. The read-back below
// remains as the belt to that slot discipline's suspenders (R6-F1's
// "keep the existing native title verification"), and additionally
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
	// The Saved settings submenu's bounded ID-positional POOL (R11-F2;
	// the pure discipline lives in traystate.go's trayPlanSettingsPool
	// header): every settings row ever created, in creation order (which
	// IS ascending immutable-ID order - IDs are monotonic in creation and
	// fresh rows are only ever appended). The first len(savedSpecs) pool
	// rows are the SHOWN ones, rendering savedSpecs verbatim; the rest
	// are retired (hidden, disabled, unbound) awaiting reuse on growth.
	// Both are empty until the first tick reconciles; two nil specs
	// compare equal, so only a real poll difference triggers a rebuild -
	// and a rebuild re-binds pooled rows in place, creating fresh native
	// rows ONLY when the wanted list exceeds the pool's historical
	// maximum size. The fork dispatches WM_COMMAND on the low 16 bits of
	// wParam, so this bound is load-bearing: the global ID counter must
	// never cross 65536, and pool-capped growth (21 static items + the
	// historical max list length, <= 101 rows) sits three orders of
	// magnitude below it.
	var savedPool []*savedMenuChild
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
		// latch keeps the steady state quiet in daemon.log) and rebuild the
		// submenu children exactly when the rendered spec list changes.
		listReply, listErr := askDaemon(traySavedSettingsListCmd, udpAddr, lightClientTimeout)
		want := traySavedSettings(trayParseSettingsList(listReply, listErr))
		if !traySameMenuSpecs(savedSpecs, want) {
			savedPool, savedSpecs = syncSavedSettingsMenu(savedSettings, savedPool, savedSpecs, want, lightCmd, logger)
		}
	}
}

// savedMenuChild is one retained "Saved settings" submenu child: the native
// item plus an ATOMIC command slot that its click closure reads at click
// time. The closure is assigned ONCE at AddSubMenuItem creation and is
// NEVER rebound (R2-F3): writing the fork's click field while the native
// message pump can invoke it is a data race (the pump may run the OLD
// binding while Go assigns the NEW one), so rebuilds never touch the
// binding - under R11-F2's bounded ID-positional pool they touch ONLY the
// slot. The slot holds the current "light settings apply <name>" command;
// "" means unbound (a placeholder, a RETIRED row, or a row mid-rebind -
// every rebuild's first step clears EVERY pool slot, so a racing click
// anywhere in the rebuild window no-ops; the re-bound row's new command
// lands only after its new title painted). retiredAt is the retirement
// timestamp (zero while shown): under the pool discipline a retired row
// (cleared slot, placeholder title, disabled, hidden) AWAITING REUSE on
// growth re-enters the menu by rebind, so its identity - and immutable
// command ID - stays put across regimes.
type savedMenuChild struct {
	item      *systray.MenuItem
	slot      atomic.Value // string: current apply command; "" = unbound
	retiredAt time.Time    // zero while shown; stamped at retirement
}

// newSavedMenuChild creates one submenu child with its STABLE click closure
// - the only place Click is ever assigned: the closure
// (traySetSettingsClick, traystate.go) reads the slot at click time,
// verifies the row's LIVE native title still names the slotted setting
// (trayVerifySettingsItemTitle), and only then dispatches.
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
// closure is NEVER rebound: under R11-F2's bounded ID-positional pool the
// slot alone carries the rebinds (cleared-first, stored-last per rebuild;
// see syncSavedSettingsMenu).
//
// The recorded standing residual (R3-F4/R4-F2/R5-F2, ruled non-blocking)
// is back to its minimal shape under the pool: a click queued in the same
// Win32 message turn as a rebuild's rebind applies the row's NEW name.
// (ROUND-7/ROUND-8 eliminated that window structurally by retiring rows
// and rendering fresh per change; R11-F2 removes THAT scheme because its
// per-change fresh IDs march the fork's global counter past WM_COMMAND's
// 16-bit truncation point and permanently break dispatch.) The slot
// discipline plus the stable closure's native-title verification
// (trayVerifySettingsItemTitle) keep every OTHER shape inert: a click in
// the cleared window no-ops on "", a click against a divergent native
// title REFUSES with one WARN (traySetSettingsClick). No automated test
// can observe any of this on Linux (no native menu, no message turn), so
// the ordering and the native read-back are review-verified; the pure
// planner and comparators are unit-tested in traystate_test.go.
func newSavedMenuChild(parent *systray.MenuItem, title, command string, lightCmd func(string) func(), logger *slog.Logger) *savedMenuChild {
	child := &savedMenuChild{}
	child.slot.Store(command) // bound BEFORE the create call: never an uninitialized slot behind a revealed item
	child.item = parent.AddSubMenuItem(title, "")
	child.item.Click(traySetSettingsClick(child.item, &child.slot, lightCmd, logger))
	return child
}

// syncSavedSettingsMenu reconciles the "Saved settings" submenu to want
// (called only when traySameMenuSpecs reported a change) under R11-F2's
// BOUNDED ID-POSITIONAL POOL discipline: pooled rows keep their identity
// (and immutable command IDs) forever; the wanted names are bound to pool
// rows in ASCENDING ROW ID order, so the fork's own ID-sorted visible
// list displays exactly the daemon order. The pure decision lives in
// trayPlanSettingsPool (traystate.go); this function is the mechanical
// executor over four phases, on the refresh loop's goroutine.
//
// ROUND-8's full-rebuild discipline (retire every row, render fresh rows
// per change) is REMOVED root-and-branch: its fresh IDs grew the fork's
// GLOBAL counter on every change, retired rows stay in the fork's map
// forever (no remove API), and Windows WM_COMMAND supplies a menu item's
// identifier ONLY in the low 16 bits of wParam - cumulative IDs past
// 65536 would alias CURRENT rows onto EARLIER map entries (inert retired
// rows at best, unrelated actions like mic mute or light toggle at
// worst), permanently breaking the tray until restart (R11-F2). Under
// the pool, global ID usage MAXES at the static items plus the pool's
// historical maximum length (<= 100 names via the store cap, or the one
// reused placeholder row) - far below 65536 - and churn (renames,
// reorders, deletes, placeholder flips) creates NOTHING.
//
// Phase A - CLEAR ALL SLOTS FIRST (the stale-click defense): every pool
// row's command slot is cleared before ANY native mutation, so from this
// instant a racing click against ANY settings row - shown or retired -
// loads "" and no-ops. The re-bound commands land only in phase B, per
// row AFTER its new title painted, so the cleared window is the only
// slot/title divergence a click can ever observe.
//
// Phase B - BIND POSITIONALLY: for each planned assignment (want[k] on
// the k-th ascending-ID row) the row is retitled to the escaped display
// spec, enabled/disabled, SHOWN (a previously-retired row re-appears at
// its ID-sorted position among the shown rows - the k-th - per verified
// fork behavior: Show re-inserts at the ID-sorted position, never the
// end), and its new command slot is stored LAST. The display/command
// split is raw-vs-Title: spec.Title is the menu-ESCAPED display string
// ("&" -> "&&"), spec.raw the VERBATIM saved name - the command must use
// raw, because the daemon's store keys names raw and an escaped title
// would apply nothing.
//
// Phase C - RETIRE THE SURPLUS: rows beyond len(want) keep their cleared
// slot, are repainted to the "(no saved settings)" placeholder (a click
// still holding a pre-clear slot then ALSO fails the native-title
// check), disabled, hidden, and stamped with the retirement time. They
// stay IN the pool - phase D of a later GROWTH rebuild reuses them; they
// are never leaked and never re-sorted.
//
// Phase D - GROW: exactly plan.fresh rows are created for the positions
// beyond the pool's size, bound to want[len(pool)..] in order, and
// appended - append-only creation keeps pool order identical to
// ascending immutable-ID order. Each fresh child arrives with its slot
// AND its stable closure already assigned (see newSavedMenuChild; R3-F4
// ordering, no yield between), then the same title/enable/Show sequence
// as a re-bind (a fresh row is already native-visible; Show is a
// SetMenuItemInfo no-op kept for sequence parity).
//
// A stalled plan (len(want) beyond traySettingsRowsHardBound - way past
// the store's 100-name cap, so only a bogus caller) logs one WARN and
// touches NOTHING: the previous render stays exactly as it was. No
// automated test can observe the native ordering on Linux (no native
// menu, no message turn), so this executor is review-verified; the pure
// planner (trayPlanSettingsPool) is unit-tested in traystate_test.go:
// identical ticks no-op, assignments land on ascending IDs positionally,
// growth only beyond the pool, shrink retires the shown surplus, churn
// creates nothing, and the hard bound stalls.
func syncSavedSettingsMenu(parent *systray.MenuItem, pool []*savedMenuChild, cached, want []trayMenuSpec, lightCmd func(string) func(), logger *slog.Logger) (newPool []*savedMenuChild, newCached []trayMenuSpec) {
	// The pool discipline maintains BY CONSTRUCTION: the first
	// len(cached) pool rows (in pool order, which is ascending-ID order -
	// creation is append-only) are the shown ones, rendering cached
	// verbatim; the rest are retired. IDs stay honest positionally (the
	// planner also sorts by them - cheap insurance, and what the
	// permutation table tests pin from deliberately un-ordered inputs).
	model := make([]traySettingsPoolRow, len(pool))
	for i := range pool {
		model[i] = traySettingsPoolRow{ID: uint32(i)}
		if i < len(cached) {
			model[i].spec = cached[i]
			model[i].Shown = true
		}
	}
	plan := trayPlanSettingsPool(model, want)
	if !plan.changed || plan.stall {
		if plan.stall {
			logger.Warn("settings submenu rebuild declined: the wanted list exceeds the bounded pool cap; leaving the previous render untouched (the fork's global menu IDs must never approach WM_COMMAND's 16-bit truncation)", "want", len(want), "bound", traySettingsRowsHardBound)
		}
		// Identical tick (the refresh loop gates on traySameMenuSpecs, so
		// this fires only from a defensive direct call): touch NOTHING -
		// no retires, no churn, no ordering frobs.
		return pool, cached
	}

	// Phase A - clear EVERY pool slot FIRST: a racing click unbinds to ""
	// and no-ops, no matter which row it fired against.
	for _, child := range pool {
		child.slot.Store("")
	}

	bind := func(child *savedMenuChild, spec trayMenuSpec) {
		// command is the row's slot content: an ENABLED real entry's apply
		// verb on the VERBATIM raw name; "" for placeholders (an unbound,
		// disabled row can never fire).
		command := ""
		if spec.Enabled {
			command = traySettingsApplyPrefix + spec.raw
		}
		child.item.SetTitle(spec.Title)
		if spec.Enabled {
			child.item.Enable()
		} else {
			child.item.Disable()
		}
		child.item.Show()
		child.slot.Store(command) // LAST: after the paint, never before it
	}

	// Phase B - bind the wanted positions onto ascending-ID rows.
	for _, assign := range plan.assigns {
		bind(pool[assign.row], assign.spec)
	}

	// Phase C - retire the shown surplus (slots already cleared in
	// phase A).
	now := time.Now()
	for _, i := range plan.retires {
		child := pool[i]
		child.item.SetTitle(trayNoSavedSettingsTitle)
		child.item.Disable()
		child.item.Hide()
		child.retiredAt = now
	}

	// Phase D - grow: fresh rows for the positions beyond the pool, bound
	// in order and appended (pool order stays ascending-ID).
	for _, spec := range want[len(pool):] {
		child := newSavedMenuChild(parent, spec.Title, "", lightCmd, logger)
		bind(child, spec)
		pool = append(pool, child)
	}

	newCached = make([]trayMenuSpec, len(want))
	copy(newCached, want)
	return pool, newCached
}
