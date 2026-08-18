# Glassline Web UI Redesign — Design Spec

- **Date:** 2026-08-17
- **Status:** Approved by user (2026-08-17)
- **Scope:** Complete visual redesign of the embedded web panel (`internal/lightui/index.html`) in the "Glassline" design language, in a command-deck layout, shipping dark and light adaptive themes.
- **Non-goal:** any behavioral, endpoint, polling, queue, or protocol change.

## 1. Locked product decisions (from brainstorm)

1. **Design language — Glassline.** Frosted translucent glass panels over a deep color-field backdrop, pill-shaped controls, airy white-on-glass typography, soft elevation and glow for active state elements. (Chosen from four presented directions.)
2. **Layout — command deck.** A left rail of *verbs* stays in view: mic hero card, all-lights gang controls, saved settings. The wide main area holds the individual panels grid and owns scrolling at large widths. Below ~1100 px it collapses to a single column in the same order. (Chosen from three hierarchy options.)
3. **Themes — dark + light, system-adaptive.** Two complete Glassline themes switched by `prefers-color-scheme`. No manual toggle in the page.
4. **Color semantics — unchanged.** Muted = red (needs attention), unmuted/live = calm (green/cool family), offline/unknown = dimmed. Matches the tray icon meaning today.
5. **Additive visual surface allowed:** a glowing "lamp" treatment for mic state, and per-light card glow proportional to brightness/steered by warmth. No new controls, no new API calls.
6. **Execution note (user request):** independent work (theme-token development, pinned-contract verification, visual review passes) is fanned out to parallel subagents; `index.html` itself is a serially-edited single artifact.

## 2. The contract that must survive (test-pinned surface)

`ui_test.go` pins the embedded page hard. The redesign is a restyle *inside* this contract. All rules below have been verified against `ui_test.go` at commit `0654bdd`.

### 2.1 JavaScript fragments pinned verbatim by string search

`TestEmbeddedLightUICardsUseTheirOwnIdentityAsTarget`, `TestEmbeddedLightUIHasSavedSettingsSection`, `TestEmbeddedLightUIMicCardUsesTheMicEndpoints` require (among others) the exact presence of:

- `const on = lightIsOn(light);`, `const brightnessDisplay = on ?`, `const tempDisplay = on ?`, `data-port="${port}"`, `const target = port;`, `{target, action: "toggle"}`, `{target, action: field, value: field === "brightness" ? value : TEMP_STEPS[value]}`, `lights.map(cardMarkup)`, `brightness.disabled = !on;`, `temp.disabled = !on;`, `"Mixed"` outputs for both group sliders, `refreshLights(true);` after a successful apply.
- Saved settings: `>Saved settings</h2>`, `id="settings-form"`, `id="settings-name"`, `id="settings-list"`, `id="settings-empty"`, `function renderSettings(names) {`, `function refreshSettings() {`, `function bindSettingsControls() {`, `const escaped = escapeHTML(name);`, `data-apply="${escaped}"`, `data-delete="${escaped}"`, `>Delete</button>`, `renderSettings(data.names || []);`, `renderSettings(result.names);`, `if (name.trim() === "") return;`, `if (action === "apply") {`, `function showApplyDetail(detail) {`, the three exact `enqueueMutation(...)` strings for save/apply/delete, `refreshLights(true);\n            showApplyDetail(result.detail);`, `function flushPendingSliders() {`, `function settingsNameOverByteCap(name) {`, `new TextEncoder().encode(name).length > 42`, `const SETTINGS_NAME_TOO_LONG = "error: settings name too long (max 42 bytes)";`.
- Exact call-site counts: `settingsNameOverByteCap(name)` guard appears **exactly 2** times (save, delete); `showError(SETTINGS_NAME_TOO_LONG);` exactly 2; `flushPendingSliders();` exactly 3.
- Mic: `id="mic-status"`, `id="mic-line"`, `data-mic-action="mute"`, `data-mic-action="unmute"`, `data-mic-action="toggle"`, the literal startup button `<button class="button-quiet" type="button" data-mic-action="toggle" disabled>Toggle</button>`, the CSS selector `.status-badge[data-state="unreachable"]`, ``enqueueMutation(`mic:${action}`, "/api/mic", {action}, false)``, `function refreshMic()`, `function updateMic(`, `function bindMicControls()`.
- Shared poll: `window.setInterval(() => { refreshLights(true); refreshMic(); refreshSettings(); }, 750);`.
- Forbidden in source: `onSettled: () => refreshLights(true)`, `const target = escapeHTML(light.name || light.port);`, `data-target`, `lights[index]`, `data-index`, mute/unmute buttons rendered initially `disabled`, direct `fetch("/api/settings", {method: "POST"...})` or `fetch("/api/mic", {method: "POST"...)`.
- DOM section order assertion: `id="all-lights-title"` index < `id="settings-title"` index < `Individual controls` index. (Mic section position is unpinned and may legally move above the gang section.)
- Exactly one attribute-less `<script>` tag (the inline IIFE), which must compile under Node (`TestEmbeddedLightUIInlineScriptCompilesNode`).

