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
// by the daemon - and neither contains "&", so they need no escaping. The
// retired-row repaint (R6-F1) reuses trayNoSavedSettingsTitle so a row
// whose name was just retired no longer displays the old name anywhere.
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

// --- full-rebuild settings-menu discipline (R7-F2, ROUND-8) ---
//
// The ROUND-7 generation-retirement pool rested on a FALSE fork
// assumption: it treated insertion/Show order as the display order. In
// the energye/systray v1.0.3 Windows backend (verified in fork source)
// each parent's VISIBLE list is kept sorted by the row's IMMUTABLE
// command ID: addToVisibleItems appends then sort.Slice ascending, and
// Show re-inserts a hidden row at its ID-sorted position - never at the
// end. IDs are monotonic in creation order (one atomic counter), so the
// displayed order IS creation order, and a row REUSED for a new name
// keeps its old ID: it would sort back into its stale position. No
// keep/rebind scheme can make the displayed order track the daemon's
// list order, so that scheme is replaced: on ANY difference the
// traySameMenuSpecs gate reports, RETIRE EVERY current row with the
// existing click-safety hygiene (command slot cleared FIRST so a racing
// click no-ops on "", title repainted to the placeholder so a click
// still holding the pre-retire slot ALSO fails the native-title check
// in traySetSettingsClick, disabled, hidden, stamped with the retirement
// time) and render the new set with FRESH native rows created in daemon
// order - ascending IDs then equal the daemon's order. Retired rows are
// NEVER re-bound; retirement remains purely as click-safety, and the
// ROUND-6 click-time native-title verification stays as the belt to
// retirement's suspenders. The R4-F2/R5-F2/R6-F1 hazard (a stale
// WM_COMMAND dispatching against a re-bound row) stays eliminated: the
// row the user saw keeps its identity forever.
//
// Native-row growth is bounded by USER-TRIGGERED name-set changes only:
// rebuilds are gated by traySameMenuSpecs to genuine changes (the 2 s
// steady-state poll can never churn), each rebuild creates at most the
// 100-name store cap in fresh rows, and renames are rare manual events
// (delete + save) - the hidden/disabled/unbound residue of one old row
// set per change tracks real user churn, a deliberate trade for exact
// daemon-order display.
//
// Steady-state ticks reuse NOTHING: traySameMenuSpecs gates the whole
// rebuild, so planning runs only on a genuine name-set change.

// trayRetiredCap bounds the tracked retired pool. Retirement past the cap
// drops the row from tracking entirely (it stays permanently hidden,
// unbound, and disabled - a leaked native row that is never displayed or
// bound again): name-set churn is gated to genuine changes, and the
// 100-name store cap bounds each rebuild's retire count, so the cap
// bounds the Go-side bookkeeping while leaked orphans track real rename
// churn over the process's lifetime.
const trayRetiredCap = 32

// traySettingsSyncPlan is the complete pure reconcile decision for one
// submenu rebuild under the full-rebuild discipline (R7-F2).
type traySettingsSyncPlan struct {
	// changed is the identical/changed decision (whole-spec identity via
	// traySameMenuSpecs): false means NO-OP - the executor touches
	// nothing, not even ordering.
	changed bool
	// retires lists EVERY cached index, in order, when changed: all
	// currently-shown rows leave the menu. Empty when unchanged.
	retires []int
	// fresh is the wanted render list, VERBATIM, in daemon order: every
	// row is created fresh in exactly this order. Immutable IDs ascend
	// with creation and the fork displays ascending IDs, so display
	// order == creation order == daemon order. Aliases want; read-only.
	fresh []trayMenuSpec
}

// trayPlanSettingsSync decides one "Saved settings" submenu rebuild from
// the cached (currently-rendered) specs and the wanted specs: identical
// sets no-op; ANY difference - a rename, an addition/removal, a reorder,
// or a placeholder<->real transition with an identical title (the name
// grammar permits the placeholder strings as literal saved names, and
// the whole-spec compare catches it) - retires ALL cached rows and
// renders every wanted row fresh. There is deliberately NO pool input:
// retired rows are never reused (their old immutable IDs could not sort
// into the new daemon order).
func trayPlanSettingsSync(cached, want []trayMenuSpec) traySettingsSyncPlan {
	if traySameMenuSpecs(cached, want) {
		return traySettingsSyncPlan{}
	}
	plan := traySettingsSyncPlan{changed: true, retires: make([]int, len(cached)), fresh: want}
	for i := range cached {
		plan.retires[i] = i
	}
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
// title check (the ROUND-6 belt; R7-F2's full-retire/fresh-render
// discipline is the suspenders - see trayPlanSettingsSync). On Windows the
// fork dispatches a menu click by command ID alone (WM_COMMAND ->
// menuItems[id].click): the native row's title is display-only, so a
// WM_COMMAND dispatched from a STALE native row must never be able to
// execute a command the row no longer displays - and rows are never
// re-bound at all now, so the row's slot only ever holds its creation-time
// name or "" (retired). The row's live native
// title is the text the menu shows for that command ID right now, and
// the click executes only when it still names the slot's setting. The
// comparison is exact and escaping-aware: the slot command carries the
// VERBATIM raw name ("light settings apply <name>") while the native
// title carries the menu-ESCAPED display form ("&" -> "&&", see
// traySavedSettings), so the raw name is re-escaped before comparing. A
// malformed command (missing prefix or empty name) never matches -
// refusal is always the safe direction.
func traySettingsCmdMatchesTitle(cmd, nativeTitle string) bool {
	name, ok := strings.CutPrefix(cmd, traySettingsApplyPrefix)
	if !ok || name == "" {
		return false
	}
	return strings.ReplaceAll(name, "&", "&&") == nativeTitle
}

// traySetSettingsClick is the single click-closure constructor for every
// "Saved settings" submenu row (tray_windows.go's newSavedMenuChild).
// The closure is STABLE for the item's lifetime (R2-F3: it is assigned
// once at creation and never rebound); rebuilds never re-bind a live row
// to a different name in place at all (R6-F1/R7-F2: on any name-set
// change the row is RETIRED - slot cleared, placeholder title, disabled,
// hidden - and never re-bound: the full-rebuild discipline creates FRESH
// rows, so a row's slot is stored exactly once, at creation). After
// loading the slot it ALSO re-verifies that the row's live NATIVE title
// still names the slotted setting (trayVerifySettingsItemTitle; portable
// default true, real read-back on Windows) - the remaining belt to
// retirement's suspenders. An unbound slot ("" - a placeholder or a
// retired row) or a failed check is a no-op; the failed check also logs
// one WARN, and the just-finished rebuild has already painted the
// truthful row for the next click.
func traySetSettingsClick(item *systray.MenuItem, slot *atomic.Value, lightCmd func(string) func(), logger *slog.Logger) func() {
	return func() {
		cmd, _ := slot.Load().(string)
		if cmd == "" {
			return
		}
		if !trayVerifySettingsItemTitle(item, cmd) {
			logger.Warn("settings click declined: the row's live native title no longer names the slotted setting (stale native row); refusing so a re-bound row can never execute its new command", "cmd", cmd)
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
// The click loads the {title, armed} snapshot EXACTLY ONCE (one read of
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
func (a *trayActions) muteClick(load func() trayMuteSnapshot, verify func(trayMuteSnapshot) bool) {
	if !a.muteFlight.TryLock() {
		a.logger.Warn("mute click dropped: a previous mute click is still in flight")
		return
	}
	defer a.muteFlight.Unlock()
	snap := load()
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
