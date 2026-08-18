# Glassline UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restyle `internal/lightui/index.html` to the approved Glassline dual-theme design (command-deck layout, dark+light adaptive) per spec rev 2, with a stub-server verification runbook over the full state matrix.

**Architecture:** Single-file restyle. Three seams: (1) replace the `<style>` block with the consolidated Glassline stylesheet (dark default at `:root`, light overrides in `@media (prefers-color-scheme: light)`); (2) restructure the body into topbar / rail / main-flow / footer command-deck markup, adding the mic lamp + state word; (3) additive JS hooks (`applyGlow`, `--fill-pct` writers, mic commit buffer) spliced into existing functions. Behavior invariants (spec rev2 §4) stay byte-for-byte.

**Tech Stack:** vanilla HTML/CSS/JS (no build tooling), Go embed (`go build ./...` is the compile gate), python3 stdlib stub server for browser verification, superpowers-chrome tool for screenshots.

## Global Constraints

(Every task implicitly includes these. Values copied verbatim from spec rev 2.)

- Target file: `/home/dan/code/mutastic/.worktrees/ui-redesign-glassline/internal/lightui/index.html`. All work happens in this worktree, branch `ui-redesign-glassline`.
- Light targets: `docs/superpowers/specs/design-light.md`; dark: `docs/superpowers/specs/design-dark.md` — committed in this worktree; they are the visual source of truth. Where this plan's stylesheet assigns an exact value, the plan value wins (canonicalization section below).
- **Never modify** `internal/lightui/mutation_queue.js`, endpoints, or payload shapes.
- Keep exactly: the 750 ms combined poll (`window.setInterval(() => { refreshLights(true); refreshMic(); refreshSettings(); }, 750);`), the `<script src="/mutation_queue.js">` tag, exactly **one** attribute-less inline `<script>` block, function names (`bindCards`, `bindMicControls`, `bindTopControls`, `bindSettingsControls`, `renderCards`, `updateCard`, `updateMic`, `micLineText`, `settingsNameOverByteCap`, `flushPendingSliders`, `scheduleSlider`, `showError`, `clearError`, `showApplyDetail`, `renderSettings`, `refreshLights`, `refreshMic`, `refreshSettings`, `enqueueMutation`, `errorMessage`, `setConnection`), helpers (`icon`, `escapeHTML`, `temperatureIndex`, `lightKey`, `lightIsOn`, `cardMarkup`), the `TEMP_STEPS` array, `settingsNameOverByteCap` gating with `SETTINGS_NAME_TOO_LONG` at its two call sites, `flushPendingSliders()` called before every settings mutation enqueue (3 call sites), the mic button `data-mic-action` set with Toggle `disabled` in markup, `role="alert"` on the error banner and `role="status"` on the connection pill + card-error strips, the `.status-badge[data-state]` state set (`unmuted/muted/unknown/unreachable` on mic; `on/off/error/disconnected` on cards), group outputs showing `Unknown`/`Mixed`, `#mic-line` texts, every `id` currently in the DOM.
- Build gate after every task: `go build ./...` from the worktree root must pass.
- Motion: all transitions 120–160 ms ease-out; press `translateY(1px)`; hover lift `translateY(-1px)`; lamp breathing 2.6 s only while muted; global `@media (prefers-reduced-motion: reduce)` kills all transitions/animations (lamp holds static lit values).
- Focus: `:focus-visible` → `outline: 2px solid var(--signal); outline-offset: 3px;` plus a 5px halo `var(--ring-halo)` appended after the element's resting shadow (never replacing it). Never `outline: none`.
- Focus recipe attaches via CSS only (`:focus-visible`) — no JS focus handling changes.

## Canonical deltas (documented, intentional — spec rev2 §2 allows these)

1. **Glow interface is unified:** JS sets two custom properties per light card — `--glow-bri` (0–100, brightness; 0 when off/error/disconnected) and `--glow-hue` (0–18, temperature index). Dark CSS derives `--bri01` + `--aura-c` (oklab color-mix via `--glow-t: calc(var(--glow-hue) / 18 * 100)`); Light CSS derives `--aura-h: calc(38 + var(--glow-hue) * 9.11)` (hsl cast/rim per design-light §3.8). This supersedes the two divergent property names in the design files; table values preserved.
2. **Slider fill interface:** brightness tracks (card + group) carry `--fill-pct` (0–100) on the `<input>` element, set in the same gated writes; group markup ships static initial `--fill-pct: 55` so a pre-JS frame renders a correct mid-track.
3. **Mic commit buffer:** `micCommittedAt` (ms) set on every mic-button click; `updateMic` ignores polls arriving < 175 ms after a press (prevents flip-flop between press and daemon ack). This delays lamp/badge/line updates only — button arming behavior unchanged.
4. **Mic lamp is pure CSS**, sibling-keyed (`#mic-status[data-state="X"] ~ .mic-lamp`), placed after the badge in DOM with `order: -1` in the flex head; new element `<span class="mic-lamp" aria-hidden="true"></span>` — no JS.
5. **Mic state word:** new `<span id="mic-word" class="mic-word" data-state="unknown">unknown</span>` (27 px/800 uppercase, color per state, `text-transform` handles case so text stays lowercase in DOM/JS). `updateMic` sets `word.dataset.state` + `word.textContent = micState` (guarded by existence).
6. **Palette notes:** light-theme lamp bead `unknown` face = `#f7f8fd / #a7accb / #7a80a3` per design-light §3.2 (deep rim, not washed-out glass); dark lamp bead keeps its translucent-glass look per design-dark §3.3.
7. **Band-aid queue:** if banding shows on the dark field during Task 5 verification, add `body::after` SVG-noise overlay at 1.5% opacity (code provided in Task 5).

---

## Task 1: Verification rig (stub server)

**Files:**
- Create: `/tmp/opencode/verify/serve.py` (session tooling — deliberately outside the repo, never committed)

**Interfaces:**
- Produces: `http://127.0.0.1:8788/` serving the worktree page; `http://127.0.0.1:8788/api/{lights,mic,settings,light,group}`; `POST http://127.0.0.1:8788/__profile__` switches state profile; mutation POSTs append one line each to `/tmp/opencode/verify/mutations.log`.
- Response shapes mirror `ui.go`: light record `{port, name, connected, state, brightness, temp, error}` (brightness/temp `null` unless on+connected); lights reply `{lights: [...]}`; mic `{state}`; settings `{names: [...], detail?}` (POST replies always include `names`).

- [ ] **Step 1: Write the stub server**

```python
#!/usr/bin/env python3
"""Glassline UI verification stub — serves the real page with canned daemon state."""
import json, pathlib, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer

ROOT = pathlib.Path("/home/dan/code/mutastic/.worktrees/ui-redesign-glassline/internal/lightui")
LOG = pathlib.Path("/tmp/opencode/verify/mutations.log")
PORT = 8788

def L(port, name, connected, state, bri, temp, err=""):
    return {"port": port, "name": name, "connected": connected, "state": state,
            "brightness": bri if (connected and state == "on") else None,
            "temp": temp if (connected and state == "on") else None, "error": err}

PROFILES = {
    "default":  {"lights": [L("COM5", "Desk Key", True, "on", 55, 4950),
                            L("COM7", "Sofa Fill", True, "on", 80, 2900),
                            L("COM9", "Spare", True, "off", None, None)],
                 "mic": "unmuted", "names": ["Studio", "Night"]},
    "muted":    {"mic": "muted"},
    "mixed":    {"lights": [L("COM5", "Desk Key", True, "on", 30, 6500),
                            L("COM7", "Sofa Fill", True, "on", 100, 2900)],
                 "mic": "unmuted", "names": ["Studio"]},
    "errcard":  {"lights": [L("COM5", "Desk Key", True, "on", 55, 4950),
                            L("COM7", "Sofa Fill", True, "on", 90, 4950, "last set-temp rejected by panel")],
                 "mic": "unmuted", "names": []},
    "empty":    {"lights": [], "mic": "unmuted", "names": []},
    "mic_unknown":      {"mic": "unknown"},
    "mic_unreachable":  {"mic": "__http503__"},
    "degraded": {"lights": "__http500__"},
    "settings_store_error": {"names": "__get500__"},
}
BASE = {k: v for k, v in PROFILES["default"].items()}
CURRENT = dict(BASE)

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):  # quiet
        pass
    def _send(self, code, payload, ctype="application/json"):
        body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def _read(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}")
    def _restart_log(self, body):
        with LOG.open("a") as f:
            f.write(f"{time.time():.3f} {self.path} {json.dumps(body, sort_keys=True)}\n")
    def do_GET(self):
        p = self.path.split("?")[0]
        if p == "/" or p == "/index.html":
            html = (ROOT / "index.html").read_text()
            self._send(200, html.encode(), "text/html; charset=utf-8"); return
        if p == "/mutation_queue.js":
            self._send(200, (ROOT / "mutation_queue.js").read_bytes(), "text/javascript"); return
        if p == "/api/lights":
            if CURRENT.get("lights") == "__http500__":
                self._send(500, {"error": "simulated daemon failure"}); return
            self._send(200, {"lights": CURRENT["lights"]}); return
        if p == "/api/mic":
            if CURRENT.get("mic") == "__http503__":
                self._send(503, {"error": "mic unreachable"}); return
            self._send(200, {"state": CURRENT.get("mic", "unmuted")}); return
        if p == "/api/settings":
            if CURRENT.get("names") == "__get500__":
                self._send(500, {"error": "settings store unavailable"}); return
            self._send(200, {"names": CURRENT.get("names", [])}); return
        self._send(404, {"error": "not found"})
    def do_POST(self):
        p = self.path.split("?")[0]
        body = self._read()
        if p == "/__profile__":
            name = body.get("profile", "default")
            CURRENT.clear(); CURRENT.update(BASE); CURRENT.update(PROFILES.get(name, {}))
            self._send(200, {"profile": name}); return
        if p == "/api/mic":
            self._restart_log(body)
            action = body.get("action")
            cur = CURRENT.get("mic", "unmuted")
            if action == "mute": cur = "muted"
            elif action == "unmute": cur = "unmuted"
            elif action == "toggle": cur = "muted" if cur == "unmuted" else "unmuted"
            CURRENT["mic"] = cur
            self._send(200, {"state": cur}); return
        if p in ("/api/light", "/api/group"):
            self._restart_log(body)
            # Return a source-of-truth list; light application fidelity is the daemon's concern.
            self._send(200, {"lights": CURRENT.get("lights", [])}); return
        if p == "/api/settings":
            self._restart_log(body)
            names = [n for n in CURRENT.get("names", [])]
            act, name = body.get("action"), body.get("name", "")
            if act == "save" and name not in names: names.append(name)
            if act == "delete" and name in names: names.remove(name)
            CURRENT["names"] = names
            out = {"names": names}
            if act == "apply":
                out["detail"] = ""
            self._send(200, out); return
        self._send(404, {"error": "not found"})

if __name__ == "__main__":
    LOG.write_text("")
    print(f"stub on http://127.0.0.1:{PORT} — POST /__profile__ {{\"profile\": NAME}}", flush=True)
    HTTPServer(("127.0.0.1", PORT), H).serve_forever()
```

