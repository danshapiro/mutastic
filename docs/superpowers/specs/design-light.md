# Glassline — Light Theme Design Spec (`design-light.md`)

- **Date:** 2026-08-17
- **Status:** Design proposal — pairs with `design-dark.md` (parallel); both serve the approved parent spec `2026-08-17-glassline-ui-redesign-design.md`.
- **Register:** Bright-field glassmorphism designed natively for daylight. Not an inversion: glass is white-on-bright (not alpha-flipped dark), "lit" states are expressed by *deepening* chromatic shadows and rims (bright glow is invisible on bright fields), and the primary pill is ink, answering the dark theme's white pill across the brightness axis.
- **Contrast basis:** every §6 ratio is computed against the **actual alpha-composited stack** (field blobs → glass tiers), not raw tokens. Compositing sampled at each blob's heart (max alpha), at the bare base, and at a two-blob overlap; backdrop blur of static fields ≈ the field's mean color, so composites are exact to ±1 RGB step.

---

## 1. Tokens

All tokens are CSS custom properties on `:root`, active when `@media (prefers-color-scheme: light)`. Shared tokens (motion, type, radii, focus recipe) match the dark theme exactly so the pair coheres.

### 1.1 Backdrop field

| Token | Value | Notes |
|---|---|---|
| `--field-base` | `#eef1fb` | Bright porcelain, cool-indigo lean. Luminance 0.881 — this is the lightest surface anything sits on. |
| `--field-lavender` | `radial-gradient(760px 560px at -8% -18%, rgba(150,120,235,.40), rgba(150,120,235,0) 62%)` | Heart composites to `#cbc1f5` (lum 0.574) — saturated enough to read *through* 60% glass. |
| `--field-sky` | `radial-gradient(820px 600px at 112% 2%, rgba(90,180,220,.38), rgba(90,180,220,0) 58%)` | Heart → `#b6daef` (lum 0.663). Sits behind the panels grid (right side). |
| `--field-rose` | `radial-gradient(900px 680px at 62% 118%, rgba(235,140,200,.32), rgba(235,140,200,0) 60%)` | Heart → `#edd1eb` (lum 0.696). Bottom-center glow behind the grid/footer zone. |

Field is static only — no animation, one pseudo-element (§2).

### 1.2 Glass surfaces — three elevations

Elevation is distinguished by **shadow depth + blur + opacity**, never by darkening the fill. All shadows are indigo-tinted; there is no black or gray shadow anywhere in the theme.

| Tier | Use | Background | Border | Backdrop blur | Outer shadow | Inner |
|---|---|---|---|---|---|---|
| **E1 — panel** | cards (mic, gang, settings), light cards, page error banner | `rgba(255,255,255,.60)` | `1px solid rgba(255,255,255,.72)` | `blur(20px) saturate(1.25)` | `0 1px 2px rgba(47,54,116,.05), 0 16px 40px -12px rgba(56,64,140,.18)` | `inset 0 1px 0 rgba(255,255,255,.90)` |
| **E2 — well** | control blocks, setting rows, inputs, track recesses, card-error strip | `rgba(255,255,255,.45)` | `1px solid rgba(255,255,255,.58)` | `blur(12px) saturate(1.2)` | `0 6px 16px -8px rgba(56,64,140,.12)` | `inset 0 1px 0 rgba(255,255,255,.70)` |
| **E3 — chip** | connection pill, badges, secondary buttons, brand tile | `rgba(255,255,255,.72)` | `1px solid rgba(255,255,255,.85)` | `blur(8px)` | `0 2px 8px -2px rgba(47,54,116,.14)` | `inset 0 1px 0 rgba(255,255,255,.95)` |

`inset 0 1px 0` is the **sun line** — the downlight highlight that sells "frosted" on a bright field. `@supports not (backdrop-filter: blur(1px))` fallback raises alpha only: E1→`.92`, E2→`.85`, E3→`.95` (all §6 checks pass trivially at higher white).

Reference composites (what text actually lands on):

| Stack | Resolved | Luminance |
|---|---|---|
| E1 over bare base (worst/lightest) | `#f8f9fd` | 0.948 |
| E1 over lavender heart | `#eae6fb` | 0.811 |
| E1 over sky heart | `#e2f0f9` | 0.853 |
| E1 over rose heart | `#f8edf7` | 0.872 |
| E1 over lavender+sky overlap | `#e5ebf9` | 0.829 |
| E2 over E1 over base | `#fbfcfe` | 0.973 |
| E3 over E1 over base | `#fdfdfe` | 0.983 |

### 1.3 Text ramp

Secondary/dim are alpha ramps keyed to the ink hue so they stay chromatically correct over any blob. Resolved values shown against the lightest E1 composite.

