# Glassline Web UI Redesign — Design Spec

- **Date:** 2026-08-17 · **Revision:** 2 (supersedes rev 1, same commit history)
- **Status:** rev 1 approved 2026-08-17; rev 2 per user directive — *"Ignore the tests and redo it; we want it to be outstanding, not consistent with the previous approach. We will update the tests as needed. Use subagents; make both designs as good."*
- **Scope:** complete visual redesign of the embedded web panel (`internal/lightui/index.html`) in the Glassline design language, command-deck layout, dark + light adaptive themes, at outstanding design quality.
- **Non-goal:** any behavioral, endpoint, polling, or protocol change. Tray, Stream Deck, daemon, and `mutation_queue.js` are untouched.

## 1. Locked product decisions (brainstorm, unchanged by rev 2)

1. **Design language — Glassline:** frosted translucent panels over deep color fields, pill controls, airy typography, macOS/iOS register (chosen from four mockups).
2. **Hierarchy — command deck:** left rail of verbs (mic hero, gang controls, saved settings) stays in view at ≥1100 px; wide main column holds the panels grid; single column below, same order (chosen from three wireframes).
3. **Themes — dark + light, system-adaptive** via `prefers-color-scheme`, equal quality in both. No in-page toggle.
4. **Color semantics — unchanged:** muted = rose/attention, unmuted-live = calm mint, off/unknown/disconnected = dim. Matches the tray icon's meaning.
5. **Quality bar — outstanding, not consistent-with-today (rev 2):** design leads; markup, CSS, and inline JS may be freely restructured. The existing test suite's page-content assertions no longer constrain the design; the suite is updated to match afterwards (owner's step, §6).

## 2. How the pair was built (rev 2 process)

Dark and light theme designs were produced **in parallel by two subagents** under one shared fixed spec, so the themes cohere without one being a lazy inversion of the other:

- **Shared fixed spec (identical in both themes):** motion band 120–160 ms ease-out, press 1 px, hover lift ≤2 px, breathing 2.6 s muted-only, full `prefers-reduced-motion` kill; type scale 11/12/13/14/16/20/27 with uppercase 800-weight state words, tabular numerals, eyebrow labels; radius scale 6/10/14/20 + 999 pill; focus = 2 px signal ring + 3 px offset + 5 px 20%-alpha halo; state semantics per §1.4.
- **Dark theme** (`design-dark.md`): deepens the chosen mockup to production fidelity.
- **Light theme** (`design-light.md`): designed natively for daylight — glass white-on-bright with indigo-tinted shadows, "lit" expressed by *deepening* chromatic shadow+rim rather than bright halos (invisible on bright fields), ink primary pill answering dark's white pill.

**Authoritative component-level design lives in the two theme files committed beside this spec:**

- `docs/superpowers/specs/design-dark.md` — tokens, backdrop field, three glass elevations, per-component treatments for every state, photometer warmth track, `--glow-bri`/hue aura formula, signature details, guardrails, and its computed-contrast table + state-matrix checklist.
- `docs/superpowers/specs/design-light.md` — same structure for light, with daylight-native glass/lit-state engineering and per-stack computed contrasts.

Everything in §3–§5 of this spec defers to those two files for values; this spec is the behavior/layout/acceptance contract.

### Signature details that define "outstanding" here (both themes)

- **Physical tally lamp** for mic state: specular fleck, rim, weighted base; calm-steady when live, breathing sonar ring only when muted; unknown renders as an unlit glass bead.
- **Photometer warmth control:** the 2900K→7000K track is a true color-temperature ramp with nineteen 1 px hardware detents (one per PL81 step), so the slider *is* the physics it controls.
- **Per-light spill:** lit cards glow via `--glow-bri` (JS-set, 0–100) with hue steered by Kelvin (`color-mix(in oklab, …)` in dark so mid-temps never go green; deepening chromatic shadow in light). Off/error cards stay dark.
- **Blur discipline:** only top-tier glass blurs; inset surfaces are tint-only with hairlines — no compound blur smearing.
- **Brightness reads as luminance,** warmth reads as temperature, state reads as light: the whole page is built out of light metaphors.

## 3. Layout — command deck (final)

Desktop ≥1100 px: `.shell` is a grid — left rail `minmax(300px, 340px)`, main `1fr`; topbar spans both columns. Rail order: **mic hero card → gang/all-lights card → saved-settings card**, sticky within scroll. Main column: panels section header, `#lights-grid` (auto-fit), empty state; footer spans full width.

900–1100 px and below: single column in the same order, existing mobile density adaptations re-skinned to Glassline.

**Markup freedom (rev 2):** the implementation restructures the DOM as needed to make DOM order == visual order (mic first in rail ⇒ tab order follows the hero emphasis), add the lamp element, re-shape card internals, rename classes, and reorganize the inline script — none of it is pinned anymore. The visible copy (headings, hints, state lines) stays as-is unless the design deliberately improves it; copy changes belong to the design review, not silent drift.