- [ ] **Step 2: Run it and smoke-check**

```bash
mkdir -p /tmp/opencode/verify
nohup python3 /tmp/opencode/verify/serve.py >/tmp/opencode/verify/serve.out 2>&1 &
sleep 1
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8788/               # expect 200
curl -s http://127.0.0.1:8788/api/lights                                     # expect {"lights":[...
curl -s http://127.0.0.1:8788/api/mic                                        # expect {"state":"unmuted"}
curl -s http://127.0.0.1:8788/api/settings                                   # expect {"names":["Studio","Night"]}
curl -s -X POST http://127.0.0.1:8788/__profile__ -d '{"profile":"muted"}'
curl -s http://127.0.0.1:8788/api/mic                                        # expect {"state":"muted"}
curl -s -X POST http://127.0.0.1:8788/api/settings -H 'Content-Type: application/json' -d '{"action":"save","name":"Desk"}'
tail -1 /tmp/opencode/verify/mutations.log                                   # expect the save line
curl -s -X POST http://127.0.0.1:8788/__profile__ -d '{"profile":"default"}' # restore
```

- [ ] **Step 3: Commit** — nothing to commit (rig outside repo by design; note in final summary).

---

## Task 2: Stylesheet — part 1 (tokens, backdrop, glass tiers, base elements)

**Files:**
- Modify: `internal/lightui/index.html` — replace the entire `<style> ... </style>` block (currently lines 5–157) with the consolidated stylesheet of Tasks 2–4. The stylesheet is shown in three code blocks (one per task: tokens/base → dark components → light overrides + interactions); the deliverable is their concatenation inside one `<style>` element.

**Interfaces:**
- Produces (tokens used by later blocks): `--field-base --f1 --f2 --f3 --f4 --g1-bg --g1-sheen --g1-border --g1-blur --g1-shadow --g1-inner --g2-bg --g2-border --g2-blur --g2-shadow --g2-inner --g3-bg --g3-border --g3-blur --g3-shadow --g3-inner --recess-bg --recess-border --recess-inner --text --text-2 --text-3 --text-disabled --pbtn-ink --signal --ring-halo --live-text --live-bg --live-border --live-word --muted-text --muted-bg --muted-border --muted-word --muted-word-fx --unreach-border --dim-word --off-border --unknown-bg --disc-border --err-bg --err-border --err-text --carderr-bg --carderr-border --hint-warm --hint-cool --b1-bg --b1-bg-hover --b1-ink --b1-shadow --b1-shadow-hover --b1-shadow-active --b2-bg --b2-border --b2-text --b2-bg-hover --b2-border-hover --b2-shadow --quiet-text --quiet-bg-hover --quiet-bg-active --qd-text --qd-bg-hover --qd-bg-active --dot-online --dot-online-fx --dot-degraded --dot-degraded-fx --dot-idle --dot-idle-fx --lamp-base-fx --lamp-live-face --lamp-live-fx --lamp-muted-face --lamp-muted-fx --lamp-muted-ring --lamp-unreach-ring --lamp-unknown-face --lamp-unknown-fx --lamp-unknown-blur --aura-warm --aura-cool --track-warm-end --track-cool-end --thumb-bg --thumb-fx --sel --scrollbar --t-fast --t-state --r-6 --r-10 --r-14 --r-20`.
- Consumes: nothing (first block).

- [ ] **Step 1: Write the tokens + base + backdrop block**

Replace from `<style>` through the end of the old `:root`/body/html and backdrop rules with (this block is complete and final):

```css
    :root {
      color-scheme: dark light;
      --font: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
      --t-fast: 120ms ease-out;
      --t-state: 140ms ease-out;
      --r-6: 6px; --r-10: 10px; --r-14: 14px; --r-20: 20px;
      /* ==== DARK THEME (default) ==== */
      --field-base: #0c0c18;
      --f1: radial-gradient(150% 120% at 50% 38%, transparent 55%, rgba(2,3,9,.46) 100%);
      --f2: radial-gradient(760px 500px at 7% -14%, rgba(120,80,220,.55), rgba(120,80,220,0) 62%);
      --f3: radial-gradient(700px 460px at 105% 3%, rgba(20,150,190,.50), rgba(20,150,190,0) 58%);
      --f4: radial-gradient(900px 620px at 57% 132%, rgba(190,60,150,.45), rgba(190,60,150,.45) 0%, rgba(190,60,150,0) 64%);
      --g1-sheen: linear-gradient(180deg, rgba(255,255,255,.045), rgba(255,255,255,0) 30%);
      --g1-bg: rgba(255,255,255,.075);
      --g1-border: rgba(255,255,255,.155);
      --g1-blur: blur(20px) saturate(1.35);
      --g1-shadow: 0 24px 48px rgba(2,4,14,.50);
      --g1-inner: inset 0 1px 0 rgba(255,255,255,.15);
      --g2-bg: rgba(255,255,255,.05);
      --g2-border: rgba(255,255,255,.11);
      --g2-blur: none;
      --g2-shadow: none;
      --g2-inner: inset 0 1px 0 rgba(255,255,255,.07);
      --g3-bg: rgba(255,255,255,.11);
      --g3-border: rgba(255,255,255,.20);
      --g3-blur: blur(12px) saturate(1.3);
      --g3-shadow: 0 8px 20px rgba(2,4,14,.40);
      --g3-inner: inset 0 1px 0 rgba(255,255,255,.20);
      --recess-bg: rgba(10,12,24,.32);
      --recess-border: rgba(255,255,255,.10);
      --recess-inner: inset 0 1px 3px rgba(2,4,14,.32), inset 0 -1px 0 rgba(255,255,255,.05);
      --text: #f4f6ff; --text-2: rgba(244,246,255,.82); --text-3: rgba(244,246,255,.68);
      --text-disabled: rgba(244,246,255,.40); --pbtn-ink: #191b2b;
      --signal: #7de3ff; --ring-halo: rgba(125,227,255,.20);
      --live-text: #8df5d9; --live-bg: rgba(88,240,200,.10);  --live-border: rgba(88,240,200,.42);
      --live-word: #7df4d6;
      --muted-text: #ffc0d0; --muted-bg: rgba(255,135,163,.10); --muted-border: rgba(255,150,190,.48);
      --muted-word: #ff9aae; --muted-word-fx: 0 0 22px rgba(255,127,150,.35);
      --unreach-border: rgba(255,143,160,.55);
      --dim-word: rgba(244,246,255,.68); --off-border: rgba(244,246,255,.14);
      --unknown-bg: rgba(244,246,255,.04); --disc-border: rgba(244,246,255,.18);
      --err-bg: linear-gradient(180deg, rgba(255,135,163,.12), rgba(255,135,163,.07));
      --err-border: rgba(255,143,160,.50); --err-text: #ffc0d0;
      --carderr-bg: rgba(255,135,163,.08); --carderr-border: rgba(255,143,160,.45);
      --hint-warm: #ffc97d; --hint-cool: #7de3ff;
      --b1-bg: rgba(244,246,255,.94); --b1-bg-hover: rgba(244,246,255,.97);
      --b1-shadow: 0 10px 24px rgba(2,4,14,.45), inset 0 1px 0 rgba(255,255,255,.35);
      --b1-shadow-hover: 0 14px 30px rgba(2,4,14,.50), inset 0 1px 0 rgba(255,255,255,.35);
      --b1-shadow-active: 0 4px 12px rgba(2,4,14,.50), inset 0 1px 0 rgba(255,255,255,.35);
      --b2-bg: rgba(244,246,255,.10); --b2-border: rgba(244,246,255,.18); --b2-text: var(--text);
      --b2-bg-hover: rgba(244,246,255,.14); --b2-border-hover: rgba(244,246,255,.32);
      --b2-shadow: none;
      --quiet-text: var(--text-2); --quiet-bg-hover: rgba(244,246,255,.07); --quiet-bg-active: rgba(244,246,255,.10);
      --qd-text: #ffc0d0; --qd-bg-hover: rgba(255,135,163,.08); --qd-bg-active: rgba(255,135,163,.12);
      --dot-online: #58f0c8;  --dot-online-fx: 0 0 8px rgba(88,240,200,.70), 0 0 0 3px rgba(88,240,200,.12);
      --dot-degraded: #ffc97d; --dot-degraded-fx: 0 0 8px rgba(255,201,125,.55), 0 0 0 3px rgba(255,201,125,.12);
      --dot-idle: rgba(244,246,255,.40); --dot-idle-fx: 0 0 0 3px rgba(244,246,255,.10);
      --lamp-base-fx: 0 0 0 1px rgba(255,255,255,.22), inset 0 2px 2px rgba(255,255,255,.50), inset 0 -6px 10px rgba(0,0,0,.28);
      --lamp-live-face: radial-gradient(circle at 34% 30%, #d2fff0, #58f0c8 52%, #22956f);
      --lamp-live-fx: 0 0 20px rgba(88,240,200,.50), 0 0 48px rgba(88,240,200,.18);
      --lamp-muted-face: radial-gradient(circle at 34% 30%, #ffd5df, #ff7f96 52%, #c23e5e);
      --lamp-muted-fx: 0 0 20px rgba(255,127,150,.58), 0 0 48px rgba(255,127,150,.22);
      --lamp-muted-ring: rgba(255,143,160,.35);
      --lamp-unreach-ring: rgba(244,246,255,.22);
      --lamp-unknown-face: radial-gradient(circle at 34% 30%, rgba(244,246,255,.18), rgba(244,246,255,.05) 60%, rgba(10,12,24,.30));
      --lamp-unknown-fx: none; --lamp-unknown-blur: blur(4px);
      --aura-warm: #ffb066; --aura-cool: #ccdcff;
      --track-warm-end: #ffb169; --track-cool-end: #ccdcff;
      --thumb-bg: #f7f9ff;
      --thumb-fx: 0 1px 4px rgba(2,4,14,.55), 0 0 0 1px rgba(255,255,255,.35), 0 0 10px rgba(244,246,255,.25);
      --sel: rgba(125,227,255,.32); --scrollbar: rgba(255,255,255,.16);
    }
    * { box-sizing: border-box; }
    [hidden] { display: none !important; }
    html, body { margin: 0; padding: 0; }
    body {
      font-family: var(--font); min-height: 100vh; background: var(--field-base); color: var(--text);
      -webkit-font-smoothing: antialiased; text-rendering: optimizeLegibility;
    }
    body::before {
      content: ""; position: fixed; inset: 0; z-index: -1; pointer-events: none;
      background: var(--f1), var(--f2), var(--f3), var(--f4);
    }
    ::selection { background: var(--sel); }
    html { scrollbar-color: var(--scrollbar) transparent; }
    .sprite { position: absolute; width: 0; height: 0; overflow: hidden; }
    .icon { width: 18px; height: 18px; fill: none; stroke: currentColor; stroke-width: 1.8; stroke-linecap: round; stroke-linejoin: round; flex: none; }
    h1, h2, h3 { margin: 0; }
    p { margin: 0; }
    .eyebrow { font-size: .72rem; font-weight: 750; letter-spacing: .13em; text-transform: uppercase; color: var(--text-3); margin: 0 0 4px; }
    .subtle { color: var(--text-2); font-size: 13px; line-height: 1.45; }
    output, .card-meta, .mic-word, .status-badge { font-variant-numeric: tabular-nums; }
```

