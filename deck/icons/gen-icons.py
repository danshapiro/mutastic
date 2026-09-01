#!/usr/bin/env python3
"""Rasterize the brand + vendored MDI SVG sources into every mutastic raster icon.

Sources:
  brand/mutastic-icon.svg               the brand mark (rounded dark badge with the
                                        colorful M-waveform + mic, drawn for dark surfaces)
  deck/icons/svg/mdi-lightbulb-on.svg   vendored Material Design Icons (Pictogrammers,
  deck/icons/svg/mdi-lightbulb-off.svg  Apache-2.0), pre-colored, used for the light keys

Outputs (all checked into git; consumers reference them by fixed name):
  deck/icons/mutastic-mic.png        144x144 saturated badge on opaque black (Deck LIVE)
  deck/icons/mutastic-mic-muted.png  144x144 DESATURATED badge on opaque black (Deck MUTED)
  deck/icons/mutastic-light-on.png   144x144 warm MDI bulb on opaque black
  deck/icons/mutastic-light-off.png  144x144 white MDI bulb on opaque black
  deck/icons/tray-mic.ico            saturated badge, transparent, per-size haloed frames
                                     (Windows tray UNMUTED)
  deck/icons/tray-mic-muted.ico      DESATURATED badge frames (Windows tray MUTED)
  deck/icons/tray-mic-unknown.ico    desaturated badge frames dimmed to ~55% alpha
                                     (tray startup / state unknown; must not read as live)

Tray color scheme (2026-08): saturated = unmuted, desaturated = muted.
The muted/unknown variants are the SAME art run through grayscale, so the
badge shape stays recognizable in the notification area. Deck key icons
composite onto PURE BLACK (deck keys are black-bezel); tray icons keep
transparency so no black square shows in the notification area, with a
thin dark halo per frame so the badge edge survives light taskbar themes.

The web favicon set (internal/lightui/favicon*) is shipped art from the
same brand package and is not generated here.

Run from the repo root:  python3 deck/icons/gen-icons.py
Requires: rsvg-convert (librsvg2-bin), Pillow.
"""
import subprocess
from io import BytesIO

from PIL import Image, ImageFilter, ImageOps

BRAND = "brand/mutastic-icon.svg"
SIZE = 144
TRAY_SIZES = [16, 24, 32, 48, 64, 128, 256]
UNKNOWN_DIM = 0.55


def render(svg, size):
    """Rasterize an SVG at exactly size x size, keeping alpha."""
    png = subprocess.run(
        ["rsvg-convert", "-w", str(size), "-h", str(size), svg],
        check=True,
        capture_output=True,
    ).stdout
    return Image.open(BytesIO(png)).convert("RGBA")


def desaturate(img):
    """Grayscale an RGBA image, preserving the alpha channel."""
    gray = ImageOps.grayscale(img)
    return Image.merge("RGBA", (gray, gray, gray, img.getchannel("A")))


def dim(img, factor):
    """Scale alpha by factor (0..1), leaving colors alone."""
    out = img.copy()
    out.putalpha(img.getchannel("A").point(lambda a: int(a * factor)))
    return out


# --- Stream Deck / OpenDeck key icons: opaque PURE BLACK background ---
DECK_JOBS = [
    (BRAND, False, "deck/icons/mutastic-mic.png"),
    (BRAND, True, "deck/icons/mutastic-mic-muted.png"),
    ("deck/icons/svg/mdi-lightbulb-on.svg", False, "deck/icons/mutastic-light-on.png"),
    ("deck/icons/svg/mdi-lightbulb-off.svg", False, "deck/icons/mutastic-light-off.png"),
]

for svg, gray, out in DECK_JOBS:
    art = render(svg, SIZE)
    if gray:
        art = desaturate(art)
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 255))
    img.alpha_composite(art)
    img.convert("RGB").save(out)
    print("wrote", out)

# --- Windows tray icons: transparent, one explicitly-rendered frame per size ---
TRAY_JOBS = [
    (False, 1.0, "deck/icons/tray-mic.ico"),
    (True, 1.0, "deck/icons/tray-mic-muted.ico"),
    (True, UNKNOWN_DIM, "deck/icons/tray-mic-unknown.ico"),
]

for gray, alpha, out in TRAY_JOBS:
    frames = []
    for s in TRAY_SIZES:
        badge = render(BRAND, s)
        if gray:
            badge = desaturate(badge)
        if alpha != 1.0:
            badge = dim(badge, alpha)
        # Thin dark halo under the badge so its edge stays legible on any
        # taskbar theme; dilation is per-frame (MaxFilter(3) at <=32px,
        # MaxFilter(5) above) so it survives at real taskbar sizes.
        halo = Image.new("RGBA", badge.size, (24, 24, 24, 255))
        halo.putalpha(badge.getchannel("A").filter(ImageFilter.MaxFilter(3 if s <= 32 else 5)))
        frames.append(Image.alpha_composite(halo, badge))
    frames[-1].save(out, format="ICO", append_images=frames[:-1])
    print("wrote", out)
