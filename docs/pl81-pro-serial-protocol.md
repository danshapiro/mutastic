# NEEWER PL81 PRO — USB serial control protocol

Sources: [m-rk/neewer-usb-control](https://github.com/m-rk/neewer-usb-control)
(RESEARCH.md — disassembled NEEWER Control Center on macOS, calibrated on a
PL81-Pro) and [Rokkit-exe/neewerctl](https://github.com/Rokkit-exe/neewerctl)
(Go, tested on a PL81 non-Pro). Cross-checked and LIVE-PROBED on this
machine's unit on 2026-08-08 (see "Local probe results" below).

## Transport (VERIFIED locally)

- The light is a CH340 USB-to-serial bridge: **VID `0x1A86`, PID `0x7523`**
- On this machine it enumerates as `USB-SERIAL CH340 (COM4)` (driver already
  present; port status OK). Do NOT hardcode COM4 — enumerate by VID/PID
  (go.bug.st/serial/enumerator).
- With multiple panels, VID/PID no longer discriminates and the CH340
  exposes **no USB serial number** — measured, not folklore: this unit's
  Windows instance ID is a machine-generated location ID
  (`8&39d912e3&0&3`, verified 2026-08-09 — no iSerialNumber, so
  `SerialNumber` is always empty via go.bug.st). The **COM port name is
  the light's identity** (this is what
  `light name`/`light-state-<COMx>.json` key on). Windows keys the COM
  assignment to the physical-jack location path, and ComDB never recycles
  released COM numbers — an unplugged device keeps its number reserved.
  Moving a panel to a different jack gives it a new COM number, i.e. a
  new identity (re-run `light name`).
- **115200 baud, 8N1**
- Open sequence used by working implementations: open, reset buffers, write
  wake bytes `00 00 00 00`, sleep ~80-120ms. Insert ~60ms delay before each
  write; idle connections are silent; unprompted frames occur only while a
  physical control is being adjusted (repeating at ~60–80 ms on the non-Pro
  during adjustment).

## Frame format (VERIFIED locally)

```
[0x3A] [tag] [payload_len] [payload...] [cs_hi] [cs_lo]
```

- Prefix `0x3A` (BLE uses 0x78, WiFi 0x80 — different transports, different
  tags; do NOT port BLE command tables)
- Checksum: **16-bit big-endian sum** of all preceding bytes

### CCT command — tag 0x02 (the only functional control on this model)

```
3A 02 03 <pwr> <brightness 0x00-0x64> <temp> <cs_hi> <cs_lo>
```

- `pwr`: `0x01` on (`0x02` = off per app disassembly, but see Power below)
- `brightness`: 0-100 decimal
- `temp`: color-temperature byte, see encoding below

### Power

Tag `0x06` power frames (`3A 06 01 01 00 42` / `3A 06 01 02 00 43`) are
accepted but DO NOTHING on the PL81 Pro (verified by m-rk). **Implement off
as brightness 0**; restore saved brightness for on. (Both working projects
do this.)

### HSI/RGB (tag 0x04) and scenes

Not supported — the PL81 Pro is bi-color only. NEEWER-app "presets" are
host-side (brightness, temp) pairs:

| Preset | Brightness | Temp |
|--------|-----------:|------|
| cold | 100% | 7000K |
| sunlight | 28% | 5600K |
| afternoon | 16% | 5000K |
| sunset | 16% | 4500K |
| candle | 28% | 3400K |

### Echo / status (VERIFIED locally)

The light echoes every accepted command byte-for-byte as an ACK, and
broadcasts unprompted 8-byte status frames (same format) when the physical
knob is touched — parse the read stream to track knob-driven state. There is
no known query command; state is learned from echoes/broadcasts only.

## Temperature encoding — CONFLICT, default to m-rk

- **m-rk (PL81-Pro, empirically calibrated)**: 19 steps `0x00`-`0x12`;
  `byte = round((K - 2900) * 18 / 4100)`; firmware clamps at 0x12 (bytes
  >= 0x13 render identically). USE THIS.
- Rokkit-exe (PL81 non-Pro): 41 steps `0x01`-`0x29` — incompatible; would
  saturate above ~4650K if m-rk's calibration holds. Do not use.
- TODO (human eyes needed): sweep 0x00->0x14 at fixed brightness on our unit
  to confirm where visible change stops.

## Local probe results (2026-08-08, COM4, PowerShell SerialPort)

```
sent: 3A 02 03 01 64 09 00 AD   (on, 100%, temp 0x09 ~4950K)
echo: 3A 02 03 01 64 09 00 AD   <- byte-for-byte ACK, light turned on
sent: 3A 02 03 01 00 09 00 49   (brightness 0 = off)
echo: 3A 02 03 01 00 09 00 49   <- ACK, light turned off
```

## Practical notes

- Whoever holds the COM port holds it exclusively — the daemon and NEEWER
  Control Center cannot run at the same time.
- The mic (Yeti X) is HID; the light is serial — independent device loops.
- NEEWER Control Center for Windows exists (v3.5.1) but is unneeded here.

## Multiple panels

- The daemon enumerates ALL VID 1A86 / PID 7523 ports and runs one
  independent session per port (own state tracker, reconnect loop, and
  60 ms rate-limited writes). A rescan every 5 s starts sessions for
  newly plugged-in panels; a session is torn down only after its port is
  missing from 2 CONSECUTIVE successful scans (debounce — a transient
  enumeration omission or power blip never kills a session). No daemon
  restart needed.
- Identity = COM port name (see Transport). User-facing names map to
  ports in `%LOCALAPPDATA%\mutastic\light-names.json`; each panel's last
  look persists in `light-state-<COMx>.json`.
- Collective toggle: if ANY panel is on, all turn off; otherwise all turn
  on, each restoring its own persisted look (unknown state counts as
  off). Bare `light` commands fan out to all panels in PARALLEL (each
  panel keeps its own ~60 ms write spacing); every per-panel call is
  deadline-bounded, so one wedged panel yields `error: timeout` on its
  reply line instead of stalling the fleet (or the mic commands).
- Power: the panel is USB BUS-POWERED — 5 V / 2 A input, 5 W, no
  battery; a single Type-C port carries BOTH power and PC control. An
  under-powered port makes the panel automatically limit its brightness
  range (documented device behavior), and a port power reset drops its
  COM port (which looks like port-gone/rescan churn). Three panels can
  draw up to ~3 A total at 5 V — prefer directly-attached or
  self-powered hub ports (the current light sits behind a two-tier hub
  chain).

## Daemon integration results (2026-08-08)

- Echo-as-ACK confirmed end-to-end from the Go daemon via go.bug.st/serial:
  every CCT frame written by `mutastic light on|off|...` came back
  byte-for-byte and was logged by the read loop (`light: frame ...` lines
  in `%LOCALAPPDATA%\mutastic\mutastic.log`).
- OFF-as-brightness-0 re-confirmed through the daemon (light off,
  echo `pwr=0x01 brightness=0`).
- Knob broadcasts were NOT exercised (no human at the desk during the
  automated run); their exact format remains uncaptured — see human
  follow-ups.

## Recorded human questions (follow-ups needing eyes/feet)

1. **Temperature-sweep calibration:** with the daemon running, sweep
   `mutastic light temp 2900` → `7000` in ~228 K steps at fixed brightness
   and watch where visible change stops, to confirm the 19-step clamp
   (byte 0x12) on this unit. (Pre-existing TODO, still open.)
2. **Real pedal press:** press the LEFT pedal (F13) and confirm the light
   toggles; confirm F14 deliberately does nothing and F15 (Winpepper) still
   behaves. Separately confirm the physical Yeti button/F24 path and Stream
   Deck mute action still perform the active full-mute flow.
3. **Knob broadcast + panel-off capture:** touch the physical knob while
   the daemon runs, then check the log for `light: frame` lines to finally
   capture a broadcast transcript (expected: CCT-shaped 8-byte frames).
   Also turn the panel off/on with its own physical control and check what
   the log records — this settles whether the pwr byte carries off-state
   (`0x00`/`0x02`), which the daemon already tolerates defensively.
4. **Unplug/replug:** with the daemon running, unplug the light's USB
   cable; confirm the log shows `light: session ended` within ~15 s (read
   error or the presence check), then replug and confirm `light: port
   opened` returns. This settles the CH340 surprise-removal behavior,
   which was validated only at source level.
5. **Long-idle re-sleep check:** after the daemon has been connected and
   idle for some hours, press F13 (or run `mutastic light on`) and confirm
   the light actually responds — settles whether wake-once-per-session
   suffices.
6. When the two additional PL81 PRO panels arrive: plug each in, confirm
   the daemon discovers it within ~5 s (`light list` gains a row; log
   shows `light COM<n>: starting session`), name them, confirm which COM
   maps to which physical light, and verify the mapping survives a
   replug into the SAME USB jack. Also verify clean teardown on unplug
   (`light COM<n>: port gone, stopping session` — appears after 2
   consecutive rescan misses, ~10 s) — untestable today with only the
   single live light in use.
7. Power bring-up with all three panels attached: verify each reaches its
   full target brightness (brightness capping is the documented
   under-power symptom) and that the port set stays stable in
   `light list`/the log (no port-gone/rescan churn). If capping or churn
   appears, move panels to directly-attached or self-powered hub ports.