- [ ] **Step 2: Compile-check + screenshot the raw foundation**

```bash
go build ./...
curl -s -X POST http://127.0.0.1:8788/__profile__ -d '{"profile":"default"}'
```

Open `http://127.0.0.1:8788/` via the browser tool at viewport 1440×900, screenshot, confirm: field blooms + vignette visible, page unstyled-but-legible (old component CSS is gone — expected at this stage), zero console errors (`get_console_messages`).

- [ ] **Step 3: Commit**

```bash
git add internal/lightui/index.html
git commit -m "feat(ui): Glassline token foundation, backdrop field, base elements"
```

---

## Task 3: Stylesheet — part 2 (dark component skins)

**Files:**
- Modify: `internal/lightui/index.html` — append this block to the stylesheet started in Task 2 (before the closing `</style>`).

**Interfaces:**
- Consumes: tokens from Task 2.
- Produces selectors later blocks rely on: `.shell .layout .topbar .brand .brand-mark .connection .rail .panel .mic-lamp .mic-word .status-badge[data-state] .power-row .control-block .control-label .range-wrap input[type=range] .range-hints .trim-row .button-primary .button-quiet .button-icon .error-banner .settings-* .empty .section-title .lights-grid .light-card .card-* .footer` (all dark-themed).

- [ ] **Step 1: Append the dark component block (complete)**

