# Glassline — Test Refresh Map (`glassline-test-refresh-map.md`)

- **Date:** 2026-08-17
- **Status:** Handoff aid — maps the old `ui_test.go` pin categories onto the redesigned page (`internal/lightui/index.html`, Glassline command-deck). All counts/line numbers below were derived from the FINAL file on branch `ui-redesign-glassline` with the grep commands shown; re-run the command after any page edit rather than trusting the line numbers.
- **Companion evidence:** dark matrix `/tmp/opencode/verify/matrix.json` (52 checks, 0 fail) · light matrix `/tmp/opencode/verify/matrix-light.json` (50 checks, 0 fail) · light contrast `/tmp/opencode/verify/contrast-light.json` (6/6) · screenshots under `/tmp/opencode/verify/shots/`.

---

## 1. Pin-category map

| Old pin (category) | New-page equivalent | How the suite should re-pin |
|---|---|---|
| `data-mic-action` trio + Toggle `disabled` | **Unchanged selectors/attrs.** Three buttons keep `data-mic-action="mute|unmute|toggle"`; Toggle still ships `<button class="button-quiet" type="button" data-mic-action="toggle" disabled>Toggle</button>` (index.html:571). | Keep the literal pins, including `data-mic-action="toggle" disabled` and the Mute/Unmute-must-start-armed negative pins. File counts: `grep -c 'data-mic-action=' internal/lightui/index.html` → 3 (all in markup; JS binds via `querySelectorAll('[data-mic-action]')`). |
| `#mic-line` four texts | **Unchanged copy**, still rendered by `micLineText(micState)` (index.html:987) into `$("mic-line").textContent` (index.html:1019). | Re-pin each string: `"The microphone is muted."` / `"The microphone is live."` / `"Mic state is unknown since daemon start — Mute and Unmute still work (they are absolute); Toggle waits for a definitive state."` / `"Mic state is unreachable — the daemon is not answering, so the buttons are disarmed."` |
| `.status-badge[data-state=...]` rule presence (×8) | **Same attribute set, restyled.** All 8 state values across 5 recipe rules: `unmuted`/`on` (index.html:185), `muted`/`error` (:186), `unreachable` (:187), `off`/`unknown` (:188), `disconnected` (:189) — plus consumers `:has(.status-badge[data-state="disconnected"])` (:458) and a light-block override for the unreachable wash (:501). | Grep the final file for the token-driven rules: `grep -n '\.status-badge\[data-state' internal/lightui/index.html` → 7 lines; verify each of the 8 attribute values appears and the recipes reference `--live-text`/`--muted-text`/`--text-3`/`--unreach-border`/`--disc-border` tokens. |
| `.mic-lamp` sibling keying | **New.** All lamp styling keys off `#mic-status[data-state] ~ .mic-lamp` sibling selectors: unmuted face (index.html:168), muted face + `lamp-breathe` (:169), unreachable opacity (:170), unreachable dashed `::after` ring (:171), reduced-motion mute-kill (:519). | Pin `#mic-status[data-state="muted"] ~ .mic-lamp` presence (`grep -c` → 2: main + reduced-motion kill) and the `@keyframes lamp-breathe` count — exactly **1** (index.html:172). |
| 750 ms poll line | **Unchanged.** `window.setInterval(() => { refreshLights(true); refreshMic(); refreshSettings(); }, 750);` at index.html:1219. | Re-pin the literal line verbatim (`grep -c` → 1). |
| `settingsNameOverByteCap` gate ×2 | **Unchanged call sites.** Helper at index.html:1068; both gates `if (settingsNameOverByteCap(name)) {` → `grep -c` → **2** (save, delete); `showError(SETTINGS_NAME_TOO_LONG);` → 2. | Re-count in the final file; keep the exact-2 assertion. |
| `flushPendingSliders()` ×3 | **Unchanged call sites.** Definition index.html:891; call sites `flushPendingSliders();` → `grep -c` → **3** (save, apply, delete). | Re-count in the final file; keep the exact-3 assertion. |
| Node DOM-stub harness selectors (`.light-card`, `data-port`, etc.) | **Same hooks, new classes around them.** The Go-side stub has no innerHTML parsing; it pre-registers elements (`makeElement`) and binds `[data-mic-action]`, `[data-apply]`, `[data-delete]` via `stubRegister` — all unchanged. Light cards never enter the stub; card identity stays pinned by markup fragments (`data-port="${port}"`, `const target = port;`, `lights.map(cardMarkup)`). | Update stub queries only if element ids/`data-*` hooks change (they did not). Card-structure moves are class-level only — no stub changes required. |
| `?` template-var substitution | **Unchanged: none.** The page ships no template substitution; `ui.go` embeds `index.html` verbatim (`//go:embed internal/lightui/index.html`). | n/a — no pin to refresh. |
| Inline-script compile gate | **Unchanged.** Exactly one attribute-less `<script>` block remains (index.html:673; the only other tag is `<script src="/mutation_queue.js">` at :672). | Keep `new vm.Script` over the extracted inline block; assert `grep -n '<script' internal/lightui/index.html` → exactly 2 lines, one attribute-less. |

