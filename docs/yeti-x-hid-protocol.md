# Blue Yeti X — vendor HID control protocol

Source: reverse-engineered by [mhils/yeti-volume-sync](https://github.com/mhils/yeti-volume-sync)
(Rust, ~250 lines; author sniffed Blue Sherpa / Logitech G HUB USB traffic).
Verified against `src/hid.rs` and `src/main.rs` of that repo. This machine's
mic is a **Yeti X, VID 0x046D PID 0x0AAF** (confirmed via Get-PnpDevice).

## Device selection

- Match VID `0x046D`, PID `0x0AAF` | `0x0AD1` | `0x0AC6` (Yeti X variants)
- The mic exposes multiple HID collections; open the one with hidapi
  `usage == 1`. (One other collection is used by G HUB, another is inert.
  Usage *page* is unrecorded — almost certainly vendor-defined `0xFF00+`.)

## Transport

Interrupt **output/input reports** (hidapi `write`/`read`), NOT feature
reports. Report ID `0x01`, 64-byte reports (65-byte hidapi write buffer).

### Outbound (host -> mic)

```
offset:  0    1  2  3    4       5  6  7    8        9...
value:  0x01  00 00 00  <op>    00 00 00  8+len    ASCII payload, zero-padded
```

Payload is an **ASCII decimal string** (e.g. volume 50 = bytes `35 30`).
Length byte at offset 8 is header-inclusive: `8 + payload_len`.

Opcodes (outbound):

| Op | Meaning |
|----|---------|
| `0x01` | GetVolume |
| `0x05` | Unknown init/subscribe (see handshake) |
| `0x08` | SetPattern ("0"=stereo, "1"=omni, "2"=cardioid, "3"=bidirectional) |
| `0x14` | SetBlend |
| `0x17` | SetGain |
| `0x20` | **Mute** (ASCII payload: "1"=muted, "0"=unmuted — CONFIRMED, see open questions) |
| `0x23` | SetVolume |

Example — mute on (confirmed 2026-08-08): `01 00 00 00 20 00 00 00 09 31 00 ... 00`

### Inbound (mic -> host)

```
offset:  0    1    2  3    4      5 6 7 8    9
value:  0x01 0x80  00 00  <evt>   ...        <value byte>
```

Byte 1 `0x80` = response/notification flag. Value read at offset 9 as a
raw byte in the reference implementation (asymmetric with ASCII outbound).
CAUTION: on this machine's firmware (PID 0AAF) the `0x20` and `0x01` echoes
declare zero-length payloads (`0x08` at offset 8) and offset 9 holds a
constant `0x0b` tag — not state (observed
`01 80 00 00 20 00 00 00 08 0b 03 00 ... 00`; see open questions).

Events come in (software, device) pairs — even code = echo of host-initiated
change, odd code = physical control touched on the mic:

| Evt | Meaning |
|-----|---------|
| `0x01`/`0x24` | DeviceVolume |
| `0x08`/`0x12` | Pattern (old/new) |
| `0x14`/`0x15` | SoftwareBlend / DeviceBlend |
| `0x17`/`0x18` | SoftwareGain / DeviceGain |
| `0x20`/`0x21` | **SoftwareMute / DeviceMute** (physical mute-button press) |
| `0x23` | SoftwareVolume |

## Mandatory init handshake

Send op `0x05` (empty payload), then op `0x01` GetVolume, before the mic
responds or emits unsolicited events. Reference implementation notes this is
"somewhat flaky, but that's also the case with the Blue Sherpa software" —
retry on silence.

## Open questions (resolve empirically, then update this doc)

All four resolved on this machine's Yeti X (VID 046D, PID 0AAF, MI_03):

1. Mute payload — **CONFIRMED (2026-08-08, mutastic live test).** ASCII
   `"1"` = muted, `"0"` = unmuted, sent as op `0x20` on the `0xFFFF/0x0001`
   collection. Ground truth: a human watched the mic while
   `mutastic mute`/`mutastic unmute` ran from the deployed exe — the hardware
   mute LED tracked the commands (muted on `mute`, unmuted on `unmute`,
   blinking during an alternating loop). Polarity is normal, not inverted.
2. Inbound value at offset 9 — **CONFIRMED (2026-08-08, mutastic live test):
   NEITHER binary nor ASCII — the `0x20` echo carries no state at all.** Raw
   dumps of the mute and unmute echoes are byte-for-byte identical:
   `01 80 00 00 20 00 00 00 08 0b 03 00 ... 00`. The length byte `0x08`
   (header-inclusive) declares a zero-length payload; the `0b 03` at offsets
   9–10 is a constant tag present in every event type (likely a firmware
   version tag, `11.3` — the startup `0x05` echo's length byte `0x0a`
   actually claims those two bytes as payload). The echo is a bare
   acknowledgment reflecting neither the new nor the previous state; state
   readback via the echo is impossible on this firmware, so hosts must track
   mute state optimistically (and watch `0x21` DeviceMute for the physical
   button).
3. Mute LED follows software-set mute — **CONFIRMED (2026-08-08, human
   observation during mutastic live test).** See question 1: the LED tracked
   software `mute`/`unmute` commands.
4. HID usage_page — **CONFIRMED (2026-08-08, mutastic live test).** The
   control collection is `usage_page=0xFFFF, usage=0x0001` (MI_03 Col04).
   Its three MI_03 siblings are `0xFF43` (usages `0x0701`, `0x0702`,
   `0x0704`) and all three reject 65-byte output reports at the transport
   level (`WriteFile: (0x00000001) Incorrect function.`) — `0xFFFF/0x0001`
   is the only collection that accepts the vendor protocol.

Remaining caveat (new, discovered 2026-08-08): on this firmware even the
GetVolume (`0x01`) response declares a zero-length payload (`08` at offset 8),
so the reference implementation's "value at offset 9" inbound convention may
not hold for PID 0x0AAF at all — a reader there sees only the constant `0x0b`
tag byte. Whether `0x21` DeviceMute events carry a real value byte is still
unverified (needs a human pressing the mic's button while a daemon logs raw
reports).

## Practical notes

- Do NOT usbipd-attach the mic to WSL for testing — it steals the active
  system microphone from Windows. Run test binaries on the Windows side.
- G HUB (if running) uses a different HID collection; coexistence is fine.