### 2.2 Behavioral contract under the Node DOM stub

`runPageScriptWithDOMStub` executes the inline script against a minimal stub. The stub supports only: `getElementById`, `querySelector`/`querySelectorAll` (registry-backed for `[data-mic-action]`, `[data-apply]`, `[data-delete]`; fresh element otherwise), listener registration/dispatch (`click`, `submit`, `dispatch`), `setAttribute`/`getAttribute`, `dataset`, `hidden`, `disabled`, `value`, `textContent`, `innerHTML`, `document.activeElement === null`, `window.setInterval`, `fetch`.

Stub elements have **no** `style`, `classList`, `createElement`, `closest`, or CSS awareness. Therefore:
- Any additive JS (e.g., writing CSS custom properties for per-light glow) must feature-detect (`if (el.style && el.style.setProperty)`) or run inside browser-only paths, or the DOM-stub tests throw and fail.
- Preferred for mic-lamp state mirroring: pure CSS keyed off the existing `#mic-status` badge's `data-state` attribute (sibling combinator on an additive static lamp element placed after the badge in DOM order) — zero JS change needed for the mic hero.

### 2.3 Not pinned by any test (safe to change)

- `<meta name="color-scheme" content="dark">` — will become `light dark`.
- All CSS (except the pinned selector `.status-badge[data-state="unreachable"]` — keep a rule carrying that selector; it may carry new declarations).
- Static markup structure beyond the pinned fragments/IDs/order assertion, provided additive changes keep every pinned substring byte-exact.

## 3. Design language tokens

All tokens are CSS custom properties on `:root`, redefined per theme inside `@media (prefers-color-scheme: light)`. Values below are starting points; implementation may fine-tune ±1 step for contrast.

### 3.1 Dark theme (default)

- **Backdrop field** (`body` background): base `#0c0c18`; three large, soft radial color fields — violet at top-left (`rgba(120,80,220,.5)`), cyan top-right (`rgba(20,150,190,.45)`), magenta bottom-center (`rgba(190,60,150,.4)`) — each faded to transparent over ~55–65% of its radius. Static (no animation); the fields also live behind the rail so glass blurs against them.
- **Glass panel surface:** `background: rgba(255,255,255,.08)`; `border: 1px solid rgba(255,255,255,.15)`; `border-radius: 20px` (cards 14–16px); `backdrop-filter: blur(16px) saturate(1.3)`; shadow `0 20px 44px rgba(0,0,0,.38)`, inner hairline `inset 0 1px 0 rgba(255,255,255,.14)`.
- **Text:** primary `#f4f6ff`, secondary `rgba(244,246,255,.62)`, dim `rgba(244,246,255,.42)`.
- **Accents:** signal cyan `#7de3ff` (focus rings, active tracks, cool end), warm amber `#ffc97d` (warm slider end, brand chip gradient), mint `#58f0c8` (online/on states).
- **State hues:** live/unmuted = mint family; muted/mic-error = rose `#ff8f9e`; offline/unknown = dim.
- **Typography:** system UI stack (unchanged); state words and panel names keep weight 700–800; tabular numerals for all values; uppercase tracking-eyebrow labels at `.69–.75rem`.
- **Buttons:** pill radius `999px`; primary = near-opaque `rgba(255,255,255,.93)` with dark ink text `#1a1c2e` and soft drop shadow; secondary = glass with visible border; quiet = borderless dim text. Min hit height stays 44px.

