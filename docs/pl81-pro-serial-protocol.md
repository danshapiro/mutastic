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
- **115200 baud, 8N1**
- Open sequence used by working implementations: open, reset buffers, write
  wake bytes `00 00 00 00`, sleep ~80-120ms. Insert ~60ms delay before each
  write; the device streams status at 60-80ms intervals and is rate-sensitive.

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