| Token | Value | Resolves to (on E1/base) | Min ratio (worst stack) | Use |
|---|---|---|---|---|
| `--ink` | `#171b2e` | — | **14.0:1** (worst: 13.96 over lavender) | names, values, headings, slider outputs, button labels on glass |
| `--text-2` | `rgba(23,27,46,.64)` | `#686b79` | **4.75:1** (over lavender heart) | descriptions, `#mic-line`, meta sentences |
| `--text-3` | `rgba(23,27,46,.66)` | `#646674` | **5.04:1** (over lavender heart) | dim metadata (port · connection), hint labels, footer, "off"-family text |
| `--text-disabled` | `rgba(23,27,46,.52)` | `#838691` | 3.45:1 (exempt; still legible) | disabled labels only |

*(Yes: `--text-3` deliberately sits at higher alpha than `--text-2` — "dim" on bright glass reads through reduced weight + smaller size, not through washed-out color. See §5 rule 1.)*

### 1.4 Accents and state hues (re-darkened for daylight)

| Token | Value | Min ratio vs all E1 composites | Use |
|---|---|---|---|
| `--signal` | `#0c6b85` (deep cyan) | **4.98:1** text / **3.61:1** on bare lavender field | focus ring, eyebrows, slider thumb ring-on-focus, brand glyph. ≥3:1 on every raw field position (5.38 / 3.61 / 4.13 / 4.32 / 3.78). |
| `--signal-strong` | `#09576b` | 7.7:1 | hover/active form of `--signal` |
| `--mint-deep` | `#0a6e50` | **5.12:1** | LIVE/unmuted text, on-badge text, power-on accents |
| `--rose-deep` | `#be123c` | **5.15:1** | MUTED text, error text, delete, unreachable |
| `--amber-deep` | `#8a5003` | **5.33:1** | warm trim text, warm hint accent |
| `--sky-deep` | `#0369a1` | **4.86:1** | cool trim text, cool hint accent |
| `--dim-state` | `#5a6079` | **5.09:1** | UNKNOWN/UNREACHABLE/off state words + badge text |
| `--live-fill` | `#0e9670` | 3.68:1 on E3 chip | online connection dot (non-text) |
| `--degraded-fill` | `#b06204` | 4.50:1 on E3 chip | degraded connection dot (non-text) |
| `--track-warm-end` | `#a34d08` | 3.77:1 min vs track | warm end of temperature ribbon |
| `--track-cool-end` | `#0369a1` | 3.86:1 min vs track | cool end of temperature ribbon |

State semantics are **locked**: muted = rose/attention, live = calm mint, unknown/disconnected = dim. Accent hues never appear on decorative chrome (§5 rule 6).

### 1.5 Shape, spacing, motion, type (shared with dark — fixed spec)

| Token | Value |
|---|---|
| `--r-6 / --r-10 / --r-14 / --r-20` | `6px / 10px / 14px / 20px`; pills `999px` |
| `--t-state` | `140ms ease-out` (state/color/border/shadow transitions; allowed band 120–160ms) |
| press | `transform: translateY(1px)` on `:active` |
| hover lift | `transform: translateY(-1px)` (never exceeds 2px) |
| lamp breathe | `2.6s ease-in-out infinite`, only while mic `data-state="muted"` |
| reduced motion | all transitions/animations → `0.01ms` under `prefers-reduced-motion: reduce` |
| focus ring | `outline: 2px solid var(--signal); outline-offset: 3px` **+** `box-shadow: 0 0 0 5px rgba(12,107,133,.18)` appended to resting shadow |
| type scale | `11 / 12 / 13 / 14 / 16 / 20 / 27px` |
| state words | `27px`, weight 800, uppercase, `letter-spacing: -.01em` |
| eyebrows | `11px` (.69–.72rem), weight 750, uppercase, `letter-spacing: .13em`, color `--signal` |
 | card names / section h2 | `14px` weight 700 / `20px` weight 800 |
| values (%, K, port meta digits) | `font-variant-numeric: tabular-nums`, weight 700 |
 | spacing | `4 / 8 / 12 / 16 / 20 / 28px` scale; card padding `17px`; grid gap `16px` |

---

## 2. Backdrop field — exact CSS

One fixed pseudo-element (parent spec §2/§5 — never intercepts clicks, never animates):