## 4. Behavioral invariants (function-level; these survive redesign)

The tests' byte-pins are gone, but the product behaviors they enthroned remain binding on the new implementation:

1. **Endpoints and payloads:** `/api/lights`, `/api/light`, `/api/group`, `/api/mic`, `/api/settings` — request/response shapes exactly as the daemon serves them. `mutation_queue.js` is not modified.
2. **Update loop:** lights/mic/settings refresh together on the shared 750 ms interval; every poller keeps its in-flight guard.
3. **Mutation discipline:** 100 ms slider debounce; pending slider mutations flush **before** any settings save/apply/delete enqueues; no direct `fetch` POSTs around the queue; mutation responses re-render cards/mic/settings from their own payloads; failed mutations surface in the rose banner and re-poll.
4. **Identity binding:** every light-card control targets its canonical COM port, never the display name, never an array index.
5. **Settings safety:** the 42-byte UTF-8 gate rejects over-cap names client-side with the daemon's own message, at both entry points (Save, row Delete), before any network call; Save of an empty/whitespace name issues no mutation; settings POST-response `names` render the list without a follow-up GET; partial-apply `detail` lines containing real failure shapes render into the banner, never hidden.
6. **Mic verbs:** Mute/Unmute are absolute and armed at any reachable state; Toggle starts disarmed in markup and arms only at definitive muted/unmuted; unreachable disarms everything; state lines keep their four-shape text behavior (muted/unmuted/unknown/unreachable).
7. **State semantics (§1.4) and ARIA:** error banner keeps `role="alert"`; live regions, labels, `aria-pressed`, and disabled-state logic keep their behavior under the new structure.
8. **Group outputs:** match-slider outputs still show `Unknown` and `Mixed` per the same rules; both group trim rows keep their current deltas.

## 5. Accessibility & performance (both themes)

- DOM order equals visual order; focus sequence follows the hero hierarchy.
- Focus ring per the fixed spec — never removed.
- Contrast: acceptance = the computed tables in `design-dark.md §6.1` and `design-light.md §6.1` (body ≥4.5:1, large state words ≥3:1) verified as visually built.
- `backdrop-filter` bounded to top-tier surfaces only; `@supports not (backdrop-filter: blur(1px))` fallbacks ship (opaque-enough alpha boosts per theme files); no animated blur.
- Color field is one static pseudo-element; card re-renders remain signature-gated (§4.2 discipline applies to re-render churn as well).

## 6. Test strategy (rev 2 — suite follows design)

1. **Build gate:** `go build ./...` must pass.
2. **Known-failing, by design:** any `ui_test.go` assertion that string-matches the old page fails until the owner (or a follow-up task) refreshes the suite. Expected failure classes: exact-fragment searches, exact call-site counts, the mic-button fixture markup, the DOM-stub behavioral harnesses (they can be re-pointed at the new structure — the harness pattern survives, selectors change), the inline-script compile gate (mechanically satisfied by keeping exactly one attribute-less `<script>` block).
3. **Delivery includes a refresh map** (in the implementation plan): a table mapping each old assertion category to its new-page equivalent so the suite update is mechanical — e.g., *"toggle starts disabled"* now points at the new lamp/button markup; *"`settingsNameOverByteCap` gates ×2"* maps to the two gate call sites in the reorganized script; selector-level invariants (`.status-badge[data-state="unreachable"]` rule presence) map to their new selector names. The refresh map is generated from the final page, not hand-guessed.
4. **Acceptance = visual/behavioral runbook:** the full state matrix from both theme files' §6.2 — every element × every state (mic unmuted/muted/unknown/unreachable; connection online/degraded; light on/off/error/disconnected; group outputs Unknown/Mixed; settings empty/populated/store-error; banner paths incl. partial apply) — at **1440 px and 650 px**, in **both themes**, verified via browser screenshots. Plus interactive checks: slider debounce/ordering (drag a slider then immediately Save, confirm the applied look matches the drag), queue ordering, focus visibility, `prefers-reduced-motion`.

## 7. Risks (rev 2)

| Risk | Mitigation |
|---|---|
| Behavior drift now that byte-pins are gone | §4 invariant list is the checklist; diff review walks every removed line against it; the runbook covers each invariant's user-visible surface. |
| Suite red on merge until refresh | Communicated as expected; refresh map makes the follow-up mechanical; `go build` + runbook are the interim gate. |
| Backdrop-field banding on cheap displays | Static gradients with wide fades are banding-prone; if visible, add a 1–2% opacity noise overlay — tokens already isolate the field in one pseudo-element. |
| Two themes diverging silently later | The shared fixed spec tokens (motion/type/radii/focus) are the single source both themes read; any later tweak touches that table in both files or neither. |

## 8. Out of scope

Tray icons/menus, Stream Deck actions, daemon endpoints/UDP protocol, `mutation_queue.js`, build/deploy scripts, in-page theme toggle, renaming lights from the panel, drag-reordering, and the test-suite refresh itself (owner's follow-up per §6).