## 2. Expected suite-failure classes after the redesign (what to refresh)

1. **Fragment pins that quoted old markup/layout strings** (old DOM skeleton, old class names around the mic/gang/settings cards) — re-pin against the new skeleton; all *behavioral* hooks (`id`s, `data-*` attrs, mutation queue, poll, gates) are unchanged.
2. **Any pixel/color literal pins from the old theme** — the new page is token-driven; assert tokens and computed values, not raw RGB from the pre-Glassline stylesheet.
3. **Stub queries tied to old card markup ordering** — the command-deck rail reorders sections (mic → gang → settings); the DOM stub registers by id/hook, so only markup-fragment pins that asserted ordering need re-anchoring (section order itself is pinned by the existing gangControls < settings < individual offsets check, which still passes).

## 3. Verification appendix

### 3.1 Dark matrix (Task 7, re-run after Task 8 fixes as regression check)

- Harness: `/tmp/opencode/verify/verify12.mjs` + `harness-lib.mjs`; results `/tmp/opencode/verify/matrix.json` — **52 checks, 0 FAIL** (re-run after the Task 8 shared-CSS change: no regression).
- Screenshots: `/tmp/opencode/verify/shots/step*-dark-*.png` (naming `step<N>-<state>-dark-<width>.png`).

### 3.2 Light matrix (Task 8, `prefers-color-scheme: light` via CDP `Emulation.setEmulatedMedia`)

