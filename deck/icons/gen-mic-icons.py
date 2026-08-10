#!/usr/bin/env python3
"""Generate the Mutastic Mute key icons.

Writes deck/icons/mutastic-mic.png (state 0, live) and
mutastic-mic-muted.png (state 1, muted): 144x144 RGB PNG, pure-black
background. Live = white mic glyph; muted = same mic with a red
circle-slash overlay.

Antialiased via 4x supersampling: drawn at 576px, downscaled to 144px
with Lanczos resampling.

Run from the repo root:  python3 deck/icons/gen-mic-icons.py
"""
from PIL import Image, ImageDraw

SIZE = 144
SS = 4
S = SIZE * SS
BLACK = (0, 0, 0)
WHITE = (255, 255, 255)
RED = (229, 57, 53)
LW = 9 * SS       # stroke width for arc/stem/base
SLASH_W = 11 * SS

def px(*vals):
    return [v * SS for v in vals]

def mic_base(muted: bool) -> Image.Image:
    img = Image.new("RGB", (S, S), BLACK)
    d = ImageDraw.Draw(img)
    # capsule (mic body)
    d.rounded_rectangle(px(58, 26, 86, 78), radius=14 * SS, fill=WHITE)
    # cradle arc (lower half circle)
    d.arc(px(44, 34, 100, 90), start=0, end=180, fill=WHITE, width=LW)
    # stem + base
    d.line(px(72, 90, 72, 106), fill=WHITE, width=LW)
    d.line(px(54, 108, 90, 108), fill=WHITE, width=LW)
    if muted:
        d.ellipse(px(14, 14, 130, 130), outline=RED, width=SLASH_W)
        d.line(px(33, 33, 111, 111), fill=RED, width=SLASH_W)
    return img.resize((SIZE, SIZE), Image.LANCZOS)

if __name__ == "__main__":
    mic_base(False).save("deck/icons/mutastic-mic.png")
    mic_base(True).save("deck/icons/mutastic-mic-muted.png")
    print("wrote deck/icons/mutastic-mic{,-muted}.png (antialiased)")
