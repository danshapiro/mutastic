package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/energye/systray"
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
// PAIR plus the VERBATIM settings name (raw): a real saved name renders
// enabled (click applies the raw name); the two placeholder rows render
// disabled and carry no raw name. The struct is compared whole by
// traySameMenuSpecs because the settings-name grammar permits a literal
// saved name equal to a placeholder string - title-only comparison would
// leave a real setting grayed (or a placeholder clickable) across a
// placeholder<->real transition.
type trayMenuSpec struct {
	// Title is the DISPLAY string: traySavedSettings escapes "&" as "&&"
	// because a Windows menu title treats a bare "&" as a mnemonic
	// underline marker and would mangle the name on screen; escaping is
	// injective, so distinct raw names never share an escaped Title.
	Title string
	// raw is the VERBATIM saved name the click command applies ("" for
	// the placeholders, which never fire): the daemon's store knows
	// nothing about menu-title escaping, and Title must not be unescaped
	// back (escaping is only safe to APPLY, not reverse-guess).
	raw string
	// Enabled is the click gate: real entries fire their apply command,
	// placeholders stay disabled and unbound.
	Enabled bool
}

// traySavedSettingsListCmd is the daemon verb the tray's refresh loop polls
// once per tick to keep the "Saved settings" submenu in sync with the
// store (the daemon's logCommand latch keeps the steady-state poll quiet in
// daemon.log).
const traySavedSettingsListCmd = "light settings list"

// The two placeholder row titles. Both are tray-side UI text, NEVER sent
// by the daemon - and neither contains "&", so they need no escaping.
// They title the two STATIC, never-name-bound placeholder rows (R12-F1,
// ROUND-13) - a real saved name equal to one of them gets its own ENABLED
// identity row instead.
const (
	traySettingsUnavailableTitle = "(settings unavailable)"
	trayNoSavedSettingsTitle     = "(no saved settings)"
)