- Harness: `/tmp/opencode/verify/light-pass.mjs`; results `/tmp/opencode/verify/matrix-light.json` — **50 checks, 0 FAIL**. Light token block matches `design-light.md` §1 (drift diff: `--f1` vignette is a reviewed Task-4 addition; all §1.2–1.5 glass/text/accent tokens exact).
- Step 1 (connection/topbar): 6/6 PASS — online `#0e9670` dot + 3px halo ring (no bloom), degraded `#b06204`, offline, idle "Connecting" ink-.35 dot, converge. Shots `light-1-{online,degraded,offline,connecting}-{1440,650}.png`.
- Step 2 (mic ×4): 4/4 PASS — daylight bead keeps a deep silhouette rim per state (`#0b6349` live / `#9f1239` muted / `#7a80a3` unknown+unreachable — computed gradient asserted); badge recipes per design-light §3.2; breathing only in muted (2.6 s); Toggle disabled at unknown; all three disarmed + dashed orbital + lamp opacity .5 at unreachable. Shots `light-2-{unmuted,muted,unknown,unreachable}-{1440,650}.png`.
- Step 3 (gang): 4/4 PASS — Mixed/Mixed in dim ink `.66`; definite `55%`/`4950K` in ink after COM7 power-off; slider POST before settings POST in `mutations.log`; fill tracks thumb (`--fill-pct` 87) while chip reverts to Mixed. Shots `light-3-{mixed,definite,ordering,filltrack}-1440.png` (+650 for mixed).
- Step 4 (light cards): 9 checks PASS + 1 NOTE — white-frosted over pale field; warm 2900K casts an amber pool (`0 8px 28px -6px` R-dominant, alpha scales with brightness: c7 .264 > c5 .194); cool 6544K casts sky; **zero outer halos** (no `0 0 <blur>` bloom; the `inset 0 0 22px` rim-light is the spec'd signature); rim tint alpha follows brightness; off card = bare glass (border alpha ≈.02); disconnected = E1 base triple only (exact string match) + dashed badge + disabled controls; error card = rose `.45` border + strip inside, aura present but outranked; heater power `data-on=true` = mint ink edge, no icon drop-shadow. NOTE: `mixed` profile's 6500K is off the 19-step ladder (rig data, not a page defect). Shots `light-4-{default,mixed,errcard,disconnected}-1440.png` (+650 for default/errcard).
- Step 5 (settings): 11 checks PASS + 1 NOTE — empty `.35` wash + dashed; 1/2/3-row flows; hover deepens to ink `.22` (row + name input); apply/delete round-trip logged; store-error line in `#be123c`; 43-byte Save = zero network + `SETTINGS_NAME_TOO_LONG` banner; **press recipes via trusted input**: primary stays ink pill, secondary darkens to ink wash. NOTE: banner has no auto-timeout (adjudicated non-defect, old-page parity). Shots `light-5-{empty,default,saved,storeerror,toolong}-1440.png` (+650 empty).
- Step 6 (banner lifecycle): 3/3 PASS + 1 NOTE — frosted-rose banner (`rgba(190,18,60,.07)` bg / `.45` border / rose text / rose Retry), `role=alert`; Retry-while-degraded stays; recover+Retry fades + pill online. NOTE: silent-poll recovery does not clear (adjudicated non-defect). Shots `light-6-{banner,recovered}-1440.png` (+650 recovered).
- Step 7 (empty panels): 1/1 PASS + 1 NOTE — dashed two-part copy, `.35` wash; NOTE: section count reads `"0 panels"` (adjudicated non-defect, copy-freeze). Shots `light-7-empty-{1440,650}.png`.
- Step 8 (keyboard walk): 2/2 PASS — 29 stops @1440 / 28 @650, `#0c6b85` 2px ring + 5px halo on **every** leaf after the halo-append fix; offset 3px / 6px on sliders. Shots `light-8-ring-slider-1440.png`, `light-8-650.png`.
- Step 9 (motion): 2/2 PASS — light: only the muted lamp breathes; reduced-motion: zero animations, lamp pinned, instant badge. Shots `light-9-muted-{normal,reduced}-1440.png`.
- Step 10 (blur fallback): 2/2 PASS — `@supports`/light/motion wedges counted (2/2/2); forced no-backdrop panels go near-opaque porcelain `.92`. Shot `light-10-nofilter-1440.png`.
- Step 11 (console): 1/1 PASS — zero page-originated console messages across the whole light pass; browser network errors only on the deliberately failing profiles (`console-events-light.json`).

### 3.3 Light contrast assist (Task 8 brief Step 3)

- Script `/tmp/opencode/verify/contrast-light.mjs`; data `contrast-light.json`. Method: computed styles sampled in the live light page; alpha foregrounds composited over the real glass stack (`rgba(255,255,255,.60)`); WCAG ratios vs the design-light §6.1 field stacks, filtered to blobs the element can geometrically overlap (blobs are viewport-fixed). **All six pairs pass**:

| # | Pair | min ratio | target |
|---|---|---|---|
| 1 | `.light-name` on `.light-card` bg | 13.99 (lavender) | 4.5 |
| 2 | `.card-meta` on card | 5.05 (lavender) | 4.5 |
| 3 | `#mic-line` on panel | 4.75 (lavender) | 4.5 |
| 4 | badge text on badge bg (`data-state=on`) | 4.96 (sky) | 4.5 |
| 5 | muted word on panel (27px/800 large) | 5.16 (lavender) | 3.0 |
| 6 | footer text on body bg | 4.74 (rose) | 4.5 |

### 3.4 Defects found in the light pass (all fixed, matrix re-run green)

1. Unreachable badge bg was the dark pink literal `rgba(255,135,163,.08)` → light override `rgba(190,18,60,.06)` (design-light §3.2). [`cf08652`]
2. `.empty` wash was dark-literal `rgba(244,246,255,.03)` → `rgba(255,255,255,.35)` in light (§3.4/§3.9). [`cf08652`]
3. `#settings-name:hover` / `.setting-row:hover` brightened white in light (§3.7 says light hovers darken) → deepen to `rgba(23,28,60,.22)` ink hairline. [`cf08652`]
4. Generic `button:active` painted `rgba(244,246,255,.12)` under light — secondary chips whitened on press; keyboard-pressed primaries risked white-on-ink → light block: press darkens to ink `.10`, primary stays `var(--b1-bg)`. [`cf08652`]
5. Button `:focus-visible` halo **replaced** the resting shadow (both specs lock the halo as appended; dark sampled only inputs, so it only surfaced in the light walk) → button families now append halo after their resting shadow. [`cf08652`]
6. Comment/order mismatch at the `#settings-name` halo rule (comment said "halo, then base"; rule lists base first) → comment corrected. [`78f42c6`]

### 3.5 Standing notes (adjudicated non-defects — do not "fix")

- Banner has no auto-timeout; silent-poll recovery does not clear it — Retry is the designed path (old-page parity).
- Section count copy reads "N panels" (copy-freeze).
- `--signal-strong` is defined nowhere and consumed nowhere — YAGNI; noted at spec level only.