```css
    /* ===== Layout shell ===== */
    .shell { min-height: 100vh; display: flex; flex-direction: column; }
    .topbar {
      display: flex; align-items: center; gap: 14px;
      padding: 18px 28px 14px; flex: none;
    }
    .brand { display: flex; align-items: center; gap: 12px; min-width: 0; }
    .brand-mark {
      width: 42px; height: 42px; border-radius: 10px; display: grid; place-items: center;
      background: var(--g1-sheen), var(--g3-bg); border: 1px solid var(--g3-border);
      box-shadow: var(--g3-shadow), var(--g3-inner);
      color: var(--signal); filter: drop-shadow(0 0 6px rgba(125,227,255,.35));
    }
    .brand h1 { font-size: 20px; font-weight: 750; letter-spacing: -.02em; color: var(--text); }
    .brand .subtle { font-size: 13px; }
    .connection {
      margin-left: auto; display: inline-flex; align-items: center; gap: 8px;
      padding: 8px 12px; border-radius: 999px; font-size: 12px; color: var(--text-2);
      background: var(--g1-sheen), var(--g3-bg); border: 1px solid var(--g3-border);
      box-shadow: var(--g3-shadow), var(--g3-inner);
      -webkit-backdrop-filter: var(--g3-blur); backdrop-filter: var(--g3-blur);
      transition: color var(--t-state);
    }
    .connection-dot { width: 8px; height: 8px; border-radius: 50%; background: var(--dot-idle); box-shadow: var(--dot-idle-fx); transition: background var(--t-state), box-shadow var(--t-state); }
    .connection[data-state="online"] .connection-dot { background: var(--dot-online); box-shadow: var(--dot-online-fx); }
    .connection[data-state="degraded"] .connection-dot { background: var(--dot-degraded); box-shadow: var(--dot-degraded-fx); }
    .layout {
      flex: 1 0 auto; display: grid; gap: 20px; align-items: start;
      padding: 0 28px; max-width: 1280px; width: 100%; margin: 0 auto;
    }
    .rail { display: flex; flex-direction: column; gap: 16px; min-width: 0; }
    .main-flow { min-width: 0; }
    @media (min-width: 1100px) {
      .layout { grid-template-columns: minmax(300px, 340px) 1fr; }
      .rail { position: sticky; top: 28px; }
    }
    .footer { flex: none; text-align: center; font-size: 11px; color: var(--text-3); letter-spacing: .02em; padding: 24px 28px 26px; }
    .section-title { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; margin: 4px 0 14px; }
    .section-title h2 { font-size: 20px; font-weight: 800; color: var(--text); }
    .section-title p { font-size: 12px; color: var(--text-3); }
    /* ===== Panels (glass-1) ===== */
    .panel {
      background: var(--g1-sheen), var(--g1-bg); border: 1px solid var(--g1-border);
      border-radius: var(--r-20); box-shadow: var(--g1-shadow), var(--g1-inner);
      -webkit-backdrop-filter: var(--g1-blur); backdrop-filter: var(--g1-blur);
      min-width: 0;
    }
    .panel-inner { padding: 20px; }
    .panel-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; margin-bottom: 16px; }
    .panel-head h2 { font-size: 20px; font-weight: 800; color: var(--text); }
    .panel-head .subtle { margin-top: 4px; }
    /* ===== Mic hero ===== */
    .mic-hero .panel-inner { padding-top: 22px; }
    .mic-lamp {
      width: 56px; height: 56px; border-radius: 50%; flex: none; order: -1; margin-right: 4px;
      background: var(--lamp-unknown-face);
      box-shadow: var(--lamp-base-fx), var(--lamp-unknown-fx);
      -webkit-backdrop-filter: var(--lamp-unknown-blur); backdrop-filter: var(--lamp-unknown-blur);
      transition: background 160ms ease-out, box-shadow 160ms ease-out, opacity 160ms ease-out;
      position: relative;
    }
    .mic-lamp::before { content: ""; position: absolute; top: 15%; left: 20%; width: 12px; height: 12px; border-radius: 50%;
      background: radial-gradient(circle, rgba(255,255,255,.85), rgba(255,255,255,0) 70%); }
    .mic-word { display: block; font-size: 27px; font-weight: 800; letter-spacing: -.01em; line-height: 1.15; text-transform: uppercase; color: var(--dim-word); margin: 0 0 8px; transition: color var(--t-state), text-shadow var(--t-state); }
    #mic-status[data-state="unmuted"] ~ .mic-lamp { background: var(--lamp-live-face); box-shadow: var(--lamp-base-fx), var(--lamp-live-fx); -webkit-backdrop-filter: none; backdrop-filter: none; }
    #mic-status[data-state="muted"] ~ .mic-lamp { background: var(--lamp-muted-face); box-shadow: var(--lamp-base-fx), var(--lamp-muted-fx); -webkit-backdrop-filter: none; backdrop-filter: none; animation: lamp-breathe 2.6s ease-in-out infinite; }
    #mic-status[data-state="unreachable"] ~ .mic-lamp { opacity: .5; }
    #mic-status[data-state="unreachable"] ~ .mic-lamp::after { content: ""; position: absolute; inset: -7px; border-radius: 50%; border: 1px dashed var(--lamp-unreach-ring); }
    @keyframes lamp-breathe {
      0%, 100% { transform: scale(1); }
      50% { transform: scale(1.045); box-shadow: var(--lamp-base-fx), 0 0 26px var(--lamp-muted-ring), 0 0 56px var(--lamp-muted-ring); }
    }
    .mic-word[data-state="unmuted"] { color: var(--live-word); }
    .mic-word[data-state="muted"] { color: var(--muted-word); text-shadow: var(--muted-word-fx); }
    #mic-line { font-size: 13px; margin: 0 0 16px; }
    /* ===== Badges (eight states) ===== */
    .status-badge {
      display: inline-flex; align-items: center; padding: 4px 10px; border-radius: 999px;
      font-size: 11px; font-weight: 740; letter-spacing: .06em; text-transform: uppercase;
      border: 1px solid transparent; transition: background var(--t-state), border-color var(--t-state), color var(--t-state);
    }
    .status-badge[data-state="unmuted"], .status-badge[data-state="on"] { color: var(--live-text); background: var(--live-bg); border-color: var(--live-border); }
    .status-badge[data-state="muted"], .status-badge[data-state="error"] { color: var(--muted-text); background: var(--muted-bg); border-color: var(--muted-border); }
    .status-badge[data-state="unreachable"] { color: var(--muted-text); background: rgba(255,135,163,.08); border-color: var(--unreach-border); border-style: dashed; }
    .status-badge[data-state="off"], .status-badge[data-state="unknown"] { color: var(--text-3); background: var(--unknown-bg); border-color: var(--off-border); }
    .status-badge[data-state="disconnected"] { color: var(--text-3); background: transparent; border-color: var(--disc-border); border-style: dashed; }
    /* ===== Buttons ===== */
    button {
      font: inherit; font-size: 13px; font-weight: 700; color: var(--b2-text);
      display: inline-flex; align-items: center; justify-content: center; gap: 8px;
      min-height: 44px; padding: 10px 18px; border-radius: 999px; cursor: pointer;
      background: var(--b2-bg); border: 1px solid var(--b2-border);
      transition: transform var(--t-fast), background var(--t-state), border-color var(--t-state), color var(--t-state), box-shadow var(--t-fast), opacity var(--t-state);
      box-shadow: var(--b2-shadow);
    }
    button:hover { background: var(--b2-bg-hover); border-color: var(--b2-border-hover); transform: translateY(-1px); }
    button:active { transform: translateY(1px); background: rgba(244,246,255,.12); }
    button:disabled { opacity: .45; cursor: not-allowed; transform: none !important; }
    button:focus-visible, input:focus-visible {
      outline: 2px solid var(--signal); outline-offset: 3px;
      box-shadow: 0 0 0 5px var(--ring-halo);
    }
    .button-primary { background: var(--b1-bg); color: var(--pbtn-ink); border-color: transparent; box-shadow: var(--b1-shadow); }
    .button-primary:hover { background: var(--b1-bg-hover); box-shadow: var(--b1-shadow-hover); }
    .button-primary:active { box-shadow: var(--b1-shadow-active); }
    .button-primary:disabled { opacity: .45; box-shadow: none; }
    .button-quiet { background: transparent; border-color: transparent; color: var(--quiet-text); box-shadow: none; }
    .button-quiet:hover { background: var(--quiet-bg-hover); color: var(--text); border-color: transparent; }
    .button-quiet:active { background: var(--quiet-bg-active); }
    .button-quiet:disabled { opacity: .40; }
    .button-quiet[data-delete], .button-quiet.button-danger { color: var(--qd-text); }
    .button-quiet[data-delete]:hover, .button-quiet.button-danger:hover { background: var(--qd-bg-hover); }
    .button-quiet[data-delete]:active, .button-quiet.button-danger:active { background: var(--qd-bg-active); }
    .button-cool { color: var(--hint-cool); } .button-cool:hover { background: rgba(125,227,255,.08); }
    .button-warm { color: var(--hint-warm); } .button-warm:hover { background: rgba(255,201,125,.08); }
    .button-icon .icon { margin: -2px 0; }
    .power-row { display: flex; flex-wrap: wrap; gap: 10px; }
    .trim-row { display: flex; flex-wrap: wrap; gap: 8px; }
    .trim-row button { min-width: 58px; min-height: 36px; padding: 7px 12px; }
    /* ===== Control blocks (glass-2) ===== */
    .control-grid { display: flex; flex-direction: column; gap: 12px; }
    .control-block {
      background: var(--g2-bg); border: 1px solid var(--g2-border); border-radius: var(--r-14);
      padding: 16px; box-shadow: var(--g2-shadow), var(--g2-inner);
    }
    .control-label { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; margin-bottom: 10px; color: var(--text-3); font-size: 12px; }
    .control-label span:first-child { color: var(--text-2); font-weight: 650; }
    .control-label output {
      background: var(--recess-bg); border: 1px solid var(--recess-border); border-radius: var(--r-6);
      padding: 2px 8px; font-size: 13px; font-weight: 700; color: var(--text);
      box-shadow: var(--recess-inner);
    }
    .control-label output[data-tone="unknown"] { color: var(--text-3); }
    /* ===== Sliders ===== */
    .range-wrap { display: flex; flex-direction: column; gap: 6px; }
    .range-hints { display: flex; justify-content: space-between; font-size: 11px; color: var(--text-3); }
    input[type=range] { -webkit-appearance: none; appearance: none; width: 100%; height: 22px; background: transparent; cursor: pointer; margin: 0; }
    input[type=range]:disabled { cursor: not-allowed; }
    input[type=range]::-webkit-slider-runnable-track {
      height: 6px; border-radius: 999px; background: rgba(255,255,255,.12);
    }
    input[type=range]::-webkit-slider-thumb {
      -webkit-appearance: none; appearance: none; width: 18px; height: 18px; border-radius: 50%;
      background: var(--thumb-bg); border: none; margin-top: -6px; box-shadow: var(--thumb-fx);
      transition: box-shadow var(--t-fast);
    }
    input[type=range]:not(:disabled):hover::-webkit-slider-thumb { box-shadow: var(--thumb-fx), 0 0 0 6px rgba(244,246,255,.10); }
    input[type=range]:focus-visible::-webkit-slider-thumb { box-shadow: var(--thumb-fx), 0 0 0 5px var(--ring-halo); }
    input[type=range]:disabled::-webkit-slider-thumb { background: rgba(244,246,255,.35); box-shadow: none; }
    input[type=range]::-moz-range-track { height: 6px; border-radius: 999px; background: rgba(255,255,255,.12); }
    input[type=range]::-moz-range-thumb { width: 18px; height: 18px; border-radius: 50%; background: var(--thumb-bg); border: none; box-shadow: var(--thumb-fx); }
    input[data-field="brightness"]::-webkit-slider-runnable-track, #group-brightness::-webkit-slider-runnable-track {
      background-image: linear-gradient(90deg, rgba(240,244,255,.28), #eef2ff);
      background-size: calc(var(--fill-pct, 50) * 1%) 100%; background-repeat: no-repeat;
      background-color: rgba(255,255,255,.12);
    }
    input[data-field="brightness"]::-moz-range-track, #group-brightness::-moz-range-track {
      background-image: linear-gradient(90deg, rgba(240,244,255,.28), #eef2ff);
      background-size: calc(var(--fill-pct, 50) * 1%) 100%; background-repeat: no-repeat;
      background-color: rgba(255,255,255,.12);
    }
    input[data-field="temp"]::-webkit-slider-runnable-track, #group-temp::-webkit-slider-runnable-track {
      background:
        repeating-linear-gradient(90deg, rgba(10,12,24,.30) 0 1px, transparent 1px calc(100% / 18)),
        repeating-linear-gradient(90deg, transparent 0 calc(100% / 18), rgba(244,246,255,.25) calc(100% / 18) calc(100% / 18 + 1px)),
        linear-gradient(90deg, #ffb169 0%, #ffc58c 24%, #ffdcb0 44%, #f4e6c8 58%, #dde3f0 78%, var(--track-cool-end) 100%);
    }
    input[data-field="temp"]::-moz-range-track, #group-temp::-moz-range-track {
      background:
        repeating-linear-gradient(90deg, rgba(10,12,24,.30) 0 1px, transparent 1px calc(100% / 18)),
        linear-gradient(90deg, #ffb169 0%, #ffc58c 24%, #ffdcb0 44%, #f4e6c8 58%, #dde3f0 78%, var(--track-cool-end) 100%);
    }
    input[type=range]:disabled::-webkit-slider-runnable-track { opacity: .5; }
    input[type=range]:disabled::-moz-range-track { opacity: .5; }
    /* ===== Error banner (page) ===== */
    .error-banner {
      margin-top: 16px; display: flex; align-items: center; justify-content: space-between; gap: 12px;
      background: var(--err-bg); border: 1px solid var(--err-border); border-radius: var(--r-14);
      padding: 12px 14px; color: var(--err-text); font-size: 13px;
    }
    .error-banner[hidden] { display: none !important; }
    .error-copy { display: flex; align-items: center; gap: 10px; min-width: 0; }
    .error-copy .icon { color: var(--err-text); }
    #retry { min-height: 36px; padding: 7px 12px; }
    /* ===== Settings ===== */
    .settings-save { display: flex; gap: 10px; align-items: center; }
    #settings-name {
      flex: 1; min-height: 44px; padding: 10px 14px; font: inherit; font-size: 13px;
      color: var(--text); background: var(--recess-bg); border: 1px solid var(--recess-border);
      border-radius: var(--r-10); box-shadow: var(--recess-inner);
    }
    #settings-name::placeholder { color: var(--text-3); }
    #settings-name:hover { border-color: rgba(244,246,255,.22); }
    .settings-line { margin: 10px 0 12px; font-size: 12px; }
    .settings-line[data-tone="error"] { color: var(--err-text); }
    .settings-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
    .setting-row {
      display: flex; align-items: center; gap: 8px; padding: 6px 6px 6px 12px;
      background: var(--g2-bg); border: 1px solid var(--g2-border); border-radius: var(--r-10);
      box-shadow: var(--g2-shadow), var(--g2-inner);
      transition: border-color var(--t-state);
    }
    .setting-row:hover { border-color: rgba(244,246,255,.20); }
    .setting-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; color: var(--text); }
    .setting-row button { min-height: 36px; padding: 7px 12px; }
    .empty {
      border: 1px dashed var(--disc-border); border-radius: var(--r-14); background: rgba(244,246,255,.03);
      padding: 30px 20px; text-align: center; color: var(--text-3); font-size: 12px;
      display: flex; flex-direction: column; gap: 6px;
    }
    .empty strong { color: var(--text-2); font-size: 13px; }
    /* ===== Lights grid + cards ===== */
    .lights-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 16px; }
    .light-card {
      background: var(--g1-sheen), var(--g1-bg); border: 1px solid var(--g1-border);
      border-radius: var(--r-14); padding: 17px;
      -webkit-backdrop-filter: var(--g1-blur); backdrop-filter: var(--g1-blur);
      --bri01: calc(var(--glow-bri, 0) / 100);
      --glow-t: calc(var(--glow-hue, 9) / 18 * 100);
      --aura-c: #ffc089;
      --aura-c: color-mix(in oklab, var(--aura-warm), var(--aura-cool) calc(var(--glow-t) * 1%));
      border-color: color-mix(in oklab, var(--g1-border), var(--aura-c) calc(45% * var(--bri01)));
      box-shadow:
        0 16px 34px rgba(2,4,14,.50),
        inset 0 1px 0 rgba(255,255,255,.15),
        0 6px calc(24px + 34px * var(--bri01)) color-mix(in oklab, var(--aura-c) calc(30% * var(--bri01)), transparent),
        0 0 calc(8px + 14px * var(--bri01)) color-mix(in oklab, var(--aura-c) calc(40% * var(--bri01)), transparent),
        inset 0 -20px 34px -20px color-mix(in oklab, var(--aura-c) calc(20% * var(--bri01)), transparent);
      transition: border-color var(--t-state), box-shadow 160ms ease-out;
    }
    .card-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; margin-bottom: 12px; }
    .light-name { font-size: 14px; font-weight: 700; color: var(--text); }
    .card-meta { font-size: 12px; color: var(--text-3); margin-top: 2px; }
    .power-button {
      display: inline-flex; align-items: center; gap: 8px; min-height: 36px; padding: 7px 14px;
      border-radius: 999px; font-size: 12px; font-weight: 700;
      background: var(--b2-bg); border: 1px solid var(--b2-border); color: var(--b2-text);
      transition: transform var(--t-fast), background var(--t-state), border-color var(--t-state), color var(--t-state);
    }
    .power-button:hover { transform: translateY(-1px); background: var(--b2-bg-hover); border-color: var(--b2-border-hover); }
    .power-button:active { transform: translateY(1px); }
    .power-button:disabled { opacity: .45; cursor: not-allowed; transform: none !important; }
    .power-button[data-on="true"] {
      color: var(--live-text); background: rgba(88,240,200,.08); border-color: rgba(88,240,200,.45);
    }
    .power-button[data-on="true"] .icon { filter: drop-shadow(0 0 5px rgba(88,240,200,.5)); }
    .card-control { margin-top: 12px; }
    .card-control .control-label { margin-bottom: 8px; }
    .card-error {
      margin-top: 14px; display: flex; align-items: center; gap: 8px;
      background: var(--carderr-bg); border: 1px solid var(--carderr-border); border-radius: var(--r-10);
      padding: 8px 10px; color: var(--err-text); font-size: 12px;
    }
    .card-error[hidden] { display: none !important; }
    /* ===== Narrow fallback ===== */
    @media (max-width: 900px) {
      .topbar { padding: 14px 16px 10px; flex-wrap: wrap; }
      .layout { padding: 0 16px; }
      .panel-inner { padding: 15px; }
      .panel-head { margin-bottom: 16px; }
      .section-title { align-items: flex-start; flex-direction: column; gap: 4px; }
      .lights-grid { grid-template-columns: 1fr; }
    }
```

