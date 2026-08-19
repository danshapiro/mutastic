# Glassline — Dark Theme Design Specification

- **Theme:** Dark (default; lives at `:root`, light theme overrides in the paired spec)
- **Baseline:** "Glassline" mockup, `directions.html` option B (`.dm.b`), elevated to production fidelity. Layout per "Command deck," `hierarchy.html` option C.
- **Coheres with:** the approved parent spec (`2026-08-17-glassline-ui-redesign-design.md`) and the fixed shared spec (motion band, type scale, radius scale, focus-ring formula). Items marked 🔒 are fixed shared spec — values are identical across both themes.
- **Locked state semantics:** muted/mic-error = rose (attention, only breathing element); live/on/online = calm mint; unknown/disconnected = dim.

Per rev 2 of the parent spec, the old test suite's byte-level pins no longer constrain selectors or structure; this theme satisfies the parent spec's behavioral invariants (§4) and may name/restructure selectors freely.

---

## 1. Tokens

All tokens are CSS custom properties on `:root`.

### 1.1 Backdrop field (colors, positions, stops — hues/opacities are the mockup's exact values; radii scaled ~1.4× for real viewports, plus one additive vignette)

| Token | Value | Notes |
|---|---|---|
| `--mut-field-base` | `#0c0c18` | page base, exactly the mockup |
| `--mut-field-violet` | `rgba(120,80,220,.55)` | radial `760px 500px` at `7% -14%`, fade to transparent at 62% |
| `--mut-field-cyan` | `rgba(20,150,190,.50)` | radial `700px 460px` at `105% 3%`, fade at 58% |
| `--mut-field-magenta` | `rgba(190,60,150,.45)` | radial `900px 620px` at `57% 132%`, fade at 64% |
| `--mut-field-vignette` | `radial-gradient(150% 120% at 50% 38%, transparent 55%, rgba(2,3,9,.46) 100%)` | additive; keeps edges grounded, concentrates the field |

### 1.2 Glass surfaces — three elevations

| Tier | Used for | background | border | backdrop-filter | outer shadow | inner shadow |
|---|---|---|---|---|---|---|
| **Glass-1** (card) | rail cards, light cards, top-level panels | `rgba(255,255,255,.075)` + `linear-gradient(180deg, rgba(255,255,255,.045), transparent 30%)` | `1px solid rgba(255,255,255,.155)` | `blur(20px) saturate(1.35)` | `0 24px 48px rgba(2,4,14,.50)` | `inset 0 1px 0 rgba(255,255,255,.15)` |
| **Glass-2** (inset) | control blocks, setting rows, text input, readout chips | `rgba(255,255,255,.05)` | `1px solid rgba(255,255,255,.11)` | **none — deliberately** (see §4.5) | `0 0 0 rgba(0,0,0,0)` | `inset 0 1px 0 rgba(255,255,255,.07)` |
| **Glass-3** (float) | connection pill, brand chip, badges | `rgba(255,255,255,.11)` | `1px solid rgba(255,255,255,.20)` | `blur(12px) saturate(1.3)` | `0 8px 20px rgba(2,4,14,.40)` | `inset 0 1px 0 rgba(255,255,255,.20)` |

Recessed variants (inputs, readout chips) swap the inner hairline for `inset 0 1px 3px rgba(2,4,14,.32), inset 0 -1px 0 rgba(255,255,255,.05)` and use `rgba(10,12,24,.32)` as fill. Fallback: under `@supports not (backdrop-filter: blur(1px))`, glass-1 bg rises to `rgba(19,21,42,.90)`, glass-3 to solid `#22253f`.

Zero-shadow tiers and zero-glow states carry the real value `0 0 0 rgba(0,0,0,0)`, never the keyword `none` — composite shadow stacks (element + tier token + appended halo/glow) must stay parseable with the token slotted in.

### 1.3 Text ramp (contrast computed in §6.1 against the worst-case glass composite `(85,64,141)`)

| Token | Value | Role |
|---|---|---|
| `--mut-text` | `#f4f6ff` | primary: headings, values, button ink on dark |
| `--mut-text-2` | `rgba(244,246,255,.82)` | secondary: body copy, `#mic-line`, subtle lines, connection pill text |
| `--mut-text-3` | `rgba(244,246,255,.68)` | dim: meta, hints, eyebrows, footer, badge-off text |
| `--mut-text-disabled` | `rgba(244,246,255,.40)` | disabled labels only (contrast-exempt) |
| `--mut-ink` | `#191b2b` | ink on light primary buttons |