```css
body { background: var(--field-base); }
body::before {
  content: "";
  position: fixed;
  inset: 0;
  z-index: -1;
  pointer-events: none;
  background:
    /* porcelain vignette on top, concentrates the field */
    radial-gradient(900px 1000px at 50% 38%, transparent 55%, rgba(200,205,240,.35) 100%),
    radial-gradient(760px 560px at -8% -18%, rgba(150,120,235,.40), rgba(150,120,235,0) 62%),
    radial-gradient(820px 600px at 112% 2%,  rgba(90,180,220,.38),  rgba(90,180,220,0) 58%),
    radial-gradient(900px 680px at 62% 118%, rgba(235,140,200,.32), rgba(235,140,200,0) 60%);
}
```

Why these alpha values: at `.40/.38/.32` the blob hearts land at `#cbc1f5 / #b6daef / #edd1eb` — visibly lavender, sky, and rose *behind* 60% white glass (the blur tint differs per region, which is what makes glass read as glass), while the deepest stack (E1 over lavender, `#eae6fb`) still holds ink at 13.96:1. Do not raise the blob alphas: beyond ~.45 the lavender heart starts costing mint-text contrast (§6 row 8).

---

## 3. Component treatments

Layout follows the approved command-deck (parent spec §3): rail `minmax(300px, 340px)` sticky at `top: 28px`, main column `1fr`, topbar spans both, single column below 1100px in DOM order. Everything below is light-theme surface treatment.

### 3.1 Topbar

- **Brand tile:** 42×42, radius 14, **E3**. Glyph (sun) `#0c6b85` at 22px. Resting shadow = E3 shadow. This is the only place `--signal` is decorative — it is the brand/focus voice, not a state.
- **Brand eyebrow** ("MUTASTIC / LOCAL CONTROL"): eyebrow spec, `--signal`.
- **Connection pill:** **E3** chip, `padding: 8px 12px`, radius 999, text 12px `--text-2` (5.12:1 on chip). Dot 8px, radius 50%:
  - `data-state="online"`: dot `#0e9670`, `box-shadow: 0 0 0 3px rgba(14,150,112,.18)`. No glow-bloom — the halo ring is the daylight legible form.
  - `data-state="degraded"` ("Daemon issue" / "Panel offline"): dot `#b06204`, `box-shadow: 0 0 0 3px rgba(176,98,4,.18)`.
  - `data-state="idle"` (initial "Connecting", no ellipsis — dim-neutral in both themes): dot `rgba(23,27,46,.35)`, `box-shadow: 0 0 0 3px rgba(23,27,46,.10)`.
  - No pulse on the dot in either state — page motion budget belongs to the muted lamp alone.

### 3.2 Mic hero card (rail)

E1 panel, radius 20, padding 24. Interior order: header row (eyebrow "MICROPHONE" + `#mic-status` badge), **lamp + state word block**, `#mic-line` (`--text-2`, 13px), button row.

**State word** (27px / 800 / uppercase / -.01em, tabular where digits):

| `data-state` | Word | Color | Badge treatment |
|---|---|---|---|
| `unmuted` | LIVE | `--mint-deep` | bg `rgba(10,110,80,.06)`, border `1px rgba(10,110,80,.40)`, text `--mint-deep` (4.73–5.47:1) |
| `muted` | MUTED | `--rose-deep` | bg `rgba(190,18,60,.08)`, border `1px rgba(190,18,60,.38)`, text `--rose-deep` |
| `unknown` | UNKNOWN | `--dim-state` | bg `rgba(23,27,46,.05)`, border `1px rgba(23,27,46,.12)`, text `--dim-state` |
| `unreachable` | UNREACHABLE | `--dim-state` | bg `rgba(190,18,60,.06)`, border `1px dashed rgba(190,18,60,.55)`, text `--rose-deep` — the dashed edge stays dashes on the badge's `data-state="unreachable"` state |

**The daylight lamp.** Additive, `aria-hidden`, 56px circle, placed as a following sibling of `#mic-status` so all styling keys off sibling selectors (`#mic-status[data-state="muted"] ~ .mic-lamp …`). Construction = three layers, per state:

1. **Core** — `radial-gradient(circle at 34% 30%, <glow> 0%, <mid> 55%, <rim> 100%)`. The off-center pale stop is the specular "sun catch"; the deep rim keeps the silhouette ≥3:1 against any glass (this is what bright-glow lamps lose in daylight).
2. **Inner depth** — `inset 0 -6px 12px <shade>` (bottom weight) + `inset 0 2px 3px rgba(255,255,255,.65)` (top glassiness).
3. **Signal shadow** — real `box-shadow` in the state hue at mid luminance: `0 0 0 3px <hue/.14>, 0 8px 22px <hue/.30>`. Saturated color, never white — reads as "this bead is lit" against pastel.