- [ ] **Step 2: Compile-check + screenshot**

```bash
go build ./...
```

Screenshot at 1440×900 (profile default): layout may still be single-column (DOM restructure is Task 5) but cards/panels/badges/sliders must render dark Glassline metal; console must be clean.

- [ ] **Step 3: Commit**

```bash
git add internal/lightui/index.html
git commit -m "feat(ui): Glassline dark component skins"
```

---

## Task 4: Stylesheet — part 3 (light theme, aura rewrite, fallbacks)

**Files:**
- Modify: `internal/lightui/index.html` — append after the Task 3 block (before `</style>`).

**Interfaces:**
- Consumes: tokens + selectors from Tasks 2–3.
- Produces: the `@media (prefers-color-scheme: light)` override block (tokens + light-only structural deltas), the `@supports not (backdrop-filter: blur(1px))` fallback, and the `@media (prefers-reduced-motion: reduce)` kill block.

- [ ] **Step 1: Append the light block (complete)**

```css
    /* ===== LIGHT THEME (system-adaptive) ===== */
    @media (prefers-color-scheme: light) {
      :root {
        --field-base: #eef1fb;
        --f1: radial-gradient(900px 1000px at 50% 38%, transparent 55%, rgba(200,205,240,.35) 100%);
        --f2: radial-gradient(760px 560px at -8% -18%, rgba(150,120,235,.40), rgba(150,120,235,0) 62%);
        --f3: radial-gradient(820px 600px at 112% 2%, rgba(90,180,220,.38), rgba(90,180,220,0) 58%);
        --f4: radial-gradient(900px 680px at 62% 118%, rgba(235,140,200,.32), rgba(235,140,200,0) 60%);
        --g1-sheen: linear-gradient(180deg, rgba(255,255,255,.60), rgba(255,255,255,0) 40%);
        --g1-bg: rgba(255,255,255,.60);
        --g1-border: rgba(255,255,255,.72);
        --g1-blur: blur(20px) saturate(1.25);
        --g1-shadow: 0 1px 2px rgba(47,54,116,.05), 0 16px 40px -12px rgba(56,64,140,.18);
        --g1-inner: inset 0 1px 0 rgba(255,255,255,.90);
        --g2-bg: rgba(255,255,255,.45);
        --g2-border: rgba(255,255,255,.58);
        --g2-blur: blur(12px) saturate(1.2);
        --g2-shadow: 0 6px 16px -8px rgba(56,64,140,.12);
        --g2-inner: inset 0 1px 0 rgba(255,255,255,.70);
        --g3-bg: rgba(255,255,255,.72);
        --g3-border: rgba(255,255,255,.85);
        --g3-blur: blur(8px);
        --g3-shadow: 0 2px 8px -2px rgba(47,54,116,.14);
        --g3-inner: inset 0 1px 0 rgba(255,255,255,.95);
        --recess-bg: rgba(23,28,60,.08);
        --recess-border: rgba(23,28,60,.12);
        --recess-inner: inset 0 1px 2px rgba(23,28,60,.10), inset 0 -1px 0 rgba(255,255,255,.55);
        --text: #171b2e; --text-2: rgba(23,27,46,.64); --text-3: rgba(23,27,46,.66);
        --text-disabled: rgba(23,27,46,.52); --pbtn-ink: #f8f9ff;
        --signal: #0c6b85; --ring-halo: rgba(12,107,133,.18);
        --live-text: #0a6e50; --live-bg: rgba(10,110,80,.06); --live-border: rgba(10,110,80,.40); --live-word: #0a6e50;
        --muted-text: #be123c; --muted-bg: rgba(190,18,60,.08); --muted-border: rgba(190,18,60,.38);
        --muted-word: #be123c; --muted-word-fx: none;
        --unreach-border: rgba(190,18,60,.55);
        --dim-word: #5a6079; --off-border: rgba(23,27,46,.12);
        --unknown-bg: rgba(23,27,46,.05); --disc-border: rgba(23,27,46,.24);
        --err-bg: rgba(190,18,60,.07); --err-border: rgba(190,18,60,.45); --err-text: #be123c;
        --carderr-bg: rgba(190,18,60,.06); --carderr-border: rgba(190,18,60,.45);
        --hint-warm: #8a5003; --hint-cool: #0369a1;
        --b1-bg: #1f2438; --b1-bg-hover: #2a3150;
        --b1-shadow: 0 8px 20px -6px rgba(31,36,56,.35), inset 0 1px 0 rgba(255,255,255,.14);
        --b1-shadow-hover: 0 10px 24px -6px rgba(31,36,56,.40), inset 0 1px 0 rgba(255,255,255,.14);
        --b1-shadow-active: 0 2px 6px rgba(31,36,56,.30), inset 0 1px 0 rgba(255,255,255,.14);
        --b2-bg: rgba(255,255,255,.55); --b2-border: rgba(23,28,60,.16); --b2-text: var(--text);
        --b2-bg-hover: rgba(255,255,255,.70); --b2-border-hover: rgba(23,28,60,.22);
        --b2-shadow: inset 0 1px 0 rgba(255,255,255,.9), 0 1px 2px rgba(47,54,116,.08);
        --quiet-text: var(--text-2); --quiet-bg-hover: rgba(23,28,60,.06); --quiet-bg-active: rgba(23,28,60,.10);
        --qd-text: #be123c; --qd-bg-hover: rgba(190,18,60,.08); --qd-bg-active: rgba(190,18,60,.12);
        --dot-online: #0e9670; --dot-online-fx: 0 0 0 3px rgba(14,150,112,.18);
        --dot-degraded: #b06204; --dot-degraded-fx: 0 0 0 3px rgba(176,98,4,.18);
        --dot-idle: rgba(23,27,46,.35); --dot-idle-fx: 0 0 0 3px rgba(23,27,46,.10);
        --lamp-base-fx: inset 0 -6px 12px rgba(23,28,60,.18), inset 0 2px 3px rgba(255,255,255,.65);
        --lamp-unknown-face: radial-gradient(circle at 34% 30%, #f7f8fd, #a7accb 55%, #7a80a3);
        --lamp-unknown-fx: 0 0 0 3px rgba(90,96,121,.16); --lamp-unknown-blur: none;
        --lamp-live-face: radial-gradient(circle at 34% 30%, #f4fefb, #17b98b 55%, #0b6349);
        --lamp-live-fx: 0 0 0 3px rgba(10,90,66,.14), 0 8px 22px rgba(10,90,66,.30);
        --lamp-muted-face: radial-gradient(circle at 34% 30%, #fff6f7, #ee4a6e 55%, #9f1239);
        --lamp-muted-fx: 0 0 0 3px rgba(159,18,57,.14), 0 8px 22px rgba(159,18,57,.30);
        --lamp-muted-ring: rgba(190,18,60,.50);
        --lamp-unreach-ring: rgba(110,116,148,.55);
        --aura-warm: #d8913f; --aura-cool: #0369a1;
        --track-warm-end: #a34d08; --track-cool-end: #0369a1;
        --thumb-bg: #fff;
        --thumb-fx: 0 0 0 1.5px rgba(23,28,60,.50), 0 2px 5px rgba(23,28,60,.25);
        --sel: rgba(12,107,133,.22); --scrollbar: rgba(23,28,60,.22);
      }
      /* Light-only structural deltas */
      body { color: var(--text); }
      /* Light cards signal "lit" by deepening, never brightening */
      .light-card {
        --aura-h: calc(38 + var(--glow-hue, 9) * 9.11);
        border-color: hsl(var(--aura-h) 76% 42% / calc(.02 + var(--glow-bri, 0) * .0026));
        box-shadow:
          0 1px 2px rgba(47,54,116,.05),
          0 16px 40px -12px rgba(56,64,140,.18),
          0 8px 28px -6px hsl(var(--aura-h) 82% 46% / calc(.04 + var(--glow-bri, 0) * .0028)),
          inset 0 0 22px hsl(var(--aura-h) 88% 58% / calc(var(--glow-bri, 0) * .0013)),
          var(--g1-inner);
      }
      .light-card:has(.card-error:not([hidden])) { border-color: rgba(190,18,60,.45); }
      .light-card:has(.status-badge[data-state="disconnected"]) {
        box-shadow: 0 1px 2px rgba(47,54,116,.05), 0 16px 40px -12px rgba(56,64,140,.18), var(--g1-inner);
        border-color: var(--g1-border);
      }
      .power-button[data-on="true"] { color: var(--live-text); background: rgba(10,110,80,.05); border-color: rgba(10,110,80,.85); }
      .power-button[data-on="true"] .icon { filter: none; }
      /* Brightness fill: deep cyan→teal on ink-wash track */
      input[data-field="brightness"]::-webkit-slider-runnable-track, #group-brightness::-webkit-slider-runnable-track {
        background-color: rgba(23,28,60,.12);
        background-image: linear-gradient(90deg, #0b7c98, #087a56);
        box-shadow: inset 0 1px 2px rgba(23,28,60,.14), 0 1px 0 rgba(255,255,255,.55);
      }
      input[data-field="brightness"]::-moz-range-track, #group-brightness::-moz-range-track {
        background-color: rgba(23,28,60,.12);
        background-image: linear-gradient(90deg, #0b7c98, #087a56);
      }
      /* Warmth ribbon: physical ramp + honesty ticks */
      input[data-field="temp"]::-webkit-slider-runnable-track, #group-temp::-webkit-slider-runnable-track {
        background:
          repeating-linear-gradient(90deg, transparent 0 calc(5.5556% - 1px), rgba(23,28,60,.30) calc(5.5556% - 1px) 5.5556%),
          linear-gradient(90deg, var(--track-warm-end) 0%, #d8913f 24%, #f2ead9 42%, #dee9f3 55%, #7fb4e2 78%, var(--track-cool-end) 100%);
        box-shadow: inset 0 1px 2px rgba(23,28,60,.14), 0 1px 0 rgba(255,255,255,.55);
      }
      input[data-field="temp"]::-moz-range-track, #group-temp::-moz-range-track {
        background:
          repeating-linear-gradient(90deg, transparent 0 calc(5.5556% - 1px), rgba(23,28,60,.30) calc(5.5556% - 1px) 5.5556%),
          linear-gradient(90deg, var(--track-warm-end) 0%, #d8913f 24%, #f2ead9 42%, #dee9f3 55%, #7fb4e2 78%, var(--track-cool-end) 100%);
      }
      /* Thumb: white face + ink identity ring */
      input[type=range]:not(:disabled):hover::-webkit-slider-thumb { box-shadow: 0 0 0 1.5px rgba(23,28,60,.60), 0 2px 6px rgba(23,28,60,.28); }
      input[type=range]:active::-webkit-slider-thumb { box-shadow: 0 0 0 1.5px rgba(23,28,60,.50), 0 3px 8px rgba(23,28,60,.30); }
      input[type=range]:focus-visible { outline-offset: 6px; }
      input[type=range]:disabled::-webkit-slider-thumb { background: #fff; box-shadow: 0 0 0 1.5px rgba(23,28,60,.30); }
      input[type=range]::-moz-range-thumb { border: 1.5px solid rgba(23,28,60,.28); }
      .error-banner { color: var(--err-text); }
      #retry { color: var(--err-text); }
      .button-quiet[data-delete], .button-quiet.button-danger { color: var(--qd-text); }
    }
    /* ===== backdrop-filter fallback ===== */
    @supports not (backdrop-filter: blur(1px)) {
      :root { --g1-bg: rgba(19,21,42,.90); --g3-bg: #22253f; --g1-blur: none; --g3-blur: none; }
    }
    @media (prefers-color-scheme: light) {
      @supports not (backdrop-filter: blur(1px)) {
        :root { --g1-bg: rgba(255,255,255,.92); --g2-bg: rgba(255,255,255,.85); --g3-bg: rgba(255,255,255,.95); --g1-blur: none; --g2-blur: none; --g3-blur: none; }
      }
    }
    /* ===== reduced motion ===== */
    @media (prefers-reduced-motion: reduce) {
      *, *::before, *::after { scroll-behavior: auto !important; transition-duration: .01ms !important; animation-duration: .01ms !important; animation-iteration-count: 1 !important; }
      #mic-status[data-state="muted"] ~ .mic-lamp { animation: none; transform: scale(1); }
    }
```