### 1.4 Accents

| Token | Value | Use |
|---|---|---|
| `--mut-signal` | `#7de3ff` | focus rings, active slider thumb ring, "Cooler" trim, brand icon tint |
| `--mut-signal-halo` | `rgba(125,227,255,.20)` | 🔒 5px focus halo |
| `--mut-amber` | `#ffc97d` | "Warmer" trim, degraded connection dot |
| `--mut-mint` | `#58f0c8` | live/on/online fills, lamp mid-tone, connection dot online |
| `--mut-mint-text` | `#8df5d9` | mint text/badges on glass |
| `--mut-mint-word` | `#7df4d6` | mic state word, LIVE |
| `--mut-rose` | `#ff8f9e` | rose fills/edges |
| `--mut-rose-mid` | `#ff7f96` | lamp mid-tone (muted) |
| `--mut-rose-text` | `#ffc0d0` | rose text/badges/errors on glass |
| `--mut-rose-word` | `#ff9aae` | mic state word, MUTED |

### 1.5 State hue matrix (semantics 🔒)

| State | text | background wash | border |
|---|---|---|---|
| live / unmuted / on / online | `--mut-mint-text` | `rgba(88,240,200,.10)` | `rgba(88,240,200,.42)` |
| muted | `--mut-rose-text` | `rgba(255,135,163,.10)` | `rgba(255,150,190,.48)` |
| error (light card) | `--mut-rose-text` | `rgba(255,135,163,.12)` | `1px solid rgba(255,143,160,.50)` |
| unreachable (mic) | `--mut-rose-text` | `rgba(255,135,163,.08)` | `1px dashed rgba(255,143,160,.55)` |
| off | `--mut-text-3` | `rgba(244,246,255,.04)` — shares unknown's wash (`--unknown-bg`) | `1px solid rgba(244,246,255,.14)` |
| unknown | `--mut-text-3` | `rgba(244,246,255,.04)` | `1px solid rgba(244,246,255,.14)` |
| disconnected | `--mut-text-3` | transparent | `1px dashed rgba(244,246,255,.18)` |

off and unknown deliberately render identically (same text, wash, and hairline — one shared rule): the states are never co-located (off appears on light cards, unknown on the mic), so sameness is unambiguous against §5 rule 3.

### 1.6 Radius 🔒, spacing, typography, motion

| Token | Value | Use |
|---|---|---|
| `--mut-r-6/10/14/20` | `6px / 10px / 14px / 20px` 🔒 | readout chips+tracks 6 · setting rows, inputs, card-error 10 · control blocks, light cards, empty states, error banner 14 · rail cards/panels 20 |
| `--mut-r-pill` | `999px` 🔒 | all buttons, badges, connection pill, sliders' channel caps |
| `--mut-sp-1…7` | `4 / 8 / 12 / 16 / 20 / 24 / 32px` | card padding 20; card gap 16; control gap 12; intra-label gaps 8/4; section gaps 24/32 |
 | Type scale 🔒 | `11 / 12 / 13 / 14 / 16 / 20 / 27px` | 11 eyebrow+footer+hints · 12 meta+secondary lines · 13 body·outputs (700, tabular) · 14 names/card titles (700) · 20 wordmark (750) · 27 mic state word |
| State words 🔒 | uppercase, weight `800`, tracking `-0.01em`, line-height `1.15` | mic state word |
| Eyebrows 🔒 | `11.5px` (.72rem), uppercase, weight `750`, tracking `.13em`, color `--mut-text-3` | section eyebrows |
| Numerals 🔒 | `font-variant-numeric: tabular-nums` on every output/%/K/meta value | outputs, readout chips |
| `--mut-ease` | `ease-out` 🔒 | — |
| `--mut-dur-1/2/3` | `120 / 140 / 160ms` 🔒 | press 120, hover/color 140, state color/bg/border/shadow ≤160 |
| Press 🔒 | `transform: translateY(1px)` on `:active` | all buttons |
| Hover lift 🔒 | `transform: translateY(-1px)` (≤2px) | buttons, setting rows |
| `--mut-breathe` | `2.6s ease-in-out infinite` 🔒 | mic lamp, **muted only** |
| Reduced motion 🔒 | global `@media (prefers-reduced-motion: reduce)` kills transitions/animations; lamp holds static lit values | — |
| Focus ring 🔒 | `outline: 2px solid var(--mut-signal); outline-offset: 3px;` + `box-shadow: 0 0 0 5px var(--mut-signal-halo), <element's base shadow>` | every `:focus-visible`; outlines never removed |