// traySavedSettings renders one poll result as submenu specs in three
// regimes: daemon not-ok (transport down OR store broken) -> one DISABLED
// "(settings unavailable)" placeholder (one wording covers both honestly);
// ok with no names -> one DISABLED "(no saved settings)" placeholder;
// otherwise one {escaped name, ENABLED} per name in input order (the daemon
// emits them sorted). Both placeholder strings are tray-side UI text, NEVER
// sent by the daemon - and neither contains "&", so they need no escaping.
// A REAL name's Title escapes "&" as "&&" (Windows would otherwise eat a
// bare "&" as a mnemonic marker and mangle the displayed name) while raw
// keeps the VERBATIM name for the click command; the raw field pins the
// title<->name pairing by index, so the glue never re-derives it.
func traySavedSettings(names []string, daemonOK bool) []trayMenuSpec {
	if !daemonOK {
		return []trayMenuSpec{{Title: traySettingsUnavailableTitle}}
	}
	if len(names) == 0 {
		return []trayMenuSpec{{Title: trayNoSavedSettingsTitle}}
	}
	specs := make([]trayMenuSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, trayMenuSpec{Title: strings.ReplaceAll(name, "&", "&&"), raw: name, Enabled: true})
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
// comparing the whole trayMenuSpec struct element-wise - {Title, raw,
// Enabled}, never titles only (raw tracks Title by construction - escaping
// is injective - so a name change still forces the rebuild; the raw field
// matters for whole-struct honesty, not as a second diff source). The
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

// --- permanent row<->name identity map (R12-F1, ROUND-13) ---
//
// History: rows were first re-bound in place, then retired and recreated
// fresh per name-set change (ROUND-8 full-rebuild - killed by R11-F2:
// its per-change fresh IDs marched the fork's GLOBAL counter toward
// WM_COMMAND's 16-bit truncation), then pooled forever and re-bound
// POSITIONALLY (ROUND-12's bounded ID-positional pool: want[k] onto the
// k-th ascending-ID row). Delta review round 12 (R12-F1) closed the pool
// too: a WM_COMMAND queued from a row the user read as name A but
// dispatched AFTER a rebuild had re-bound that row to name B would
// execute B - and the click's native-title verification could no longer
// refuse, because the rebind had already painted B's title. The
// thrice-ruled non-blocking one-message-turn residual was only tolerated
// while the alternatives were worse; the identity map eliminates the
// wrong-apply window STRUCTURALLY: a row is NEVER re-bound to a
// different name.
//
// The discipline: the tray keeps a name->row identity map across
// rebuilds. A row, once created for a name, is bound to that name for
// the process lifetime - its command slot is stored ONCE at creation
// ("light settings apply <name>") and never changed to another name; its
// title is painted from that name at creation and re-asserted verbatim
// on every re-show; its stable Click closure (R2-F3) reads that row's
// own slot at click time. Each reconcile (change-gated by
// traySameMenuSpecs) walks the daemon's CURRENT name list and:
//
//  1. for a name no longer present, RETAINS the row but disarms it: the
//     command slot is cleared FIRST (a stale WM_COMMAND dispatched from
//     the pre-change display loads "" and no-ops), then the row is
//     disabled, then hidden. The binding is conceptually kept - the row
//     is never re-titled and never slotted to another name.
//  2. for a current name with no row yet, creates ONE fresh row: a fresh
//     immutable fork ID (IDs strictly increasing per name, never reused
//     across names), the slot stored BEFORE the create call and the
//     closure on the statement after (R3-F4), enabled and visible.
//  3. for a current name whose row already exists, re-shows THE SAME
//     row - title re-asserted (same name => same title), enabled, shown,
//     and its slot RESTORED to the identical command LAST, after the
//     paint - so delete+re-save consumes zero new IDs.
//  4. renders the two placeholders ("(no saved settings)", "(settings
//     unavailable)") as STATIC rows of their own, never name-bound:
//     permanently "" slots, always disabled, shown/hidden per regime. A
//     real saved name equal to a placeholder string gets its own ENABLED
//     identity row - the placeholder<->real transition is a show/hide
//     pair between DISTINCT rows, never a rebind.
//
// DISPLAY ORDER IS CREATION ORDER, not the daemon's sorted order: the
// fork keeps each parent's visible list sorted by the row's IMMUTABLE
// command ID (verified fork behavior: addToVisibleItems appends then
// sort.Slice ascending; Show re-inserts a hidden row at its ID-sorted
// position, never the end) and IDs are monotonic in creation, so visible
// order == first-seen order. The tray therefore lists saved settings in
// the order they were first created (after a tray start the very first
// poll seeds every current name in the daemon's sorted order; names
// saved later append at the END) - while the web UI sorts alphabetically
// and the daemon's wire list stays sorted. The divergence from daemon
// order is deliberate and documented: positional rebinding - the only
// scheme that could track daemon order with immutable IDs and no remove
// API - is exactly the wrong-apply window R12-F1 closed, and the row a
// name eternally owns is the only binding the click-time native-title
// verification can trust (it now compares the live title against the
// row's eternally-bound name, so ANY divergence - a failed native paint,
// an unreadable HMENU - refuses with one WARN).
//
// ID budget (16-bit WM_COMMAND): distinct names EVER SEEN by this tray
// process consume one ID each; deletions, re-saves, reorders, and
// placeholder flips consume ZERO (no native row is created for them).
// With ~21 static menu IDs plus the 2 placeholder rows, the store's
// 100-name cap puts an ordinary session at ~123 IDs - over 500x below
// the 65536 truncation point - and even pathological delete+recreate
// churn across a long session stays far below it. The defense-in-depth
// hard bound below stalls any reconcile that would push the identity map
// past 1024 distinct names (dead code by construction; the stall leaves
// the previous render untouched and logs one WARN per poll).
//
// The recorded residuals on this path are now refusal-shaped ONLY: a
// stale click dispatched while its row is disarmed no-ops (the cleared
// slot), and a click whose live native title diverges from the row's
// bound name refuses with one WARN. A click can NEVER apply a setting
// other than the one its row displays.
//
// traySettingsIdentityHardBound is that defense-in-depth cap on the
// identity map's size - the number of DISTINCT names ever bound to rows
// by this tray process. Independent of the store's 100-name cap, 1024
// exceeds every legal name history by an order of magnitude while
// standing ~60x below 65536 (21 static IDs + 2 placeholders + 1024 name
// rows) - the fork's global command-ID counter must never approach
// WM_COMMAND's low-16-bit truncation point.
const traySettingsIdentityHardBound = 1024

// traySettingsPlaceholder identifies the submenu's static placeholder
// regime: which (if any) of the two never-name-bound placeholder rows is
// currently on screen.
type traySettingsPlaceholder int

const (
	// trayPhNone: real name-bound rows are on screen; both placeholder
	// rows are hidden.
	trayPhNone traySettingsPlaceholder = iota
	// trayPhEmpty: the daemon answered with an empty store -
	// "(no saved settings)".
	trayPhEmpty
	// trayPhUnavailable: the poll failed or the store is disabled/corrupt
	// - "(settings unavailable)".
	trayPhUnavailable
)

// traySettingsIdentityRow is the PURE model of one name-bound "Saved
// settings" submenu row the planner consumes: the immutable fork command
// ID (monotonic in creation - visible order is ascending ID, i.e.
// creation order, BY DESIGN), the VERBATIM saved name the row is bound
// to for the process lifetime, and whether it is currently shown. A
// hidden row keeps its name - the binding is the row's identity - while
// its command slot is held cleared ("") for the whole hidden time, so a
// stale click dispatched from the pre-change display no-ops.
type traySettingsIdentityRow struct {
	ID    uint32
	Name  string
	Shown bool
}

// traySettingsSyncPlan is the complete pure reconcile decision for one
// "Saved settings" submenu sync under the permanent identity discipline
// (R12-F1). The executor half is tray_windows.go's savedSettingsMenu.sync,
// which runs the actions on the refresh loop's goroutine in class order:
// hides (slot cleared FIRST, then disable, then hide) before the
// placeholder flip before shows before creates, and a row's slot is
// stored LAST inside every show/create, after the paint.
type traySettingsSyncPlan struct {
	// changed is the identical/changed decision: false means NO-OP - the
	// executor touches nothing, not even a slot (identical ticks never
	// churn a native row).
	changed bool
	// stall is the hard-bound refusal (the identity map would exceed
	// traySettingsIdentityHardBound distinct names): the executor logs
	// one WARN and touches nothing, leaving the previous render exactly
	// as it was.
	stall bool
	// create lists the first-seen names needing a FRESH row, in the
	// wanted list's order (their first-seen order): each becomes a new
	// identity row eternally bound to its name (strictly increasing
	// fresh IDs, never reused across names).
	create []trayMenuSpec
	// show lists the wanted names whose identity rows already exist and
	// must be (re-)shown: title re-asserted, enabled, shown, and the
	// slot restored to the row's OWN eternally-bound command - never
	// another name's.
	show []trayMenuSpec
	// hide lists the raw names of the shown rows no longer present in
	// the wanted list: disarmed (slot cleared FIRST) and hidden, with
	// the binding kept. Already-hidden rows are not listed (nothing to
	// do).
	hide []string
	// phFrom/phTo is the placeholder-regime transition (equal means no
	// flip): the executor hides the phFrom placeholder row and shows the
	// phTo one, creating it lazily on first use.
	phFrom, phTo traySettingsPlaceholder
}

// trayWantPlaceholder reads the placeholder regime out of a rendered want
// list. traySavedSettings emits EXACTLY ONE placeholder entry when a
// regime applies, and placeholders are the only entries with an empty
// raw name (the name grammar forbids empty names, so raw == "" is
// unambiguous - even though the grammar permits a literal saved name
// equal to a placeholder string: that name arrives ENABLED with a
// non-empty raw field and takes the identity-row path).
func trayWantPlaceholder(want []trayMenuSpec) traySettingsPlaceholder {
	if len(want) == 1 && want[0].raw == "" {
		switch want[0].Title {
		case traySettingsUnavailableTitle:
			return trayPhUnavailable
		case trayNoSavedSettingsTitle:
			return trayPhEmpty
		}
	}
	return trayPhNone
}

// trayPlanSettingsSync decides one "Saved settings" reconcile from the
// known identity rows (the executor passes them in creation order; the
// per-name decisions never depend on that order - IDs are carried so the
// tests can pin strictly-increasing creation and no reuse across names)
// and the wanted render list in daemon order:
//
//   - a wanted name with no known row -> [create row];
//   - a wanted name with a known, hidden row -> [show existing row X];
//   - a known, shown row whose name is not wanted -> [hide row Y];
//   - a placeholder regime mismatch -> the phFrom->phTo flip;
//
// a known, shown row still wanted, and a matching regime, need nothing
// (identical tick => changed=false). The identity map NEVER rebinds: no
// action points a row at a different name, and a daemon-order change of
// the same name SET reconciles to a no-op (tray order is creation order
// by design). The reconcile stalls - touching nothing - if the creates
// would push the map past traySettingsIdentityHardBound.
func trayPlanSettingsSync(known []traySettingsIdentityRow, phShown traySettingsPlaceholder, want []trayMenuSpec) traySettingsSyncPlan {
	plan := traySettingsSyncPlan{phFrom: phShown, phTo: trayWantPlaceholder(want)}
	// wanted double-duties as the duplicate guard: the daemon emits
	// distinct map keys, but a hand-built want must never plan two rows
	// for one name (identity is one row per name, first occurrence wins).
	wanted := make(map[string]trayMenuSpec, len(want))
	for _, spec := range want {
		if spec.raw == "" {
			continue // placeholders are never name-bound
		}
		if _, dup := wanted[spec.raw]; dup {
			continue
		}
		wanted[spec.raw] = spec
		knownHas := false
		for _, row := range known {
			if row.Name == spec.raw {
				knownHas = true
				break
			}
		}
		if !knownHas {
			plan.create = append(plan.create, spec)
		}
	}
	if len(known)+len(plan.create) > traySettingsIdentityHardBound {
		return traySettingsSyncPlan{changed: true, stall: true}
	}
	for _, row := range known {
		if spec, ok := wanted[row.Name]; ok {
			if !row.Shown {
				plan.show = append(plan.show, spec)
			}
		} else if row.Shown {
			plan.hide = append(plan.hide, row.Name)
		}
	}
	plan.changed = len(plan.create) > 0 || len(plan.show) > 0 || len(plan.hide) > 0 || plan.phTo != plan.phFrom
	return plan
}

// trayArmedMatchesNative is the pure comparison behind trayVerifyArmed
// (R6-F3): the mic item's live NATIVE row must read back exactly the
// painted title AND enablement of the computed snapshot before the armed
// premise may be stored (refresh) or the click may fire (dispatch).
// nativeDisabled is the native row's disabled bit; the expected
// enablement is trayMuteEnabled(snap.Armed) - an ARMED snap requires a
// natively ENABLED row, a disarmed one a natively DISABLED row. Any
// mismatch means the visible row diverges from the premise, and the only
// safe move is to stay disarmed / refuse the click.
func trayArmedMatchesNative(snap trayMuteSnapshot, nativeTitle string, nativeDisabled bool) bool {
	return nativeTitle == snap.Title && nativeDisabled == !trayMuteEnabled(snap.Armed)
}

// traySettingsApplyPrefix is the daemon verb prefix a "Saved settings"
// row's command slot carries: the apply verb plus the VERBATIM raw saved
// name (never the escaped display title).
const traySettingsApplyPrefix = "light settings apply "

// traySettingsCmdMatchesTitle is the pure half of the click-time native
// title check (the ROUND-6 belt; ROUND-13's permanent row<->name identity
// map is the suspenders - see trayPlanSettingsSync). On Windows the fork
// dispatches a menu click by command ID alone (WM_COMMAND ->
// menuItems[id].click): the native row's title is display-only, so a
// WM_COMMAND dispatched from a STALE native row must never be able to
// execute a command the row no longer displays. Under the identity
// discipline a row's slot holds only "" (held while the row is hidden -
// a stale dispatch no-ops) or the row's ETERNALLY-bound command (while
// shown), and the title it displays is that bound name's; the check
// reads the row's live native title and executes only when it still
// names the slotted setting, catching the residual the slot discipline
// cannot: a FAILED native paint (a SetTitle that never landed) or an
// unreadable HMENU. The comparison is exact and escaping-aware: the slot
// command carries the VERBATIM raw name ("light settings apply <name>")
// while the native title carries the menu-ESCAPED display form ("&" ->
// "&&", see traySavedSettings), so the raw name is re-escaped before
// comparing. A malformed command (missing prefix or empty name) never
// matches - refusal is always the safe direction.
func traySettingsCmdMatchesTitle(cmd, nativeTitle string) bool {
	name, ok := strings.CutPrefix(cmd, traySettingsApplyPrefix)
	if !ok || name == "" {
		return false
	}
	return strings.ReplaceAll(name, "&", "&&") == nativeTitle
}

// traySetSettingsClick is the single click-closure constructor for every
// "Saved settings" submenu row (tray_windows.go's newSavedMenuChild and
// newSavedSettingsPlaceholder). The closure is STABLE for the item's
// lifetime (R2-F3: it is assigned once at creation and never rebound);
// under ROUND-13's permanent row<->name identity the row behind it is
// bound to ONE name for the process lifetime. The atomic slot is the
// only state the closure reads: it holds the row's eternally-bound
// "light settings apply <name>" while the row is shown, and "" while it
// is hidden (its name deleted, or every name disarmed behind a
// placeholder regime) - hides clear the slot BEFORE the native Hide, so
// a click queued from the pre-change display but dispatched after loads
// "" and no-ops, and a re-save of the SAME name restores the IDENTICAL
// command to the SAME row: the slot is never repointed to another name,
// so the R12-F1 wrong-apply shape of the old positional rebind is
// structurally closed. After loading the slot the closure ALSO
// re-verifies that the row's live NATIVE title still names the slotted
// setting (trayVerifySettingsItemTitle; portable default true, real
// read-back on Windows) - the remaining belt to the slot discipline's
// suspenders. An unbound slot ("" - a placeholder or a hidden row) or a
// failed check is a no-op; the failed check also logs one WARN, and the
// current poll state paints the truthful row for the next click.
func traySetSettingsClick(item *systray.MenuItem, slot *atomic.Value, lightCmd func(string) func(), logger *slog.Logger) func() {
	return func() {
		cmd, _ := slot.Load().(string)
		if cmd == "" {
			return
		}
		if !trayVerifySettingsItemTitle(item, cmd) {
			logger.Warn("settings click declined: the row's live native title no longer names the slotted setting (stale native row); refusing so a mid-rebind row can never execute a command it does not display", "cmd", cmd)
			return
		}
		lightCmd(cmd)()
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
	openPanel     func() error                         // open/focus the browser control panel (mic + lights + settings)
	stopPanel     func() error                         // POST the light panel's /api/shutdown (Task 4 endpoint)
	requestQuit   func()                               // leave the systray message loop
	signalRefresh func()                               // ask the display loop to repoll
	logger        *slog.Logger
	// muteFlight is the single-flight guard around muteClick's whole
	// conditional-verb->refresh sequence (R2-F2): clicks arrive on their
	// own goroutines, and an overlapping second click would duplicate the
	// first click's verb+sweep, so a click that finds one already in
	// flight is DROPPED with one WARN. The zero value is ready; the
	// guard releases at the end of the sequence.
	muteFlight sync.Mutex
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

// trayMuteConditionalVerb maps one ARMED snapshot premise to the daemon's
// ATOMIC conditional mic verb (R6-F2): the premise is the state the
// displayed label's verb targets FROM - armed unmuted (label "Mute")
// asks "mute-if unmuted", armed muted (label "Unmute") asks "unmute-if
// muted". Inline literals mirror the existing inline "mute"/"unmute"
// strings; the daemon-side grammar is pinned in
// proto.ParseConditionalMute.
func trayMuteConditionalVerb(armed trayState) string {
	if armed == trayStateMuted {
		return "unmute-if muted"
	}
	return "mute-if unmuted"
}

// muteClick is the dynamic Mute/Unmute item's click entry point: the click
// performs exactly the action the displayed label names. The whole sequence
// runs under the muteFlight single-flight guard: a second click arriving
// while one is still in flight is DROPPED with one WARN (an overlapping
// click would duplicate the first click's verb+sweep), and the guard
// releases so a later click works.
//
// R11-F1 capture boundary: snap arrives BY VALUE, captured SYNCHRONOUSLY in
// the native click callback (tray_windows.go; the callback shape is
// func(){ s := load(); go act(s) }). Loading the snapshot inside THIS
// goroutine instead was the R11-F1 glue race: a mid-flight refresh could
// repaint and store the OPPOSITE {title, armed} pair between the click and
// the goroutine's load, and the late load would then serve the NEW premise
// to the OLD click - firing the new verb (tracked state allowing) though
// the user acted on the old label. With the value captured at the native
// dispatch moment, the premise the click acts on IS the pair its display
// was read from. muteClick therefore takes the snapshot as a parameter and
// never re-loads it - and tests inject a stale snapshot DIRECTLY (a snap
// whose premise the daemon no longer holds), pinning the refusal semantics
// through the capture boundary.
//
// The click uses that one {title, armed} snapshot (one read of
// what the user saw: the menu's title and enabled bit are only the last
// verified poll's rendering), then - when the premise is armed - checks
// that the live NATIVE row still displays the snapshot's title with the
// matching enablement (verify: trayVerifyArmed arm; portable stub true -
// R6-F3; a native paint failure or a mid-paint click can never execute
// the opposite of what is on screen). It then makes ONE daemon call: the
// ATOMIC conditional verb the label names (trayMuteConditionalVerb); the
// daemon checks the premise against its tracked state and, in the SAME
// serveUDP step, either runs the absolute verb plus the F24 meeting-app
// sweep or refuses - the R6-F2 fix for the probe->verb hardware-event
// double-sweep window. Reply regimes (R3-F2 FINAL RULE carried over,
// superseding r2's sweep-on-flip):
//
//   - ok: the premise matched; the daemon already fired the absolute mic
//     verb AND the sweep. One INFO and a refresh. Spy order:
//     ask:<conditional>,refresh. (The tray no longer injects anything
//     itself - the daemon-side inject covers it, so the old tray-side
//     inject request is REMOVED from this path.)
//   - flipped <state> (the mic already sits in the label's TARGET state
//     or is unknown - it flipped between the last poll and the click):
//     NO mic verb AND NO sweep ran daemon-side - one WARN and a refresh
//     only; this refusal IS the precision-amendment semantics. The
//     flip's cause remains unknowable per click: (a) a SWEEPING path
//     made the flip (physical button, tray item, Stream Deck key - the
//     frequent real-world case, e.g. a post-meeting physical press
//     inside the 2 s poll window), in which case the apps were already
//     carried along and sweeping again would UNDO them (apps toggle back
//     while the mic stays put); (b) a mic-only path made the flip (panel
//     card, CLI), in which case the apps were never moved. Choosing
//     no-sweep keeps case (a) correct; case (b)'s app desync is the
//     deliberate, documented limitation whose recovery is the manual
//     resync procedure (toggle the apps once by hand, then every
//     sweeping path keeps them in sync). Spy order:
//     ask:<conditional>,refresh.
//   - An error:-prefixed reply (a dead daemon or a failed verb/inject):
//     one ERROR and a refresh; the 2 s poll restores the truthful
//     display. Spy order: ask:<conditional>,refresh.
//   - An unarmed premise (unknown/down - the item displays the neutral
//     gray label) or a failed armed-verify (the native row diverges from
//     the snapshot): NOTHING is asked - one WARN and a refresh. Spy
//     order: refresh.
//
// Declining every half-action on a flip is the correct convergence, not a
// breach of "click performs the displayed action": the label's TARGET
// state is already true (a "Mute" label targets muted; "flipped muted"
// means the mic's job is done) and the apps belong to whichever path made
// the flip. The native menu title can go cosmetically stale between
// polls, but that staleness can never cause a wrong mic action BECAUSE
// the daemon-side conditional verb gates every firing - this is a
// documented rendering limitation, not a behavioral residual.
//
// Contract alignment (ROUND-7): docs/plans/2026-08-15-saved-settings.md
// carries the dated "Precision amendment (review convergence, ROUND-6)"
// pinning the mute-label constraint to the never-wrong rule (perform the
// displayed action while the premise matches; refuse on any flip), and
// "Precision amendment (review convergence, ROUND-7)" carrying that rule
// onto the atomic conditional verbs: the premise check and the action
// now live in ONE daemon step, so the window the R6-F2 finding re-opened
// (probe datagram -> hardware event -> verb datagram) is closed. The
// amendment also records the F24 physics rationale: the app sweep is a
// blind toggle (meeting-app mute state is unobservable), so when the
// premise flipped every ACTING alternative has a catastrophic case and
// this gated refusal is the unique never-wrong action. README's tray
// section states the same contract for end users.
func (a *trayActions) muteClick(snap trayMuteSnapshot, verify func(trayMuteSnapshot) bool) {
	if !a.muteFlight.TryLock() {
		a.logger.Warn("mute click dropped: a previous mute click is still in flight")
		return
	}
	defer a.muteFlight.Unlock()
	if !trayMuteEnabled(snap.Armed) {
		a.logger.Warn("mute click declined: the menu premise is not armed (unknown mic state); no truthful direction exists", "armed", trayStateName(snap.Armed))
		a.signalRefresh()
		return
	}
	if !verify(snap) {
		// R6-F3: the live native row does not show the snapshot's title
		// with the matching enablement (a failed native paint, or a click
		// that landed mid-paint): never execute against a diverging
		// display - the next poll re-paints and re-arms.
		a.logger.Warn("mute click declined: the live native row does not match the armed premise (title or enablement diverged); refusing so the click can never perform the opposite of the displayed label", "title", snap.Title, "armed", trayStateName(snap.Armed))
		a.signalRefresh()
		return
	}
	reply, err := a.ask(trayMuteConditionalVerb(snap.Armed))
	switch {
	case err != nil || strings.HasPrefix(reply, "error:"):
		a.logger.Error("mute click failed", "daemon_reply", reply, "ask_err", errString(err))
		a.signalRefresh()
	case reply == "ok":
		a.logger.Info("mic set (conditional verb matched premise; daemon ran the verb + F24 app sweep in one step)", "armed", trayStateName(snap.Armed))
		a.signalRefresh()
	case strings.HasPrefix(reply, "flipped "):
		// Flipped premise (R3-F2): the mic is already at the label's
		// target (or unknown) AND the apps were carried by the sweeping
		// path that made the flip (physical/tray/deck) - the daemon ran
		// no verb and no sweep, and neither may we.
		a.logger.Warn("mute click declined: the daemon's state no longer matches the label's premise (flipped); no verb and no sweep ran", "armed", trayStateName(snap.Armed), "daemon_reply", reply)
		a.signalRefresh()
	default:
		a.logger.Warn("mute click declined: unrecognized daemon reply (not firing an unconfirmed action)", "daemon_reply", reply)
		a.signalRefresh()
	}
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