- [ ] **Step 2: Constraint static-checks**

```bash
go build ./...
grep -c '@media (prefers-color-scheme: light)' internal/lightui/index.html   # expect 2
grep -c '@supports not (backdrop-filter' internal/lightui/index.html          # expect 2
grep -c 'prefers-reduced-motion' internal/lightui/index.html                  # expect 2 (kill + lamp pin)
```

⚠️ The second `prefers-color-scheme: light` count includes the `@supports` fallback wedge inside the light block — verify by eye that only one top-level light media block carries tokens.

- [ ] **Step 3: Commit**

```bash
git add internal/lightui/index.html
git commit -m "feat(ui): Glassline light theme, aura rewrite, fallbacks, reduced-motion"
```

---

## Task 5: DOM restructure — command deck

**Files:**
- Modify: `internal/lightui/index.html` — body only (everything between `<body>` and `<script src=`), restyled per layout below. No behavioral code changes in this task except two additive markup attributes called out inline.

**Interfaces:**
- Consumes: stylesheet from Tasks 2–4; existing body sections.
- Produces (structure JS in Task 6 hooks into): `.shell > .layout > .rail > (mic panel, gang panel, settings panel) + .main-flow > (section-title, lights-grid, empty)`; `.mic-lamp` and `#mic-word` exist; `#group-brightness` carries inline `style="--fill-pct:55"`.