### 3.2 Light theme

- **Backdrop field:** base `#eef1fb`; same field positions at 40–50% opacity with pastel hues (violet `rgba(150,120,235,.35)`, cyan `rgba(90,180,220,.35)`, magenta `rgba(235,140,200,.3)`).
- **Glass panel surface:** `background: rgba(255,255,255,.62)`; `border: 1px solid rgba(255,255,255,.9)` plus outer hairline via shadow tint; blur 16px; shadow `0 16px 36px rgba(60,70,120,.14)`.
- **Text:** primary `#171b2e`, secondary `rgba(23,27,46,.6)`, dim `rgba(23,27,46,.42)`.
- **Accents re-darkened for contrast:** cyan `#0e7490`, amber `#b5780f`, mint `#16845f`; rose `#c2434f`.
- **Primary button:** dark glass — `rgba(23,27,46,.88)` with white text (a white-on-light-primary has no contrast; the "solid pill" inverts).
- Contrast target: ≥ 4.5:1 for body text, ≥ 3:1 for large state words, in both themes.

### 3.3 Motion

- Transitions on background/border/shadow/color ≤ 160 ms ease; press translation 1px; hover lift ≤ 2px.
- Lamp glow breathes only when mic is *muted* (attention should pulse; calm states don't). All animation fully disabled under `prefers-reduced-motion: reduce` (keep and extend the existing guard block).

## 4. Layout — command deck

Desktop ≥1100 px: `.shell` becomes a grid — left rail column (`minmax(300px, 340px)`) and main column (`1fr`); the topbar spans both columns. Rail contains, in order: **mic hero card**, **gang/all-lights card**, **saved-settings card**. Main column contains: panels section header, `#lights-grid` (2+ column auto-fit), the empty state. The rail has `position: sticky` within the scroll so verbs stay in view while panels scroll; the footer spans the full width at the bottom.

900–1100 px: single column; order = topbar, mic, gang, settings, panels (identical to DOM order after the mic move).

<900 px: existing mobile adaptations kept and re-skinned (padding/radius scale down, rail unstick).

**Markup strategy:** the mic `<section>` physically moves above the gang section in source so DOM order == visual order (keyboard tab order follows focus order: Mute/Unmute/Toggle reachable before gang controls; matches the hero emphasis). The section-order assertion in the tests (`gang < settings < individual`) is unaffected by this move. No other structural reordering; additive decorative elements only.

## 5. Component specifications

1. **Backdrop field:** implemented on `body::before` (fixed, `inset:0`, `pointer-events:none`, `z-index:-1` stacking) so it never intercepts clicks; static gradients only.
2. **Topbar:** brand chip = pill with the sun glyph on a frosted tile; connection pill is glass with a glowing dot (mint=cool glow online, amber degraded, dim offline).
3. **Mic hero card (rail):** large state lamp — a static, additive element (`aria-hidden`) placed as a following sibling of `#mic-status` inside the card head region; its appearance is driven entirely by CSS keyed on `#mic-status[data-state=...]` (unmuted → mint glow, calm; muted → rose glow + breathing pulse; unknown/unreachable → dim/inert). Big state word + `#mic-line` beneath; Mute = primary solid pill, Unmute/Toggle glass pills; Toggle's pinned initial-disabled markup is untouched.
4. **Gang card:** same control set (power row, two match sliders with warm→cool gradient tracks, two trim rows); wide control blocks become a vertical stack sized to the rail; slider tracks gain cyan→amber gradient and a lit fill up to thumb where feasible with pure CSS.
5. **Saved-settings card:** save form pill input + primary pill button; rows = thin frosted chips with the name, Apply (solid small pill), Delete (quiet small pill); empty state keeps the dashed treatment re-toned to glass.
6. **Light cards:** frosted cards in the main grid; name + connection meta + status badge (unchanged semantics); power button as glowing toggle (lit edge + glow ring when `data-on="true"`); sliders as gang-card sliders. **Per-light glow (additive JS):** `updateCard` sets `card.style.setProperty('--glow-bri', …)` (0–100) and `--glow-hue` (from the temp index) inside an `if (card.style && card.style.setProperty)` feature-detect so the Node DOM stub (no `style`) keeps passing untouched; the card's aura is `box-shadow`/pseudo-element computed from the custom properties (higher brightness = stronger halo; warmer temp = warmer halo tint). Off/disconnected/error → no glow (halo alpha 0), and the card-error banner keeps visual precedence over any glow.
7. **Error banner + card errors:** frosted rose-tinted glass, keep `role="alert"`/structure; error presentation always outranks glows.
8. **Footer:** centered dim line, unchanged text.

## 6. State-coverage requirement

Every visual treatment must be designed and verified for: mic unmuted/muted/unknown/unreachable; connection online/degraded; light on/off/error/disconnected/mixed-group outputs; settings list empty/populated + store-error line — in **both** themes, and at ~650 px and ~1440 px widths. A checklist runbook (screenshots) is part of the implementation plan's verification step.

## 7. Accessibility

- DOM order equals visual order after the mic move; no focus-trapping elements added.
- All interactive elements keep `focus-visible` outlines — restyled to the signal cyan ring, never removed.
- Pure-CSS/sibling-driven state visuals mean no new live-region behavior; existing `aria-live` nodes and roles remain exactly as pinned.
- Contrast per §3.2 in both themes.

## 8. Performance constraints

- `backdrop-filter` budget: rail cards + topbar pill + each light card = O(5–10) blurred surfaces on one page; no nested blur-in-blur stacking. If jank appears with many panels, reduce card blur to 10px or swap grid cards to an opaque surface — never animate backdrop-filter.
- The color-field backdrop is one static pseudo-element; no per-frame GPU work. Poll renders unchanged (signature-gated innerHTML rewrite logic untouched).

## 9. Testing & verification strategy

1. **Existing suite:** `go test ./...` must be green with **zero edits to test logic**; the suite itself is the regression net for the contract in §2. (`internal/lightui/mutation_queue_test.js` similarly untouched; standalone Node gate passes.)
2. **Pinned-contract verification (automated, additive):** before-opening-PR step diffs a machine-extracted list of every string literal asserted against `lightUIHTML` in `ui_test.go` between pre-redesign and post-redesign page content — all must still be present `go test` green suffices for enforcement; the diff list is a review aid.
3. **Manual visual runbook:** build the Windows binary (`./build.sh`), run the daemon + panel locally, and capture screenshots for every state cell in §6 in both themes (Chrome device emulation `prefers-color-scheme`, or inspect in light via OS toggle). Drag sliders to check warm/cool tracks and card glow response; confirm the rail stays pinned while scrolling a long panel grid (synthesize ≥6 fake light cards if hardware isn't present — devtools-only, no code hooks added).

## 9.1 Visual north-star

The chosen direction mockup lives at `.superpowers/brainstorm/2776365-1787011390/content/directions.html` (option B, "Glassline"). The redesign should read as a faithful, full-fidelity realization of that mockup — same backdrop fields, glass treatment, pill controls, and airy type — adapted to the command-deck layout from `hierarchy.html` option C ("Command deck"). If the implementation diverges from the mock in any visible way, the divergence is a bug unless deliberately decided in review.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Pinned-fragment drift while re-skinning markup | Fragment list above; `go test ./...` after every markup session; move-only mic section (no content edits). |
| `backdrop-filter` cost or absence | Bounded surface count; graceful opaque fallback via `@supports not (backdrop-filter: …)` raising glass alpha. |
| Light-glass contrast misses | Token values chosen with §3.2 targets; runbook includes a contrast spot-check pass. |
| `updateCard` additive style writes breaking DOM-stub tests | Feature-detect gate (`card.style && card.style.setProperty`) — verified by the untouched Node tests. |
| `position: sticky` rail weirdness with long grids | Sticky only ≥1100 px; drop to static below; manual scroll runbook item. |
| Single-file edit bottleneck vs. parallel subagents | Serial core edit owned by one worker; parallelism applied to token derivation, runbook execution, and review — not to concurrent edits of `index.html`. |

## 11. Out of scope

Tray icons/menus, Stream Deck key actions, daemon endpoints and UDP protocol, `mutation_queue.js`, build/deploy scripts, and any new feature (no themes toggle UI, no drag-reordering, no renaming from the panel).
