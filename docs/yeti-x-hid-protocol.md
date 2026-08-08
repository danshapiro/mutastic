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
| `0x20` | **Mute** (payload "0"/"1" — see open questions) |
| `0x23` | SetVolume |

Example — mute on (inferred): `01 00 00 00 20 00 00 00 09 31 00 ... 00`

### Inbound (mic -> host)

```
offset:  0    1    2  3    4      5 6 7 8    9
value:  0x01 0x80  00 00  <evt>   ...        <value byte>
```

Byte 1 `0x80` = response/notification flag. Value read at offset 9 as a
raw byte in the reference implementation (asymmetric with ASCII outbound —
see open questions).

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

1. Mute payload: `"1"` = muted / `"0"` = unmuted is inferred from the other
   opcodes' conventions; reference code only ever sends `"0"`.
2. Inbound value at offset 9: binary byte vs ASCII digit — verify via the
   SoftwareMute echo after setting mute.
3. Whether the mic's mute LED follows software-set mute (firmware-driven, so
   very likely — but needs a human looking at the mic).
4. HID usage_page of the control collection (dump descriptor when convenient).

## Practical notes

- Do NOT usbipd-attach the mic to WSL for testing — it steals the active
  system microphone from Windows. Run test binaries on the Windows side.
- G HUB (if running) uses a different HID collection; coexistence is fine.