- [ ] **Step 1: Restructure `<body>` to this exact skeleton (move — do not rewrite — the existing sections)**

Replace the `<main class="shell"> ... </main>` wrapper with:

```html
  <div class="shell">
    <header class="topbar"> ...existing brand + connection pill, unchanged... </header>

    <div class="layout">
      <div class="rail">
        <!-- MIC HERO — the existing mic <section class="panel"> moved here FIRST and upgraded: -->
        <section class="panel mic-hero" aria-labelledby="mic-title">
          <div class="panel-inner">
            <div class="panel-head">
              <div>
                <p class="eyebrow">Audio</p>
                <h2 id="mic-title">Microphone</h2>
              </div>
              <span id="mic-status" class="status-badge" data-state="unknown">unknown</span>
              <span class="mic-lamp" aria-hidden="true"></span>
            </div>
            <span id="mic-word" class="mic-word" data-state="unknown">unknown</span>
            <p id="mic-line" class="subtle">Waiting for the daemon…</p>
            <p class="subtle">Yeti X mute state, straight from the daemon. These buttons move the mic only — the meeting-app sweep stays on the physical mic button, the tray mute item, and the Stream Deck key.</p>
            <div style="height:14px"></div>
            <div class="power-row" role="group" aria-label="Microphone mute">
              <button class="button-primary" type="button" data-mic-action="mute">Mute</button>
              <button type="button" data-mic-action="unmute">Unmute</button>
              <!-- Toggle starts disarmed (behavioral invariant) -->
              <button class="button-quiet" type="button" data-mic-action="toggle" disabled>Toggle</button>
            </div>
          </div>
        </section>

        <!-- GANG — existing all-lights section moved here SECOND, `<section class="panel" aria-labelledby="all-lights-title">` unchanged inside -->
        ...existing gang section (lines ~183-244) verbatim, still containing #error-banner and #refresh...

        <!-- SETTINGS — existing settings section moved here THIRD, unchanged inside except: mark its delete buttons with class "button-danger" (additive class alongside button-quiet — their data-delete attribute is already the hook, class is for the light-theme rule parity) -->
        ...existing settings section (lines ~266-283) verbatim...
      </div>

      <main class="main-flow">
        ...existing .section-title block, #lights-grid, #empty, verbatim...
      </main>
    </div>

    <footer class="footer">...existing footer line...</footer>
  </div>
```

Notes for the implementer (each is a hard requirement):
- DOM order = visual order: rail holds mic → gang → settings in that order (move the mic section NODE above the gang section node; current file has gang first).
- No `id` may be removed, renamed, or duplicated; every existing `role`/`aria-*` stays; `#error-banner` remains inside the gang section where it is today (all its JS references are by id — moving nodes is safe as long as the tree shape inside each section is untouched).
- `#group-brightness` gets `style="--fill-pct:55"` inline in its markup (the 55 matches its existing `value="55"`; Task 6 maintains it from then on).
- The two mic paragraphs keep their exact current copy; the spacer `<div style="height:14px">` may instead be a classed margin if the implementer prefers — but keep the visual gap ≈14 px between the explainer line and the button row.
- The light-card template (`cardMarkup` in JS) is not touched in this task.

- [ ] **Step 2: Compile + both-width screenshots**

```bash
go build ./...
curl -s -X POST http://127.0.0.1:8788/__profile__ -d '{"profile":"default"}'
```

Browser checks: 1440×900 → rail on the left (mic/gang/settings stacked, sticky), panels grid ≥2 columns on the right; 650×900 → single column, order: topbar → mic → gang → settings → panels → footer. Zero console errors.

- [ ] **Step 3: Commit**

```bash
git add internal/lightui/index.html
git commit -m "feat(ui): command-deck DOM — rail mic/gang/settings, mainFlow panels, lamp + state word"
```

---

## Task 6: JS hooks — glow, fill-pct, mic commit buffer, state word

**Files:**
- Modify: `internal/lightui/index.html` — the single attribute-less inline `<script>` block only.

**Interfaces:**
- Consumes: helpers `lightIsOn`, `temperatureIndex`, `lightKey` (existing); DOM from Task 5 (`#mic-word`, `.mic-lamp`, `--fill-pct` hook).
- Produces (additive functions, all new): `applyGlow(cardEl, light)`; `writeFill(inputEl, pct)`; mic commit buffer (`micCommittedAt` module-scope `let`). No existing function is removed or renamed.

- [ ] **Step 1: Add the helpers (place immediately after the `enqueueMutation` definition)**

```js
      // -- Glow + fill helpers (additive; never required for the page to function) --
      function applyGlow(cardEl, light) {
        if (!cardEl || !cardEl.style || !cardEl.style.setProperty) return;
        const on = lightIsOn(light);
        const bri = on ? Math.max(0, Math.min(100, Number(light.brightness))) : 0;
        const hue = on ? temperatureIndex(light.temp) : 0;
        cardEl.style.setProperty("--glow-bri", String(bri));
        cardEl.style.setProperty("--glow-hue", String(hue >= 0 ? hue : 0));
      }
      function writeFill(inputEl, pct) {
        if (!inputEl || !inputEl.style || !inputEl.style.setProperty) return;
        inputEl.style.setProperty("--fill-pct", String(Math.max(0, Math.min(100, Number(pct)))));
      }
      let micCommittedAt = 0;
```

- [ ] **Step 2: Wire `applyGlow` + per-card fill into the light-state writers (two hooks)**

In `updateCard(light)`, immediately after the `tempOutput.textContent = ...` line (i.e., after all state writes, before the error block), insert:

```js
        applyGlow(card, light);
        writeFill(brightness, on ? light.brightness : 0);
```

In the `input` listener inside `bindCards()` (block `card.querySelectorAll("input[type=range]")...`), after the existing `output.textContent = ...` line insert:

```js
              if (field === "brightness") writeFill(input, value);
```

- [ ] **Step 3: Wire group fills into `bindTopControls()` + `updateGroupDefaults()`**

In `bindTopControls()`, inside the `brightness` input listener, after the output line add:

```js
          writeFill(brightness, Number(brightness.value));
```

In `updateGroupDefaults(lights)`, inside the `if (!brightnessActive)` block, in the `else if` branch where `brightness.value = brightnessValues[0]` is set, add on the next line:

```js
            writeFill(brightness, brightnessValues[0]);
```

(No write in the "Unknown"/"Mixed" branches — last local thumb position is the fallback, per design-dark §3.4.)

- [ ] **Step 4: Mic commit buffer + state word in `updateMic()` + click hook in `bindMicControls()`**

At the top of `updateMic(micState)`, insert:

```js
        if (Date.now() - micCommittedAt < 175) return; // a just-pressed verb owns the UI until the daemon acks
```

After the existing `$("mic-line").textContent = micLineText(micState);` line insert:

```js
        const word = $("mic-word");
        if (word) { word.dataset.state = micState; word.textContent = micState; }
```

In `bindMicControls()`, inside the click listener, as the FIRST statement add:

```js
            micCommittedAt = Date.now();
```

- [ ] **Step 5: Compile + behavior spot-checks**

```bash
go build ./...
sed -n '/^  <script>$/,/^  <\/script>$/p' internal/lightui/index.html | sed '1d;$d' > /tmp/opencode/verify/inline.js
node --check /tmp/opencode/verify/inline.js && echo "inline-script OK"
```

Browser spot-check (stub on `default`): poke one light's brightness slider via JS: `document.querySelector('[data-field="brightness"]').value=80; document.querySelector('[data-field="brightness"]').dispatchEvent(new Event("input"))` → track fill jumps to 80%, card aura edges strengthen. `POST /__profile__ {"profile":"default"}` then `sendBeacon`-less fetch check: `document.querySelectorAll(".light-card")[1].style.getPropertyValue("--glow-bri")` returns `"80"` for the second card (Spare/100→unknown→0 is fine too; choose an on light). Click Mute → mic state flips to muted, lamp breathes, word reads MUTED; the immediate 750 ms poll cannot flip it back because the stub committed the change (buffer guards the race, nothing visibly breaks).

- [ ] **Step 6: Runbook + commit**

```bash
go build ./...
git add internal/lightui/index.html
git commit -m "feat(ui): glow/fill writers, mic commit buffer + state word"
```

---

## Task 7: Full verification runbook (acceptance)

**Files:**
- None (pure verification + in-place fixes). Any fix returns to its owning task's patch and amends that file, then re-runs both checks.

**Interfaces:**
- Consumes: stub rig (Task 1, `/tmp/opencode/verify/serve.py`), browser tool, profiles: `default muted mixed errcard empty mic_unknown mic_unreachable degraded settings_store_error`.

