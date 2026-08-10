#!/usr/bin/env python3
"""Generate the Mutastic Lights key icons.

Writes deck/icons/mutastic-light-on.png and mutastic-light-off.png,
matching the mic icons' visual language: 144x144 RGB PNG, pure-black
background, bold hard-edged glyph, few flat colours. State 0 (OFF) is a
dim gray OUTLINE of a desk light panel; state 1 (ON) is the panel filled
bright warm with short rays.

Run from the repo root:  python3 deck/icons/gen-light-icons.py
"""
from PIL import Image, ImageDraw

SIZE = 144
BLACK = (0, 0, 0)
GRAY = (128, 128, 128)
WHITE = (255, 255, 255)
WARM = (255, 228, 138)  # warm lit panel fill
RAY = (255, 240, 200)   # slightly paler warm rays

# Panel geometry (sits in the mic glyph's footprint: x 44..100, y 26..112)
PANEL = (38, 44, 106, 92)  # rounded-rect light panel
RADIUS = 10
STEM = (68, 92, 76, 104)   # stand stem below the panel
BASE = (52, 104, 92, 112)  # stand base bar

RAYS = [  # short rays for the ON state: (x0, y0, x1, y1), width 8
    (72, 34, 72, 16),    # straight up
    (46, 38, 32, 24),    # up-left diagonal
    (98, 38, 112, 24),   # up-right diagonal
    (28, 68, 12, 68),    # left
    (116, 68, 132, 68),  # right
]


def off_icon() -> Image.Image:
    img = Image.new("RGB", (SIZE, SIZE), BLACK)
    d = ImageDraw.Draw(img)
    # Dim gray OUTLINE only: the panel is dark.
    d.rounded_rectangle(PANEL, radius=RADIUS, outline=GRAY, width=8)
    d.rectangle(STEM, fill=GRAY)
    d.rectangle(BASE, fill=GRAY)
    return img


def on_icon() -> Image.Image:
    img = Image.new("RGB", (SIZE, SIZE), BLACK)
    d = ImageDraw.Draw(img)
    for x0, y0, x1, y1 in RAYS:
        d.line((x0, y0, x1, y1), fill=RAY, width=8)
    # Bright warm-lit panel with a white frame.
    d.rounded_rectangle(PANEL, radius=RADIUS, fill=WARM, outline=WHITE, width=6)
    d.rectangle(STEM, fill=WHITE)
    d.rectangle(BASE, fill=WHITE)
    return img


def main() -> None:
    off_icon().save("deck/icons/mutastic-light-off.png")
    on_icon().save("deck/icons/mutastic-light-on.png")
    print("wrote deck/icons/mutastic-light-off.png and mutastic-light-on.png")


if __name__ == "__main__":
    main()