| State | glow / mid / rim | shade | signal shadow | Silhouette vs E1 |
|---|---|---|---|---|
| unmuted (LIVE) | `#f4fefb` / `#17b98b` / `#0b6349` | `rgba(6,60,45,.25)` | `rgba(10,90,66,·)` — calm, static | **6.89 / 5.94:1** |
| muted | `#fff6f7` / `#ee4a6e` / `#9f1239` | `rgba(90,8,28,.28)` | `rgba(159,18,57,·)` + breathing ring | **7.62 / 6.57:1** |
| unknown | `#f7f8fd` / `#a7accb` / `#7a80a3` | `rgba(70,76,110,.18)` | `rgba(90,96,121,.16)` faint | **3.67 / 3.16:1** |
| unreachable | as unknown | as unknown | static dashed ring: `::after { inset: -6px; border: 2px dashed rgba(110,116,148,.55); border-radius: 50%; }` | **4.35 / 3.75:1** |

Breathing (muted only), on `::before` (core scale) and `::after` (sonar ring):

```css
@keyframes lamp-core-breathe { 0%,100% { transform: scale(1); } 50% { transform: scale(1.045); } }
@keyframes lamp-ring-breathe { 0%,100% { transform: scale(.86); opacity: .6; } 50% { transform: scale(1.18); opacity: 0; } }
#mic-status[data-state="muted"] ~ .mic-lamp        { animation: lamp-core-breathe 2.6s ease-in-out infinite; }
#mic-status[data-state="muted"] ~ .mic-lamp::after { inset: -6px; border-radius: 50%; border: 2px solid rgba(190,18,60,.5); animation: lamp-ring-breathe 2.6s ease-in-out infinite; }
```

No breathing for LIVE/unknown/unreachable (calm), all off under reduced-motion.

**Buttons:** `Mute` = primary (§3.6); `Unmute` = secondary; `Toggle` = quiet, starts `disabled` (behavioral invariant, parent spec §4.6). Disabled recipe §3.7. At `unreachable`, Mute/Unmute also disarm (existing JS) — same disabled recipe.

### 3.3 Gang / all-lights card (rail)

E1 panel. Control blocks become a vertical stack of **E2** wells, radius 14.

- **Power row:** `All on` = primary; `All off` = secondary; `Toggle` = quiet. 44px min height, `min-width: 88px` — shipped light-only as `.power-row[aria-label="All lights power"] button`.
- **Match brightness** slider + **Match warmth** slider — construction §3.5. Labels: 12px two-tone — key span `--text-2` 650, trailing span `--text-3`, no tracking; outputs 13px/700, `--ink`, tabular; "Unknown" / "Mixed" in `--text-3` (same tabular slot; the word is the state, not the color).
- **Trim rows:** four `-5/-1/+1/+5` secondary chips (`min-width: 58px`), and `Warmer` / `Cooler` quiet buttons with icon + text in `--amber-deep` / `--sky-deep` (6.18:1 / 5.64:1). The trim hues are *hints*, not state — they never fill.
- **Refresh** (panel head): quiet icon button, `--text-2`, hover `--ink`.

### 3.4 Saved-settings card (rail)

- **Name input:** E2 well, radius 10, 44px, `padding: 10px 14px`, text `--ink`, placeholder `--text-3` (5.40:1 on the well), caret `--signal`. Focus: global ring recipe (well keeps its own border underneath — the 3px offset keeps both visible).
- **Save:** primary, same row as input.
- **Setting rows:** E2 chips, radius 10, `padding: 6px/12px`, name `--ink` 13px with ellipsis, **Apply** = small secondary (36px), **Delete** = small destructive-quiet: text `--rose-deep`, hover `background: rgba(190,18,60,.08)` (text 5.23:1 on hover bg).
- **Empty state:** `1px dashed var(--disc-border)` (`rgba(23,27,46,.24)`), radius 14, bg `rgba(255,255,255,.35)`, padding 30px 20px, centered; headline `--ink`, body `--text-2`.
- **Error line** (store refusal render target): 12px `--rose-deep` — it replaces the hint line in place, no extra box (the page banner owns boxed errors).

### 3.5 Sliders (gang + per-card, identical construction)

Track is a **recessed ink-wash well** on the glass — the one place the light theme darkens to create depth, because a white track on white glass is invisible.

```css
track  { height: 6px; border-radius: 999px;
         background: rgba(23,28,60,.12);
         box-shadow: inset 0 1px 2px rgba(23,28,60,.14), 0 1px 0 rgba(255,255,255,.55); }
thumb  { width: 20px; height: 20px; border-radius: 50%; background: #fff;
         border: 1.5px solid rgba(23,28,60,.28);
         box-shadow: 0 0 0 1.5px rgba(23,28,60,.50), 0 2px 5px rgba(23,28,60,.25); }
```

