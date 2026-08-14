#!/usr/bin/env python3
"""Rasterize the vendored MDI SVGs into the mutastic deck key icons.

Source glyphs: Material Design Icons (Pictogrammers, Apache-2.0), vendored
as pre-colored SVGs in deck/icons/svg/ (fetched once from api.iconify.design
with ?color=...). Rendering: rsvg-convert rasterizes each SVG at 144x144
onto transparency; PIL composites onto a PURE BLACK background.

Colors baked into the vendored SVGs:
  mdi-microphone      #FFFFFF  (live mic)
  mdi-microphone-off  #E53935  (muted - red, matches previous design language)
  mdi-lightbulb-on    #FFE48A  (warm lit bulb with rays)
  mdi-lightbulb-off   #FFFFFF  (pure white, per user preference)

Also emits the three Windows tray icons (tray-mic.ico white live,
tray-mic-muted.ico red, tray-mic-unknown.ico gray): same glyphs on
TRANSPARENT backgrounds (a black composite would render as a black square
in the notification area), one explicitly-rendered frame per size
(16-256px), each with a thin dark halo so the glyph reads on any taskbar
theme.

Run from the repo root:  python3 deck/icons/gen-mdi-icons.py
Requires: rsvg-convert (librsvg2-bin), Pillow.
"""
import subprocess
from io import BytesIO
from PIL import Image, ImageFilter

SIZE = 144
JOBS = [
    ("deck/icons/svg/mdi-microphone.svg",     "deck/icons/mutastic-mic.png"),
    ("deck/icons/svg/mdi-microphone-off.svg", "deck/icons/mutastic-mic-muted.png"),
    ("deck/icons/svg/mdi-lightbulb-on.svg",   "deck/icons/mutastic-light-on.png"),
    ("deck/icons/svg/mdi-lightbulb-off.svg",  "deck/icons/mutastic-light-off.png"),
]

for svg, out in JOBS:
    png = subprocess.run(
        ["rsvg-convert", "-w", str(SIZE), "-h", str(SIZE), svg],
        check=True, capture_output=True).stdout
    glyph = Image.open(BytesIO(png)).convert("RGBA")
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 255))
    img.alpha_composite(glyph)
    img.convert("RGB").save(out)
    print("wrote", out)

TRAY_SIZES = [16, 24, 32, 48, 64, 128, 256]
TRAY_JOBS = [
    ("deck/icons/svg/mdi-microphone.svg",     None,       "deck/icons/tray-mic.ico"),
    ("deck/icons/svg/mdi-microphone-off.svg", None,       "deck/icons/tray-mic-muted.ico"),
    ("deck/icons/svg/mdi-microphone.svg",     "#9E9E9E",  "deck/icons/tray-mic-unknown.ico"),
]

for svg, recolor, out in TRAY_JOBS:
    frames = []
    for s in TRAY_SIZES:
        png = subprocess.run(
            ["rsvg-convert", "-w", str(s), "-h", str(s), svg],
            check=True, capture_output=True).stdout
        glyph = Image.open(BytesIO(png)).convert("RGBA")  # keep alpha: tray icons sit on the taskbar
        if recolor:
            solid = Image.new("RGBA", glyph.size, recolor)
            solid.putalpha(glyph.getchannel("A"))
            glyph = solid
        # Thin dark halo under the glyph so the glyph stays legible on any
        # taskbar theme; dilation is per-frame (MaxFilter(3) at <=32px,
        # MaxFilter(5) above) so it survives at real taskbar sizes.
        halo = Image.new("RGBA", glyph.size, (24, 24, 24, 255))
        halo.putalpha(glyph.getchannel("A").filter(ImageFilter.MaxFilter(3 if s <= 32 else 5)))
        frames.append(Image.alpha_composite(halo, glyph))
    # One explicitly-rendered frame per size (no resampling in the encoder).
    frames[-1].save(out, format="ICO", append_images=frames[:-1])
    print("wrote", out)