**State matrix — verify EVERY cell at 1440×900 and 650×900, in the OS default (dark) theme AND a forced light pass** (force via browser emulated media if the tool supports it — else rely on Task 8's token diff + a temporary `:root` override; see Task 8):

- [ ] **Step 1: Connection + topbar** — `default`→ dot green-halo "Daemon connected"; `degraded` → amber "Daemon issue"/"Panel offline"; fresh load shows dim "Connecting" before first poll.
- [ ] **Step 2: Mic hero × {unmuted, muted, unknown, unreachable}** — badge/lamp/word/line agree; breathing ONLY in muted (2.6 s); unknown = Toggle disabled, Mute/Unmute armed, bead renders the field; unreachable = all three disabled, dashed orbital ring, lamp opacity .5.
- [ ] **Step 3: Gang outputs** — `default` (COM5 on 55%/4950K vs COM7 on 80%/2900K) ⇒ group outputs read `Mixed`/`Mixed` in the dim tone; click COM7's power off ⇒ outputs resolve to the definite `55%`/`4950K` chips; drag group brightness then hit Save ⇒ `/tmp/opencode/verify/mutations.log` shows the slider POST **before** the settings save POST; definite-brightness state shows the fill tracking the thumb.
- [ ] **Step 4: Light cards** — `default` (on 55% 4950K, on 80% 2900K, off) + `mixed` (30% 6500K, 100% 2900K) + `errcard` (`card-error` strip visible; card border rose in light mode, aura still present but banner visually outranks: aura is outside, error strip inside) + off card = zero aura + disabled sliders + `off` badge; heater: power-button `data-on="true"` carries the lit-edge treatment; disconnected cards quieted.
- [ ] **Step 5: Settings** — empty (`empty` profile) → dashed empty state; 1 row + 3 rows (`default`) → apply/delete round-trip from the `settings-line`, rows keep hover border; `settings_store_error` → `#settings-line` shows the store error in rose text; over-42-byte name typed into `#settings-name` + Save → zero network call (no new line in `mutations.log`), page banner shows `SETTINGS_NAME_TOO_LONG` text, then clears after the timeout.
- [ ] **Step 6: Banner lifecycle** — `degraded` → banner visible with `role="alert"`, Retry present; press Retry (or wait for next poll after switching back to `default`) → banner fades, connection pill returns to online.
- [ ] **Step 7: Empty panels** — `empty` → dashed panel with two-part copy in main column; section count reads "0 lights".
- [ ] **Step 8: Keyboard walk (both widths, default profile)** — Tab from topbar: focus ring (2px signal + 5px halo) clearly visible on connection pill (if focusable), Refresh, all mic buttons (Toggle skipped only when disabled), every gang control, sliders (ring offset 6px clears the thumb), settings name input, Save, each row Apply/Delete, each card's power button + sliders. Tab order == DOM order (mic first). No focus trap.
- [ ] **Step 9: Motion budget** — `prefers-reduced-motion: reduce` emulated: lamp static, no lift/press, instant badge changes; normal: only the muted lamp animates (observe 3 s on `muted` profile: no color-field shimmering, no slider self-motion).
- [ ] **Step 10: Blur fallback spot-check** — force no-`backdrop-filter` (browser flag or emulation); light panels read as near-opaque porcelain (alphas raised); dark panels solid-navy. Field still peeks at card rims via border+shadow.
- [ ] **Step 11: Console + build** — `get_console_messages` shows zero errors/warnings across all profiles; final `go build ./...` and `go vet ./...` clean.

- [ ] **Step 12: Fix loop** — catalogue every defect (screenshot + spec line), fix in the owning task's block, re-verify the cell, amend the matching task commits (keep tasks atomic).

---

## Task 8: Light-theme systematic pass + test refresh map + handoff

**Files:**
- Modify: `internal/lightui/index.html` (only if defects surface)
- Create: `docs/superpowers/specs/glassline-test-refresh-map.md` (owner-facing; maps old `ui_test.go` pin categories to the new page)

- [ ] **Step 1: Force light theme for verification.** Preferred: browser/CDP emulation of `prefers-color-scheme: light` (DevTools Rendering tab / `Emulation.setEmulatedMedia`) so the real media query is exercised. If the tool cannot emulate: serve a *probe copy* of index.html where the light media-query tokens are spliced into `:root` (python one-liner over the file; regenerated each run from the real file — never hand-edited into it):

```bash
python3 - <<'EOF'
import re, pathlib
p = pathlib.Path("/home/dan/code/mutastic/.worktrees/ui-redesign-glassline/internal/lightui/index.html")
src = p.read_text()
light = re.search(r"@media \(prefers-color-scheme: light\) \{\n      :root \{(.*?)\n      \}", src, re.S).group(1)
out = src.replace("</head>", f"<style>:root{{{light}}}</style></head>")
pathlib.Path("/tmp/opencode/verify/index.light.html").write_text(out)
print("probe written")
EOF
```

…then point the stub to serve `/tmp/opencode/verify/index.light.html` for all page requests during the light pass (one-line change; restart stub). Additionally diff the light token block against `design-light.md §1` to catch drift: `sed -n '/prefers-color-scheme: light/,/^      }/p' internal/lightui/index.html`.

- [ ] **Step 2: Re-run the full state matrix (Task 7, Steps 1–11) in light.** Light-specific pass criteria: cards read white-frosted over pale field; lit cards cast a *colored shadow* (warm 2900K = amber pool, cool = sky) with zero halos; disconnected cards lose the chromatic cast entirely; lamp silhouettes stay deep (no washed-out bead); primaries are ink pills; focus ring `#0c6b85` clearly visible on every leaf (walk ≥10 stops).

- [ ] **Step 3: Contrast check (programmatic assist).** In the light-forced page, run this in the console (or via tool eval) for the four crucial pairs and assert the tool-reported ratios ≥4.5 (body) / ≥3 (large word):

```js
// sample reported computed styles; compute WCAG contrast in page
const lum = (rgb) => { const c = rgb.match(/\d+(\.\d+)?/g).map(Number).slice(0,3).map(v => { v /= 255; return v <= .03928 ? v/12.92 : ((v+.055)/1.055) ** 2.4; }); return .2126*c[0] + .7152*c[1] + .0722*c[2]; };
const ratio = (a,b) => (lum(a)+.05)/(lum(b)+.05);
const cs = (sel, prop="color") => getComputedStyle(document.querySelector(sel))[prop];
// report back the six numbers for: .light-name on .light-card bg, .card-meta on card, #mic-line on panel, badge text on badge bg (data-state=on), muted word on panel, footer text on body bg
```

- [ ] **Step 4: Write the refresh map** `docs/superpowers/specs/glassline-test-refresh-map.md` — regenerate counts from the FINAL file (commands included in the doc), mapping each old pin category to its newPage equivalent. Content skeleton (fill actual counts/lines at write time — pull them with the grep commands listed; do not hand-guess):

| Old pin (category) | New-page equivalent | How the suite should re-pin |
|---|---|
| `data-mic-action` trio + Toggle `disabled` | unchanged selectors/attrs | pin `data-mic-action="toggle" disabled` literal (still present) |
| `#mic-line` four texts | unchanged copy | re-pin each string |
| `.status-badge[data-state=...]` rule presence (×8) | same attribute set, restyled | grep final `--live-text`/`--muted-text`/… rules |
| `.mic-lamp` sibling keying | new | pin `#mic-status[data-state="muted"] ~ .mic-lamp` presence + `lamp-breathe` keyframes count (×1) |
| 750 ms poll line | unchanged | re-pin exact `window.setInterval(() => { refreshLights(true); refreshMic(); refreshSettings(); }, 750);` |
| `settingsNameOverByteCap` gate ×2 | unchanged call sites | re-count in final file |
| `flushPendingSliders()` ×3 | unchanged call sites | re-count |
| Node DOM-stub harness selectors (`.light-card`, `data-port`, etc.) | same hooks, new classes around them | update stub queries to new DOM (card structure moved classes only) |
| `?` template-var substitution | unchanged | n/a |
| Inline-script compile gate | unchanged | one attribute-less `<script>` remains |

Include verification appendix: list the runbook cells executed (Task 7/8) with results and screenshots paths (save under `/tmp/opencode/verify/shots/`).

- [ ] **Step 5: Final build + commit + summary**

```bash
go build ./... && go vet ./...
git add -A
git commit -m "docs: Glassline test-refresh map + handoff (verification appendix)"
git log --oneline -8
```

Summarize: what was built; commit list; verification evidence locations; the expected suite-failure classes the owner will refresh; any probe files left in `/tmp/opencode/verify/`.

---

## Self-review audit (author record)

1. **Spec coverage:** every §4 invariant → Task 5 notes + Task 6 patches + Task 7 cells (poll, gate×2, flush×3, mic verbs, state semantics, one-script, queue untouched). Design values → Tasks 2–4 blocks traceable to design-dark §3.1–3.9 and design-light §1–3. Command-deck order → Task 5 Step 1 note. Glow interface → canonical delta 1. Refresh map → Task 8 Step 4.
2. **Placeholder scan:** no TBD; Task 5 Step 1 shows full DOM skeleton with "…existing … verbatim…" move directives (legal — it says *where from* with id anchors); Task 8 Step 4 table instructs deriving counts with commands (values unknown until build; the *method* is complete).
3. **Type consistency:** `--glow-bri/--glow-hue` (JS) ↔ Task 3 dark aura (`--glow-t` derived) ↔ Task 4 light aura (`--aura-h` derived) ✓; `--fill-pct` (JS + group markup attr) ✓; `applyGlow/writeFill/micCommittedAt` defined once (Task 6) used throughout ✓; `#mic-word`, `.mic-lamp` created Task 5, styled Task 3, updated Task 6 ✓; profile names shared Task 1 ↔ Task 7 ✓.








