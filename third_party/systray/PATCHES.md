# Vendored systray patches

This directory is a copy of `github.com/energye/systray` at **v1.0.3**
(pulled from the Go module cache; `LICENSE` and `go.mod` unchanged, all
platform files intact), referenced from the mutastic root module via

    replace github.com/energye/systray => ./third_party/systray

Imports stay `github.com/energye/systray` — nothing outside this
directory changes at call sites.

## Divergences from upstream v1.0.3

Exactly one change class: **synchronized native-dispatch callback
bindings** (delta review round 8, R8-F4).

Upstream stored every user callback reachable from a native dispatch
loop in a bare `func` field, written by a public/setter method and read
by the dispatch side with no synchronization. mutastic (re)binds menu
item handlers AFTER the message pump is already dispatching (the tray's
"Saved settings" rows are created and rebound while the Win32 loop
runs), so an unsynchronized field is a formal data race: the pump can
observe a half-written or stale binding at the moment it dispatches a
click.

Sites changed (all read/write sites for each field, every platform):

- `systray.go` — `MenuItem.click` (the column the delta review flagged:
  written by `MenuItem.Click`, read by the pump dispatch
  `systrayMenuItemSelected`, which the Windows `WM_COMMAND` handler, the
  darwin `systray_menu_item_selected` export, and the unix dbus menu
  handler all funnel through) becomes `atomic.Pointer[func()]`.
  `Click` stores the binding atomically; a paired unexported getter
  `(*MenuItem).getClick()` loads it, and `systrayMenuItemSelected` calls
  through the getter (a never-bound item still no-ops, exactly as a nil
  `click` did before).
- `systray_windows.go` — `winTray.onClick` / `onDClick` / `onRClick`
  (written by `setOnClick`/`setOnDClick`/`setOnRClick`, which callers may
  invoke after the pump is running; read by the pump in
  `(*winTray).wndProc` on `WM_LBUTTONUP` / `WM_LBUTTONDBLCLK` /
  `WM_RBUTTONUP`) become `atomic.Pointer[func(menu IMenu)]`, with the
  setters storing atomically and the reader sites going through paired
  getters `getOnClick` / `getOnDClick` / `getOnRClick`.
- `systray_darwin.go` — package-level `onClick` / `onDClick` / `onRClick`
  (written by the exported `SetOnClick`/`SetOnDClick`/`SetOnRClick` path,
  read by the cgo-exported `systray_on_click` / `systray_on_rclick`
  callbacks) become `atomic.Pointer[func(menu IMenu)]`, with atomic
  stores in the setters and paired getters `getOnClick` / `getOnDClick` /
  `getOnRClick` at the read sites.
- `systray_unix.go` — `UnimplementedStatusNotifierItem.activate` /
  `dActivate` (written by `setOnClick` / `setOnDClick`, read by the
  dbus-exported `Activate` handler) become
  `atomic.Pointer[func(x int32, y int32)]`, with atomic stores in the
  setters (the wrapping closure is unchanged) and atomic loads at both
  read sites in `Activate`.

Fields deliberately NOT changed:

- `title`, `tooltip`, `disabled`, `checked` and friends on `MenuItem`:
  the dispatch loops never read them (Windows menu text/state is written
  and read through winapi calls; unix/darwin render through their native
  toolkits from the caller goroutine), so they do not share the
  write-vs-pump-read shape.
- `UnimplementedStatusNotifierItem.contextMenu` / `secondaryActivate` /
  `scroll`: no setter ever writes them in this codebase, so they have no
  post-pump write at all.
- `darwin dClickTime` / unix `dActivateTime`: plain `int64` bookkeeping
  read and written only on the dispatch goroutine of its own platform
  (the setter side never touches them), so no cross-goroutine field
  exists here to flag.

No other semantic divergence. Additionally, the tree was passed through
`gofmt -w` so the parent repo's "gofmt silent" gate holds: a zero-behavior,
comment/whitespace-only normalization applied to `systray.go`'s Chinese
doc comments and `internal/generated/notifier/status_notifier_item.go`'s
generated doc comments (upstream predates the gofmt version that formats
these). Likewise `systray_darwin.m` was stripped of upstream's one
trailing-whitespace line (a two-space blank line inside
`addOrUpdateMenuItem`) so the parent repo's `git diff --check` gate stays
clean (delta review round 12, R12-F2): a whitespace-only normalization,
no behavior change.

### Why not upstream-shaped timing discipline instead

Timing around the race (bind-before-reveal rules in the tray glue) left
the formal race class open for every future caller. The atomic field
removes the class at the root while keeping upstream's public API and
behavior identical for every caller that binds before dispatch begins.
