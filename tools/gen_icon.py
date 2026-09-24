#!/usr/bin/env python3
# 生成 PWA 图标：深蓝圆角方块 + 白色“车”字
from PIL import Image, ImageDraw, ImageFont

FONT = "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc"

def make_icon(size, out):
    img = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    # 圆角背景：深蓝渐变效果（用垂直渐变模拟）
    top = (26, 35, 126)   # 深蓝
    bottom = (13, 71, 161)
    step = size // 24
    for i in range(step):
        t = i / max(step - 1, 1)
        c = tuple(int(top[j] + (bottom[j] - top[j]) * t) for j in range(3))
        d.rectangle([0, i * (size / step), size, (i + 1) * (size / step) + 1], fill=c + (255,))
    # 圆角遮罩
    mask = Image.new("L", (size, size), 0)
    md = ImageDraw.Draw(mask)
    r = size * 0.22
    md.rounded_rectangle([0, 0, size - 1, size - 1], radius=r, fill=255)
    img.putalpha(mask)
    # 白色“车”字
    font = ImageFont.truetype(FONT, int(size * 0.56))
    text = "车"
    bbox = d.textbbox((0, 0), text, font=font)
    tw, th = bbox[2] - bbox[0], bbox[3] - bbox[1]
    x = (size - tw) / 2 - bbox[0]
    y = (size - th) / 2 - bbox[1] - size * 0.02
    d.text((x, y), text, font=font, fill=(255, 255, 255, 255))
    img.save(out, "PNG")
    print("saved", out, size)

base = "/home/user/Doubao/chats/38421063805886978/car-billing/templates/pwa/icons"
make_icon(512, base + "/icon-512.png")
make_icon(192, base + "/icon-192.png")
make_icon(180, base + "/apple-touch-icon.png")