---

## 2. Backdrop field

Exact CSS — one static fixed layer, no animation, no per-frame GPU work:

```css
body {
  background-color: #0c0c18;               /* --mut-field-base */
}
body::before {
  content: "";
  position: fixed;
  inset: 0;
  z-index: -1;
  pointer-events: none;
  background:
    /* vignette on top, concentrates the field */
    radial-gradient(150% 120% at 50% 38%, transparent 55%, rgba(2,3,9,.46) 100%),
    /* violet bloom, upper left — behind the rail */
    radial-gradient(760px 500px at 7% -14%, rgba(120,80,220,.55), rgba(120,80,220,0) 62%),
    /* cyan bloom, upper right — behind the first panels */
    radial-gradient(700px 460px at 105% 3%, rgba(20,150,190,.50), rgba(20,150,190,0) 58%),
    /* magenta bloom, below center — warms the scroll depth */
    radial-gradient(900px 620px at 57% 132%, rgba(190,60,150,.45), rgba(190,60,150,0) 64%);
}
```

Static only. Painted once; the 750 ms poll never touches it. Field hues/opacities are byte-identical to the mockup; only radii were scaled and the vignette added (see §4/e deviations).

---

## 3. Component treatments

`state fade` below means: `transition: background-color 140ms var(--mut-ease), border-color 140ms var(--mut-ease), color 140ms var(--mut-ease), box-shadow 160ms var(--mut-ease)` — so 750 ms poll re-renders cross-fade instead of snapping.

### 3.1 Leaf recipes (referenced by components)

**Buttons** — radius 999, min-height 44px, padding `10px 18px`, font 13px/700, state fade.

| Tier | base | hover | active | disabled |
|---|---|---|---|---|
| **Primary** (Mute, All on, Save) | bg `rgba(244,246,255,.94)`, ink `--mut-ink`, shadow `0 10px 24px rgba(2,4,14,.45)`, `inset 0 1px 0 rgba(255,255,255,.35)` | bg `.97`, `translateY(-1px)`, shadow `0 14px 30px rgba(2,4,14,.50)` | `translateY(1px)` (120ms), shadow `0 4px 12px rgba(2,4,14,.50)` | opacity `.45`, `cursor: not-allowed`, no transform |
| **Secondary glass** (Unmute, All off, Apply, trims) | bg `rgba(244,246,255,.10)`, border `1px solid rgba(244,246,255,.18)`, text `--mut-text` | bg `.14`, border `.32`, `translateY(-1px)` | `translateY(1px)`, bg `.12` | opacity `.45` |
| **Quiet** (Toggle, Refresh, Delete) | borderless, transparent, text `--mut-text-2` | bg `rgba(244,246,255,.07)`, text `--mut-text` | bg `rgba(244,246,255,.10)` | opacity `.40` |
| **Quiet-danger** (Delete) | quiet base | text `--mut-rose-text`, bg `rgba(255,135,163,.08)` | bg `rgba(255,135,163,.12)` | as quiet |

Small variants (setting rows, trim buttons): min-height 36px, padding `7px 12px`, radius 999, min-width 58px for trims. Tinted trims: **Cooler** = secondary with text `--mut-signal`; **Warmer** = secondary with text `--mut-amber`; hovers add the respective hue at `.08` bg.

**Sliders** — channel height 6px, radius 999, full-width. Thumb: 18px circle, `#f7f9ff`, `box-shadow: 0 1px 4px rgba(2,4,14,.55), 0 0 0 1px rgba(255,255,255,.35), 0 0 10px rgba(244,246,255,.25)`; `::-webkit-slider-thumb { margin-top: -6px }`. Hover adds `0 0 0 6px rgba(244,246,255,.10)` to the thumb; active raises halo to `.16` (no translate on sliders). `:focus-visible` = 🔒 ring on the input + thumb halo `0 0 0 5px var(--mut-signal-halo)`. Disabled (light off/disconnected): channel alpha halved, thumb `rgba(244,246,255,.35)` flat, no halo.