The thumb's white face is deliberately near-invisible (1.2:1); its **identity is the ink ring** (5.05:1 on track over base, 4.69:1 over lavender) plus drop shadow — iOS-pattern daylight thumb. Hover deepens ring to `.60`; drag (`:active`) keeps ring + raises shadow to `0 3px 8px rgba(23,28,60,.30)`. Focus: `input[type=range]:focus-visible { outline: 2px solid var(--signal); outline-offset: 6px; box-shadow: 0 0 0 5px rgba(12,107,133,.18); }` (offset widened to 6px so the ring clears the thumb).

- **Brightness track fill** (progress, where feasible pure CSS — `::-moz-range-progress`; Chromium keeps the ticked track + ringed thumb, outputs carry exact numbers): `linear-gradient(90deg, #0b7c98, #087a56)` — 3.60:1 / 3.99:1 vs track over base; 3.14 / 3.48 over lavender. This is the dark theme's cyan→mint fill, re-darkened, answering across the pair.
- **Warmth track** — full-width physical temperature ribbon (no fill; position on the map *is* the value):

```css
background:
  /* 19-step honesty ticks — 1px hairlines at each 1/18 boundary */
  repeating-linear-gradient(90deg,
    transparent 0 calc(5.5556% - 1px),
    rgba(23,28,60,.30) calc(5.5556% - 1px) 5.5556%),
  linear-gradient(90deg,
    #a34d08 0%, #d8913f 24%, #f2ead9 42%, #dee9f3 55%, #7fb4e2 78%, #0369a1 100%);
```

  Endpoints measured: warm 3.77–4.32:1, cool 3.86–4.43:1 vs track. The pale mid-stops (`#f2ead9`/`#dee9f3`, ~1:1 vs track) are **intentional**: 4950K is untinted light, the midpoint reads as "no color cast", and value indication is carried by the ringed thumb + K output, not the ribbon. Ticks (1.8:1, decorative) make the 19 discrete steps visible and honest — the slider steps, and the UI says it steps.
- **Hint labels:** "2900K warm" / "7000K cool", 11px `--text-3`; may tint the words "warm"/"cool" `--amber-deep`/`--sky-deep`.
- **Disabled slider** (light off/disconnected): `opacity .5`, thumb ring drops to `.30`, no fill.

### 3.6 Buttons — tier recipes

Shared: radius 999, min-height 44px, weight 650–760, transitions `var(--t-state)`, press `translateY(1px)`, focus per global recipe, `disabled { cursor: not-allowed }`.

| Tier | Recipe |
|---|---|
| **Primary** | `background: #1f2438; color: #f8f9ff; border: 1px solid #1f2438; box-shadow: 0 8px 20px -6px rgba(31,36,56,.35), inset 0 1px 0 rgba(255,255,255,.14)`. White text 15.35:1. **Hover:** bg `#2a3150` (12.70:1) + lift. **Active:** press. **Disabled:** `background: rgba(31,36,56,.50); color: rgba(255,255,255,.95); box-shadow: none;` (3.09:1, reads "asleep", not "gone"). Justification §4.1. |
| **Secondary** | E3 chip + anti-white-on-white hairline: `background: rgba(255,255,255,.55); border: 1px solid rgba(23,28,60,.16); box-shadow: inset 0 1px 0 rgba(255,255,255,.9), 0 1px 2px rgba(47,54,116,.08); color: var(--ink)`. **Hover:** bg alpha .70, border `.22`, lift. |
| **Quiet** | transparent; color `--text-2`; hover `background: rgba(23,28,60,.06); color: var(--ink)` (light-theme hovers *darken* flat buttons). |
| **Destructive-quiet** (Delete) | quiet + text `--rose-deep`; hover `rgba(190,18,60,.08)`. |
| **Warm/Cool quiet** (trim) | quiet + `--amber-deep` / `--sky-deep` text+icon. |
| Small variants (Apply/Delete, trims) | min-height 36px, padding `6px 10px`. |

### 3.7 Global interaction states

- **Hover:** lift `translateY(-1px)` on primary/secondary; background treatments per §3.6; quiet darkens.
- **Active:** `translateY(1px)`, shadows shorten (remove the 8–16px layer, keep 1–2px contact shadow).
- **Focus-visible:** `outline: 2px solid #0c6b85; outline-offset: 3px`, **plus** halo `box-shadow: 0 0 0 5px rgba(12,107,133,.18)` **appended after** the element's resting shadow (never replacing it). Ring contrast: ≥3.61:1 on every raw field position, ≥4.39 everywhere on glass. Outlines are never removed; the halo alone never carries focus (§5 rule 5).
- **Disabled:** per recipe above; never just "lower opacity until unreadable" (§5 rule 1).

