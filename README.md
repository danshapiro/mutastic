# ✨ mutastic

**Your whole meeting desk, in one place.** One press mutes the microphone and
your meeting apps. One glance tells you whether you're live. Name your favorite
lighting once — one click brings it back whenever you want it.

## 🎙️ Mute everything at once

Tap the mute button on the microphone, a Stream Deck key, or the tray menu —
the mic and your meeting apps move together in a single press. Teams, Zoom,
Webex, and Meet all follow your mic; your whole desk flips at once.

## 👀 Always know your state

A tray icon shows the truth at a glance: bright when you're live, red when
you're muted. The menu's mic action tells you exactly what a click will do —
it reads **Mute** while you're live and **Unmute** while you're muted — and a
click does exactly what it says. Stream Deck keys mirror the same live state,
so your desk agrees with itself everywhere you look.

## 💡 Run your desk lights

Dial brightness and warmth across every panel at once or each light by name,
jump straight to a mood with presets like *sunlight* or *candle*, or flick the
whole fleet on and off together. Panels you plug in later join the family on
their own, and every light remembers its favorite look across reboots. Give a
panel a name and talk to it directly:

```
mutastic light@\"left key\" brightness 60
```

Lights at **28% / 3356 K** for a call? Save it:

```
mutastic light settings save "podcast"
```

…and later, from the panel, the tray, or the command line:

```
mutastic light settings apply "podcast"
```

## 💾 Save your favorite looks

Name any lighting moment — *podcast*, *movie mode*, *late night* — and keep up
to 100 of them, named however you like: spaces, accents, CJK, and emoji
welcome. The web panel and the tray menu share the same saved collection, so a
look you save in the browser is already waiting in the tray.

## 🌐 A friendly web panel

Left-click the tray icon and a clean page opens: a card per light with sliders
and presets, a **Mic** card with Mute / Unmute / Toggle, and the **Saved
settings** collection. It starts with Windows and waits quietly in your
browser until you want it.

## 🎮 Stream Deck, command line, scripts

Two native OpenDeck key actions — one that toggles mute everywhere with a true
mic-state icon, one that toggles the lights with an any-light-on icon. And the
panel and the tray are front ends for `mutastic` commands: anything you can
click, you can script — set states absolutely, nudge brightness or warmth in
steps (perfect for pedals and dials), read status back from a shell, or snap a
still frame from any OBS source:

```
mutastic obs snapshot --out desk.png
```

## ▶️ Quick start

Built for **Windows**. This is exactly how this desk is wired — clone the
repo, then:

```bash
./build.sh             # builds the Windows binary (from WSL, with the mingw toolchain)
```

```cmd
deploy\deploy.cmd      # installs, wires autostart, and launches everything
                       # (AutoHotkey v1.1 and OpenDeck installed on the Windows side)
```

That's it — the tray icon appears, the panel answers in your browser, and
everything comes back on its own at every login.

## 🧩 Made for

- **Windows** 10/11 — starts at login, lives in the notification area
- A **Logitech Blue Yeti X** microphone
- **NEEWER PL81 PRO** video-light panels (one or many)
- **Microsoft Teams, Zoom, Webex, and Google Meet**
- A **Stream Deck**, via **OpenDeck**
- **OBS Studio** — still-frame capture from any source
- **AutoHotkey** and your favorite scripting

Curious how the hardware magic works? The deep dives live in
[`docs/`](docs/), and [`AGENTS.md`](AGENTS.md) orients contributors and coding
agents.