- **Brightness track** (0–100, continuous): luminance ramp fill clipped to `--fill-pct` (0–100, set alongside `--glow-bri` in the same feature-detected JS write; group inputs ship static initial values in markup so the fallback is a correct mid-track, never a wrong empty one):
  ```css
  background-color: rgba(255,255,255,.12);
  background-image: linear-gradient(90deg, rgba(240,244,255,.28), #eef2ff);
  background-size: calc(var(--fill-pct, 50) * 1%) 100%;
  background-repeat: no-repeat;
  ```
- **Warmth track** (19 hardware steps, 2900K→7000K): full-range photometric ramp + 19 detents, no fill (the range itself is the information):
  ```css
  background:
    /* detents: 1px dark notches at steps 0–17; track endcap marks step 18 */
    repeating-linear-gradient(90deg, rgba(10,12,24,.30) 0 1px, transparent 1px calc(100% / 18)),
    linear-gradient(90deg, #ffb169 0%, #ffc58c 24%, #ffdcb0 44%, #f4e6c8 58%, #dde3f0 78%, #ccdcff 100%);
  ```
  Warm→cool hint labels in `--mut-text-3` at 11px: `2900K warm` / `7000K cool`.

**Value outputs** (all `%`/`K`/`Unknown`/`Mixed`/port meta): tabular numerals 🔒. Slider outputs render in **readout chips**: inline-flex, radius 6px, consuming `--recess-*` — bg `rgba(10,12,24,.32)`, `inset 0 1px 3px rgba(2,4,14,.32), inset 0 -1px 0 rgba(255,255,255,.05)`, padding `2px 8px`, 13px/700, `--mut-text`; the unit (`%`, `K`) at 11px `--mut-text-2`. `Unknown`/`Mixed` in `--mut-text-3`.

**Badges** — 999 pills, 11px/740 uppercase, tracking .06em, padding `4px 10px`, border 1px; palettes exactly per §1.5 (dashed reserved for unreachable/disconnected). State fade on all. All eight states (`on/off/error/disconnected`, `unmuted[live]/muted/unknown/unreachable`) are drawn from §1.5 — no other badge variant exists.

**Text input** (`#settings-name`): glass-2 recessed (see §1.2), radius 10, min-height 44, padding `10px 14px`, 13px `--mut-text`; placeholder `--mut-text-3`; hover border `.22`; focus per 🔒 ring. Over-42-byte rejection surfaces only in the page error banner (behavior unchanged).

**Focus** — every interactive leaf gets exactly the 🔒 ring; on dark glass the 2px signal ring + 3px offset + 5px halo reads cleanly over any field zone (verified §6.1). `outline: none` never appears.

### 3.2 Topbar

- **Brand chip:** 42×42, radius 10, glass-3 recipe; sun glyph `stroke: var(--mut-signal)` at 80% opacity… final: `color: var(--mut-signal)` (`#7de3ff`), the cyan drop-shadow retained: `filter: drop-shadow(0 0 6px rgba(125,227,255,.35))`; a quiet cyan ember, not a neon sign.
- **Wordmark:** 20px/750 `--mut-text`, tracking -.02em; eyebrow above per §1.6; tagline 13px `--mut-text-2`.
- **Connection pill** (glass-3, 999): dot 8px + text 12px `--mut-text-2`, state fade.
  - `online`: dot `--mut-mint`, `box-shadow: 0 0 8px rgba(88,240,200,.70), 0 0 0 3px rgba(88,240,200,.12)`.
  - `degraded`: dot `--mut-amber`, `0 0 8px rgba(255,201,125,.55), 0 0 0 3px rgba(255,201,125,.12)`.
  - initial `data-state="idle"` (`Connecting`, no ellipsis): dot `rgba(244,246,255,.40)`, ring `rgba(244,246,255,.10)`, no glow — dim-neutral, same treatment in both themes.
- Layout: flex, space-between, static (not sticky); collapses in the 900px density block.

### 3.3 Mic hero card (rail, first)

Glass-1, radius 20. Head row: **lamp → eyebrow block ("Microphone" + wordmark sub) → badge**, then state word, `#mic-line`, then buttons (Mute primary / Unmute secondary / Toggle quiet).

**The lamp** — 56px, `aria-hidden`, pure CSS keyed on `#mic-status[data-state]` (sibling combinator; lamp placed after the badge in DOM, `order: -1` in the head flex). Base construction, all states:

```css
.mic-lamp { width: 56px; height: 56px; border-radius: 50%;
  box-shadow: 0 0 0 1px rgba(255,255,255,.22),           /* rim definition */
              inset 0 2px 2px rgba(255,255,255,.50),      /* top specular edge */
              inset 0 -6px 10px rgba(0,0,0,.28),          /* bottom depth */
              /* + state glow below */ ;
  transition: background 160ms var(--mut-ease), box-shadow 160ms var(--mut-ease), opacity 160ms; }
.mic-lamp::before { /* specular fleck */ width: 12px; height: 12px; border-radius: 50%;
  background: radial-gradient(circle, rgba(255,255,255,.85), rgba(255,255,255,0) 70%);
  position: absolute; top: 15%; left: 20%; }
```

| `data-state` | face | glow | motion |
|---|---|---|---|
| `unmuted` (LIVE) | `radial-gradient(circle at 34% 30%, #d2fff0, #58f0c8 52%, #22956f)` | `0 0 20px rgba(88,240,200,.50), 0 0 48px rgba(88,240,200,.18)` | none — calm is still |
| `muted` | `radial-gradient(circle at 34% 30%, #ffd5df, #ff7f96 52%, #c23e5e)` | breathes (below) | 🔒 only breathing element |
| `unknown` | **unlit glass bead:** `radial-gradient(circle at 34% 30%, rgba(244,246,255,.18), rgba(244,246,255,.05) 60%, rgba(10,12,24,.30))`, own `backdrop-filter: blur(4px)` | `0 0 0 rgba(0,0,0,0)` | none |
| `unreachable` | bead + `::after` ring: `inset: -7px`, `border: 1px dashed rgba(244,246,255,.22)`, radius 50%; lamp `opacity .5` | none | none |

Breathing (muted only, 2.6s ease-in-out infinite 🔒):

```css
@keyframes lamp-breathe {
  0%,100% { transform: scale(1);
    box-shadow: 0 0 0 1px rgba(255,255,255,.22), inset 0 2px 2px rgba(255,255,255,.50), inset 0 -6px 10px rgba(0,0,0,.28),
                0 0 16px rgba(255,127,150,.45), 0 0 40px rgba(255,127,150,.16); }
  50%     { transform: scale(1.045);
    box-shadow: 0 0 0 1px rgba(255,255,255,.30), inset 0 2px 2px rgba(255,255,255,.55), inset 0 -6px 10px rgba(0,0,0,.28),
                0 0 26px rgba(255,127,150,.75), 0 0 56px rgba(255,127,150,.30); }
}
```

Reduced motion: animation removed; the lamp holds its static scarf (`--lamp-muted-fx` rest values) at scale 1.

- **State word** (27px/800 uppercase 🔒): `unmuted` → `--mut-mint-word`, no bloom; `muted` → `--mut-rose-word` + `text-shadow: 0 0 22px rgba(255,127,150,.35)`; `unknown`/`unreachable` → `--mut-text-3`. State fade 160ms cross-fades color.
- **Badge**, `#mic-line`, buttons per leaf recipes and §1.5. Choreography: at `unknown`, Toggle disabled only (Mute/Unmute stay armed — absolute verbs); at `unreachable`, all three disabled. Disabled recipe per §3.1.

### 3.4 Gang / all-lights card (rail, second)

Glass-1; control blocks stack vertically (rail width), each glass-2 radius 14, padding 16, gap 12.

- Power row: **All on** primary / **All off** secondary / **Toggle** quiet; Refresh quiet icon-button in the card head.
- **Match brightness** slider: leaf recipe, luminance ramp, static initial `--fill-pct: 55` in markup; output chip shows `47%` or `--mut-text-3` `Unknown`/`Mixed`.
- **Match warmth** slider: warmth track with detents (no fill), output chip `4950K`; hint labels at track ends.
- Trim rows: `-5 -1 +1 +5` four small secondary pills; **Cooler** / **Warmer** tinted trims per §3.1.
- The group sliders are never disabled (they write absolute values); `Mixed` never derives a fill — fill reflects the last local thumb position only.

### 3.5 Saved-settings card (rail, third)

Glass-1. Save form: recessed input + primary **Save** pill. `#settings-line` 12px `--mut-text-2`; its store-error text state goes `--mut-rose-text` (no extra chrome). Rows: glass-2 chips, radius 10, padding `6px 6px 6px 12px`; name 13px `--mut-text`, ellipsis; **Apply** small secondary, **Delete** small quiet-danger. Row hover: border `.11→.20`, no lift. Empty state: `1px dashed var(--disc-border)` (`rgba(244,246,255,.18)`), radius 14, bg `rgba(244,246,255,.03)`, copy 12px `--mut-text-3`, centered.