### 3.8 Light cards (main grid)

E1 panel, radius 14, padding 17. Header: name (14px/700 `--ink`) + meta line (12px, `--text-3`, tabular — "COM7 · connected") + status badge (§3.2 badge recipes, states `on`/`off`/`error`/`disconnected`). Power toggle: quiet chip — **`data-on="true"` adds** `color: var(--mint-deep); border: 1px solid rgba(10,110,80,.85)` (4.52:1, computed border, not adjective) `background: rgba(10,110,80,.05)`; **`data-on="false"`** is plain quiet. Sliders per §3.5; disabled when off.

**The bright-field aura (replaces dark-theme glow).** Daylight rule: *light deepens, it does not brighten.* The card announces "lit" through a **chromatic rim + hue-carrying drop shadow** — a colored *shadow* stays visible on a pastel field where a bright halo washes out. Driven by the two feature-detected custom properties (parent spec §2, signature details): `--glow-bri` (0–100, brightness) and `--glow-hue` (0–18, temperature index).

```css
.light-card {
  --aura-h: calc(38 + var(--glow-hue, 9) * 9.11);   /* 38° amber → 202° sky across the 19 steps */
  border-color: hsl(var(--aura-h) 76% 42% / calc(.02 + var(--glow-bri, 0) * .0026));   /* rim tint rides the border */
  box-shadow:
    0 1px 2px rgba(47,54,116,.05),                                              /* E1 base, always */
    0 16px 40px -12px rgba(56,64,140,.18),                                      /* E1 base, always */
    0 8px 28px -6px hsl(var(--aura-h) 82% 46% / calc(.04 + var(--glow-bri, 0) * .0028)),  /* chromatic cast */
    inset 0 0 22px hsl(var(--aura-h) 88% 58% / calc(var(--glow-bri, 0) * .0013)),       /* inner rim-light */
    var(--g1-inner);                                                            /* sun line */
  transition: border-color var(--t-state), box-shadow 160ms ease-out;
}
```

(`--glow-bri`/`--glow-hue` arrive as per-card inline custom properties from JS — 0 when off/error/disconnected — so the `, 0`/`, 9` fallbacks in the calc are the pre-JS rest state.)

