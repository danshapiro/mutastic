#!/usr/bin/env python3
"""Generate the Mutastic Lights key icons.

Writes deck/icons/mutastic-light-on.png and mutastic-light-off.png:
144x144 RGB PNG, pure-black background, classic LIGHT BULB glyph.
State 0 (OFF) is a dim gray outline bulb; state 1 (ON) is a warm
bright-filled bulb with short rays.

Antialiased via 4x supersampling: drawn at 576px, downscaled to 144px
with Lanczos resampling.

Run from the repo root:  python3 deck/icons/gen-light-icons.py
"""
from PIL import Image, ImageDraw

SIZE = 144
SS = 4  # supersample factor
S = SIZE * SS
BLACK = (0, 0, 0)
GRAY = (128, 128, 128)
WARM = (255, 228, 138)   # warm lit bulb fill
RAY = (255, 240, 200)    # slightly paler warm rays
OUTLINE_W = 7 * SS       # stroke width for the OFF outline
RAY_W = 8 * SS

def px(*vals):
    """Scale 144-space coordinates into supersampled space."""
    return [v * SS for v in vals]

# Bulb geometry in 144-space:
#   glass globe: circle centered (72, 58), radius 30
#   neck: trapezoid narrowing from globe into the base
#   screw base: two horizontal bars, y 96..114
GLOBE = (42, 28, 102, 88)          # bounding box of the glass circle
NECK = [(60, 82), (84, 82), (80, 98), (64, 98)]
BASE1 = (60, 100, 84, 106)
BASE2 = (62, 109, 82, 115)

RAYS = [  # (x0, y0, x1, y1) in 144-space, radiating from the globe
    (72, 18, 72, 4),      # straight up
    (44, 30, 34, 20),     # up-left
    (100, 30, 110, 20),   # up-right
    (32, 58, 18, 58),     # left
    (112, 58, 126, 58),   # right
]

def draw_bulb(on: bool) -> Image.Image:
    img = Image.new("RGB", (S, S), BLACK)
    d = ImageDraw.Draw(img)
    if on:
        # filled warm bulb
        d.ellipse(px(*GLOBE), fill=WARM)
        d.polygon([tuple(px(x, y)) for x, y in NECK], fill=WARM)
        d.rounded_rectangle(px(*BASE1), radius=2 * SS, fill=GRAY)
        d.rounded_rectangle(px(*BASE2), radius=2 * SS, fill=GRAY)
        for x0, y0, x1, y1 in RAYS:
            d.line(px(x0, y0, x1, y1), fill=RAY, width=RAY_W)
    else:
        # dim gray outline bulb
        d.ellipse(px(*GLOBE), outline=GRAY, width=OUTLINE_W)
        d.line(px(60, 84, 64, 98), fill=GRAY, width=OUTLINE_W)
        d.line(px(84, 84, 80, 98), fill=GRAY, width=OUTLINE_W)
        d.rounded_rectangle(px(*BASE1), radius=2 * SS, outline=GRAY, width=OUTLINE_W // 2)
        d.rounded_rectangle(px(*BASE2), radius=2 * SS, outline=GRAY, width=OUTLINE_W // 2)
    return img.resize((SIZE, SIZE), Image.LANCZOS)

if __name__ == "__main__":
    draw_bulb(True).save("deck/icons/mutastic-light-on.png")
    draw_bulb(False).save("deck/icons/mutastic-light-off.png")
    print("wrote deck/icons/mutastic-light-{on,off}.png (antialiased bulb)")