### 3.6 Light cards (main column)

Glass-1, radius 14 (denser than rail cards), padding 17px, grid `repeat(auto-fill, minmax(300px, 1fr))`, gap 16. Name 14px/700; meta 12px tabular `--mut-text-3` (`COM7 · connected`); badge per §1.5.

**Power toggle** (`data-on`): secondary pill with power icon; when `data-on="true"`: text `--mut-mint-text`, bg `rgba(88,240,200,.08)`, border `rgba(88,240,200,.45)`, icon `filter: drop-shadow(0 0 5px rgba(88,240,200,.5))`; when false/disconnected: neutral, disconnected also disabled.

**Per-card aura — the light spill** (driven by `--glow-bri` 0–100, set per card feature-detected 🔒; hue steered by `--glow-t` 0–100 = tempIndex/18×100, set in the same gated write):

```css
.light-card {
  --bri01: calc(var(--glow-bri, 0) / 100);
  --aura-c: #ffc089; /* pre-color-mix fallback */
  --aura-c: color-mix(in oklab, #ffb066, #ccdcff calc(var(--glow-t, 56) * 1%)); /* oklab: no green midpoints */
  border-color: color-mix(in oklab, rgba(255,255,255,.155), var(--aura-c) calc(45% * var(--bri01)));
  box-shadow:
    0 16px 34px rgba(2,4,14,.50),                                   /* constant depth, not glow */
    inset 0 1px 0 rgba(255,255,255,.15),                            /* constant top hairline */
    /* L1 halo-far — ambient spill onto the field */
    0 6px calc(24px + 34px * var(--bri01)) color-mix(in oklab, var(--aura-c) calc(30% * var(--bri01)), transparent),
    /* L2 halo-near — edge bloom hugging the card */
    0 0 calc(8px + 14px * var(--bri01)) color-mix(in oklab, var(--aura-c) calc(40% * var(--bri01)), transparent),
    /* L3 uplight — spill rising from the card's own face */
    inset 0 -20px 34px -20px color-mix(in oklab, var(--aura-c) calc(20% * var(--bri01)), transparent);
}
```

Baked values (per layer: blur / alpha; warm = 2900K `#ffb066`, cool = 7000K `#ccdcff`; border mix % toward aura):

| `--glow-bri` | L1 halo-far | L2 halo-near | L3 uplight α | border mix |
|---|---|---|---|---|
| 0 (off/error/disconnected) | none (α 0) | none (α 0) | 0 | 0% — plain glass line |
| 25 | 33px / .075 | 12px / .10 | .05 | 11% |
| 50 | 41px / .15 | 15px / .20 | .10 | 22% |
| 75 | 50px / .23 | 19px / .30 | .15 | 34% |
| 100 | 58px / .30 | 22px / .40 | .20 | 45% |

Aura color at the two extremes is exactly `#ffb066`→`#ccdcff`; at 4950K the oklab mix passes ~`#e9e4da` (near-neutral white spill) — physical, never green. Off/error/disconnected carry no glow by construction; the card-error banner always outranks the aura (rose sits *inside* the card, aura outside).

**Sliders** per §3.1; card brightness slider gets `--fill-pct` from the same updateCard write; warmth slider detented, no fill. Disabled slider treatment when the light is off or disconnected (locked behavior).

**Card error banner** (`role="status"`): radius 10, bg `rgba(255,135,163,.08)`, border `1px solid rgba(255,143,160,.45)`, text `--mut-rose-text` 12px, alert icon same ink; margin-top 14.

### 3.7 Page error banner (`role="alert"`)

Inside the gang card: radius 14, `background: linear-gradient(180deg, rgba(255,135,163,.12), rgba(255,135,163,.07))` over the card's glass, border `1px solid rgba(255,143,160,.50)`, text 13px `--mut-rose-text`, alert icon same, **Retry** small secondary (neutral, not rose-tinted — it is a verb, not part of the error state). Appearance/disappearance cross-fades 160ms (no layout-shifting animation).

### 3.8 Empty-panels state