- At `--glow-bri: 100`, 2900K: cast `hsla(38°,82%,46%,.32)` — a deep amber shadow pooling under the card; rim `hsla(38°,76%,42%,.28)` carried on `border-color`; inner warm rim `.13`. At 7000K the same weights arrive in sky `202°`. At 0 the aura weights land at `.04/.02/0` — bare glass.
- **Hue map (19 steps):** `h = 38 + i × 9.11` — i=0→38.0, i=9 (≈4950K)→120.0 (neutral sage-crossing, the midpoint stops looking amber *or* blue, matching the ribbon's pale midpoint), i=18→202.0.
- **Precedence (locked):** aura exists only when the light is on and healthy. Off / disconnected → JS writes 0. Error → 0 **and** the card takes `border: 1px solid rgba(190,18,60,.45)` via `.light-card:has(.card-error:not([hidden]))`; the error banner outranks any glow visually. (`:has()` no-ops harmlessly where unsupported — legibility never depends on it.)
- `disconnected` cards additionally quiet the whole surface: `.light-card:has(.status-badge[data-state="disconnected"]) { box-shadow: 0 1px 2px rgba(47,54,116,.05), 0 16px 40px -12px rgba(56,64,140,.18), var(--g1-inner); border-color: var(--g1-border); }` + meta/badge already dim; sliders disabled per §3.5.

### 3.9 Page error banner, empty-panels, footer

- **Page error banner** (`role="alert"`): frosted rose glass — `background: rgba(190,18,60,.07)` **over** an E1 body (composite `#f4deea` worst-case rose-on-rose), `border: 1px solid rgba(190,18,60,.45)`, radius 14, text 13px `--rose-deep` (4.93–5.31:1), alert icon `--rose-deep`, **Retry** = small secondary with `--rose-deep` text. Inside the gang card, below controls; margin `16px 0 0`.
- **Empty-panels state:** dashed `1px var(--disc-border)` (`rgba(23,27,46,.24)`), radius 14, bg `rgba(255,255,255,.35)`, padding `30px 20px`, centered; `<strong>` `--ink`, span `--text-2`.
- **Footer:** centered, 11px, `--text-3` (5.21:1 directly on field base).

---

## 4. Signature details

1. **Ink/paper primary mirror.** Dark Glassline's primary is a `.94` white pill with ink `#1a1c2e` text; the light primary is ink `#1f2438` with `#f8f9ff` text (15.35:1) — the two themes answer each other across the brightness axis instead of one being derived from the other. Reserved strictly for true verbs (Mute, Save, All on); never tinted by state hues.
2. **Kelvin-true aura.** "Lit" is a *deepening* chromatic shadow + rim whose hue interpolates amber 38° → sky 202° across the 19 temperature steps and whose alpha is formula-driven by `--glow-bri` — a light at 30%/6500K reads faint and cool, at 90%/2900K warm and strong. Bright halos are never used (they vanish on pastel).
3. **Daylight-engineered lamp.** A glass-bead construction — off-center specular catch, deep silhouette rim (5.9–7.6:1 against any glass stack), hue-carrying signal shadows in place of glow — with a muted-only breathing sonar ring; calm states are static, and the ring dies to opacity 0 at peak so the pulse never accumulates.
4. **The warmth ribbon admits it's stepped.** Nineteen 1px honesty ticks over a physical temperature gradient whose 4950K midpoint is deliberately *untinted* (pale on purpose); deep amber/sky endpoints carry the 3:1 measurement, the ringed thumb + tabular K output carry the value.
5. **Sun-line + ink-hairline discipline.** Every glass tier gets `inset 0 1px 0` white at .70–.95 (overhead daylight) while every floating chip/button carries an ink hairline (`rgba(23,28,60,.16–.28)`), so nothing ever floats borderless white-on-white — and no shadow anywhere in the theme is gray or black, only indigo.

## 5. Anti-cliché guardrails

1. **Never wash out text.** Minimum body contrast is 4.5:1 against the *composited* stack. "Dim" is expressed by size/weight, never below `--text-3` (`rgba(23,27,46,.66)`, 5.04:1 worst). No `.4`-alpha paragraphs, no pastel text, no white text on glass.
2. **Never a white or accent-filled primary.** White-on-white-glass is invisible and accent-fill primaries counterfeit state semantics. Primary is always ink `#1f2438`, and ink is used for *nothing else decorative*.
3. **Never signal "lit" by brightening.** On bright fields, active/lit/attention = deeper hue: chromatic rim, saturated signal shadow, denser border. A bright box-shadow "glow" on this field is a bug.
4. **Never a gray or black shadow.** All elevation and aura shadows are indigo/state-hued (`rgba(47,54,116,·)` family). A neutral gray shadow kills the pastel field's color logic instantly.
5. **Never focus by halo alone.** The 2px solid `#0c6b85` ring (≥3.61:1 on every raw field position) is the contract; the 5px 18% halo is additive warmth. Never remove or restyle the outline away.
6. **Never spend a state hue on chrome.** Rose/amber/mint/mint-surfaces mean *things* (muted/degraded-warm/live). Brand and focus speak signal cyan + ink only; trim hints may tint text but never fill; no decorative gradients in state hues.

## 6. Verification checklist

### 6.1 Computed contrast (WCAG 2.1 formula over the real composite)

Worst-case stacks: E1 over bare base `#f8f9fd` ("E1·A"), E1 over lavender heart `#eae6fb` ("E1·B", the gating case for green/cyan hues), E1 over sky `#e2f0f9` ("E1·C"), E1 over rose `#f8edf7` ("E1·D"). Targets: ≥4.5 body, ≥3.0 large state words (27px/800 qualifies as large), ≥3.0 non-text UI.

| # | Pair | Ratio (min across stacks) | Target | Pass |
|---|---|---|---|---|
| 1 | `--ink` `#171b2e` on E1 panels | 13.96 (E1·B) … 16.19 (E1·A) | 4.5 | ✅ |
| 2 | `--text-2` `.64` on E1 | 4.75 (E1·B) | 4.5 | ✅ |
| 3 | `--text-3` `.66` on E1 | 5.04 (E1·B) | 4.5 | ✅ |
| 4 | `--text-disabled` `.52` on E1 | 3.45 | exempt (held ≥3) | ✅ |
| 5 | State word LIVE `--mint-deep` on E1 | 5.12 | 3.0 | ✅ |
| 6 | State word MUTED `--rose-deep` on E1 | 5.15 | 3.0 | ✅ |
| 7 | State word UNKNOWN/UNREACH `--dim-state` | 5.09 | 3.0 | ✅ |
| 8 | Badge on/unmuted (`--mint-deep` on mint-tint `.06` chip) | 4.73 (over E1·B) | 4.5 | ✅ |
| 9 | Badge muted/error (`--rose-deep` on rose `.08` chip) | 4.52 (over E1·B) | 4.5 | ✅ |
| 10 | Badge off/unknown (`--dim-state` on ink `.05/.06` chip) | 4.62 | 4.5 | ✅ |
| 11 | Page error text `--rose-deep` on banner composite | 4.93 (rose-on-rose worst) | 4.5 | ✅ |
| 12 | Delete / Retry `--rose-deep` on `rgba(190,18,60,.08)` hover | 5.23 | 4.5 | ✅ |
| 13 | Primary: `#f8f9ff` on `#1f2438` / hover `#2a3150` | 15.35 / 12.70 | 4.5 | ✅ |
| 14 | Primary vs resting surface (button identification) | 14.59 | 3.0 | ✅ |
| 15 | Primary disabled: white `.95` on ink `.50` composite | 3.09 | exempt (held ≥3) | ✅ |
| 16 | Focus ring `#0c6b85` on raw field | 5.38 base / 3.61 lav / 4.13 sky / 4.32 rose / 3.78 overlap | 3.0 | ✅ |
| 17 | Eyebrow `--signal` on E1 | 4.98 | 4.5 | ✅ |
| 18 | Warm trim `--amber-deep` / Cool trim `--sky-deep` on E1 | 5.33 / 4.86 | 4.5 | ✅ |
| 19 | Lamp silhouette rims on E1 · live `#0b6349` / muted `#9f1239` / unknown `#7a80a3` / unreach `#6e7494` | 5.94 / 6.57 / 3.16 / 3.75 | 3.0 | ✅ |
| 20 | Connection dots on E3 · online `#0e9670` / degraded `#b06204` | 3.68 / 4.50 | 3.0 | ✅ |
| 21 | Brightness fill `#0b7c98→#087a56` vs track | 3.14–3.99 | 3.0 | ✅ |
| 22 | Warmth ribbon endpoints `#a34d08` / `#0369a1` vs track | 3.77 / 3.86 | 3.0 | ✅ (mid-stops intentionally pale; value carried by thumb+output) |
| 23 | Thumb identity ring `rgba(23,28,60,.50)` vs track | 4.69 (over E1·B) | 3.0 | ✅ |
| 24 | Power-on border `rgba(10,110,80,.85)` on E3 chip | 4.52 | 3.0 | ✅ |
| 25 | Input text/placeholder on E2 well | 16.60 / 5.40 | 4.5 | ✅ |
| 26 | Footer/hints `--text-3` directly on field base | 5.21 | 4.5 | ✅ |

*Method note: blob hearts sampled at full token alpha; two-blob overlap at .65/.55 strength; glass tiers composited in stack order (field → E1 → E2/E3). Blur of static fields ≈ mean color. All numbers reproducible from §1–§3 values.*

### 6.2 State matrix — sign-off grid

Verify each cell at **1440px** (rail sticky left, grid ≥2 cols) and **650px** (single column, rail unstickied, order = topbar → mic → gang → settings → panels → footer). Each cell = appearance + specified state behavior + focus ring visible on tab.

| Component | States to capture | Pass criteria (spot) |
|---|---|---|
| Topbar | online ("Daemon connected") / degraded ("Daemon issue" / "Panel offline") | dot + halo correct hue; pill text 12px; brand tile sun line visible |
| Mic hero | unmuted / muted / unknown / unreachable | lamp palette per §3.2; breathing ONLY muted (2.6s); word + badge + line text per table; Toggle disabled until first definitive state; Mute/Unmute disarm at unreachable |
| Mic buttons | hover / focus / active / disabled (each tier) | ink primary hover `#2a3150` + 1px lift; quiet hover darkens; focus ring offset clears neighbors |
| Gang | power row; match brightness 0/55/100 (incl. "Mixed"/"Unknown" outputs); match warmth steps 0/9/18 (2900K/4950K/7000K); trim rows | thumb ring visible at every position; ribbon pale midpoint at step 9; ticks visible |
| Saved settings | empty state; 1 row; ≥4 rows (long name ellipsis 42-byte cap); store-error line | dashed empty; Apply/Delete tiers correct; error line rose 13px |
| Light cards | on (bri 30/100 × temp 2900/4950/7000) / off / error / disconnected | aura hue follows temp, alpha follows brightness; off = bare glass; error = rose edge + banner outranks aura; disconnected = dim + disabled sliders |
| Page error banner | shown, with Retry; then cleared | rose frosted band inside gang card; `role=alert`; Retry = rose-text secondary |
| Empty panels | no lights | dashed panel, centered copy |
| Footer | always | 11px `--text-3`, centered |
| Global | keyboard-only pass both widths | every interactive leaf shows the 2px signal ring + halo; tab order = DOM order (mic first) |
| Reduced motion | OS-level toggle both widths | no breathing, no lift/press motion (instant) |
| No-backdrop-filter | `@supports` fallback forced | glass alphas raise (E1 .92); field still visible at card rims via border + shadow |
