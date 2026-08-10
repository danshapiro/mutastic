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

Run from the repo root:  python3 deck/icons/gen-mdi-icons.py
Requires: rsvg-convert (librsvg2-bin), Pillow.
"""
import subprocess
from io import BytesIO
from PIL import Image

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