Full-main-column dashed variant of §3.5's empty recipe at radius 14, padding `30px 20px`: `<strong>` 13px `--mut-text-2`, `<span>` 12px `--mut-text-3`.

### 3.9 Footer

11px, `--mut-text-3`, centered, tracking .02em, margin-top 24; text unchanged (`Loopback only · … · status refreshes every 750 ms`).

---

## 4. Signature details

1. **A physical tally lamp, not a dot.** The mic lamp is built like hardware — specular fleck, top edge highlight, weighted base shadow, rim ring — and its 2.6s breath drives *two* glow radii plus a 4.5% scale swell, so muted reads as "emergency broadcast light," while LIVE is deliberately still: calm is quiet, attention breathes.
2. **The unlit glass bead.** At `unknown`/`unreachable` the lamp is the *only* place the raw color field shows through inside a control (2px-radius blur, no glow): the UI says "no data" by showing you the room with the light off — then the bead ignites to mint or rose in a single 160ms cross-fade when state resolves. Unreachable adds one dashed orbital ring, never a new color.
3. **The warmth track is a photometer.** A measured-feeling 2900K→7000K ramp (`#ffb169 → #ccdcff`, six stops, lightness rising warm→cool like tungsten→skylight) with 19 one-pixel detents — one per hardware step — so users *see* the device's discreteness instead of an implied continuum.
4. **Oklab light spill.** Per-card aura hue is `color-mix(in oklab, #ffb066, #ccdcff, --glow-t)` — a straight line in perceptual space, so 4950K spills neutral white, never green — and the three named layers (halo-far / halo-near / uplight) scale linearly with `--glow-bri`, alpha-capped at .40: lit panels warm the field beside them like real panels warming a desk.
5. **Blur discipline.** Only the ~5–8 elevation-1 and -3 surfaces carry `backdrop-filter`; every nested surface is tint-only with a 1px hairline. Edges stay razor sharp, the 750ms re-render stays cheap, and the glass reads as polished panes — not frost smearing into frost.

## 5. Anti-cliché guardrails

1. **Never ghost-caption.** No text below `rgba(244,246,255,.68)` alpha may carry information (4.6:1 worst-case, computed §6.1); lower opacities exist only on disabled controls. "Airy" is achieved with size and tracking, never with faintness.
2. **Never blur the inside of glass.** Nested frosted-on-frosted surfaces are forbidden; inner groups are flat tints with hairline borders. If a new surface is proposed with its own blur, it gets rejected or promoted to elevation-1.
3. **Never state-by-color-alone.** Every state pairs hue with a second channel: label text, border style (dashed = unreachable/disconnected, solid = definitive), or motion (breathing = muted only). Two same-hue badges never differ only in lightness.
4. **Never two buttons of the same construction side by side.** Tiers differ by *mass*, not just color: solid light pill / bordered glass / borderless. Within any button row, at most one primary, and quiet buttons never sit adjacent to each other.
5. **Cap the glow.** No glow alpha above .45 anywhere; no glow radius over 58px; no glow darker than L≈68% (no "lit neon tubes"); glow on *text* only on the 27px mic state word, ≤ .35 bloom. The backdrop field carries color; components carry information.
6. **Motion budget is fixed.** Allowed: state fades 120–160ms, press 1px, hover lift 1px, the muted lamp's breath. Forbidden: animated gradients/backdrops, blur-radius animation, cursor-following effects, entrance choreography beyond the lamp's ignite.

## 6. Verification checklist

### 6.1 Computed contrast pairs (WCAG 2.x formula, computed exactly)

Worst-case glass composite — glass-1 over the brightest field point (violet bloom center, α .55 over `#0c0c18`, before the vignette which only darkens): field `(71,49,132)`; + glass `.075` white ⇒ bg `(85,64,141)`, L = 0.0763. All ratios below use this worst case unless noted.

| # | Foreground | Background | Ratio | Target | Pass |
|---|---|---|---|---|---|
| 1 | `--mut-text` `#f4f6ff` | glass-1 worst | **7.7:1** | ≥4.5 | ✓ |
| 2 | `--mut-text` `#f4f6ff` | darkest zone composite `(30,30,41)` | **15.6:1** | ≥4.5 | ✓ |
| 3 | `--mut-text-2` `.82` | glass-1 worst | **5.8:1** | ≥4.5 | ✓ |
| 4 | `--mut-text-3` `.68` (hints/meta/eyebrow/footer) | glass-1 worst | **4.6:1** | ≥4.5 | ✓ |
| 5 | `--mut-text-disabled` `.40` | glass-1 worst | 2.9:1 | exempt (disabled) | n/a |
| 6 | `--mut-ink` `#191b2b` | primary pill `.94` white | **16.5:1** | ≥4.5 | ✓ |
| 7 | secondary button text `#f4f6ff` | secondary bg `.12` over glass-1 worst | **5.7:1** | ≥4.5 | ✓ |
| 8 | `--mut-mint-text` `#8df5d9` | glass-1 worst | **6.4:1** | ≥4.5 | ✓ |
| 9 | `--mut-mint-text` on mint badge wash `.10` | composite | **6.3:1** | ≥4.5 | ✓ |
| 10 | `--mut-rose-text` `#ffc0d0` | glass-1 worst | **5.4:1** | ≥4.5 | ✓ |
| 11 | `--mut-rose-text` on rose wash `.10` (muted badge) | composite | **4.8:1** | ≥4.5 | ✓ |
| 12 | `--mut-rose-text` on banner wash (≈.095 avg) | composite | **4.9:1** | ≥4.5 | ✓ |
| 13 | `--mut-signal` `#7de3ff` (Cooler trim, focus ring vs adjacent) | glass-1 worst | **5.7:1** | ≥3 (non-text ≥3) | ✓ |
| 14 | `--mut-amber` `#ffc97d` (Warmer trim) | glass-1 worst | **5.5:1** | ≥4.5 | ✓ |
| 15 | mic state word LIVE `#7df4d6` (27px/800) | glass-1 worst | **6.2:1** | ≥3 (large) | ✓ |
| 16 | mic state word MUTED `#ff9aae` (27px/800) | glass-1 worst | **4.2:1** | ≥3 (large) | ✓ |
| 17 | readout chip text `#f4f6ff` | chip recess `(59,46,100)` | **11.1:1** | ≥4.5 | ✓ |
| 18 | focus ring `#7de3ff` + `.20` halo | any field zone (min over darkest `(30,30,41)`) | **5.7+ :1** ring | ≥3 (non-text) | ✓ |

### 6.2 State matrix — sign off in both viewports (1440px and 650px)

**Global chrome**
- [ ] Backdrop: three blooms + vignette resolved, static, no repaint on scroll (paint profiler)
- [ ] Topbar: chip/wordmark/pill alignment at 1440; stacked at 650
- [ ] Connection pill: online (mint halo) · degraded (amber halo) · initial Connecting (dim)
- [ ] Focus ring visible on every interactive leaf via keyboard walk (tab order = DOM order)

**Mic hero** (for each of `unmuted` / `muted` / `unknown` / `unreachable`)
- [ ] Badge + state word + lamp + `#mic-line` all agree; cross-fade ≤160ms between states
- [ ] `muted`: only element breathing on the page; breath period 2.6s; `prefers-reduced-motion` → static lit
- [ ] `unknown`: Toggle disabled, Mute/Unmute armed; bead renders field through it
- [ ] `unreachable`: all three buttons disabled; dashed orbital ring; opacity .5

**Gang card**
- [ ] Outputs: definite `47%`/`4950K` · `Mixed` · `Unknown` each in correct chip tone
- [ ] Brightness fill tracks thumb; warmth track shows full ramp + 19 detents, no fill
- [ ] Trim rows: Cooler cyan-tint, Warmer amber-tint; disabled-less behavior intact

**Light cards** (on / off / error / disconnected, at bri 25 & 100, temps 2900K & 7000K)
- [ ] Aura layers scale per table (§3.6); 2900K warm spill `#ffb066`; 7000K ice `#ccdcff`; 4950K neutral
- [ ] off/error/disconnected: zero aura; card error banner outranks everything
- [ ] Power button lit-edge when `data-on="true"`; sliders disabled treatment when off/disconnected

**Saved settings** — empty state · 1 row · 6+ rows · store-error line (rose) · name-too-long rejection in page banner
**Page error banner** — shown/hidden cross-fade; Retry secondary neutral
**Empty panels** — dashed empty state in main column
**Widths** — 1440: rail sticky, panels auto-fill ≥2 cols · 650: single column, order topbar → mic → gang → settings → panels → footer, rail unsticks
**Motion** — `prefers-reduced-motion`: no breath, no lifts, instant state swaps; normal: nothing animates except the muted lamp
