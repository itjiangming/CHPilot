#!/usr/bin/env python3
"""生成 CHPilot 应用图标（多尺寸 PNG + ICO）。

设计：圆角方形蓝色渐变底 + 白色拨动开关（配合左侧串口数据引脚），
寓意"通过串口控制通道开关"。全部用几何图元绘制并超采样，保证各尺寸清晰。
"""
from PIL import Image, ImageDraw

S = 1024  # 超采样画布
img = Image.new("RGBA", (S, S), (0, 0, 0, 0))

# ---- 背景：圆角方形 + 垂直渐变（#3B82F6 → #1D4ED8）----
grad = Image.new("RGB", (1, S))
for y in range(S):
    t = y / (S - 1)
    grad.putpixel((0, y), (
        int(59 + (29 - 59) * t),
        int(130 + (78 - 130) * t),
        int(246 + (216 - 246) * t),
    ))
grad = grad.resize((S, S))
mask = Image.new("L", (S, S), 0)
ImageDraw.Draw(mask).rounded_rectangle([0, 0, S - 1, S - 1], radius=int(S * 0.23), fill=255)
img.paste(grad, (0, 0), mask)
d = ImageDraw.Draw(img)

# ---- 拨动开关：白色胶囊 + 蓝色圆钮（钮在右侧＝导通/开启）----
# 单一主图形并居中放大，保证 16px 小尺寸下依然清晰可辨
sw_x0, sw_x1 = int(S * 0.175), int(S * 0.825)
sw_y0, sw_y1 = int(S * 0.382), int(S * 0.618)
sw_r = (sw_y1 - sw_y0) // 2
d.rounded_rectangle([sw_x0, sw_y0, sw_x1, sw_y1], radius=sw_r, fill=(255, 255, 255, 255))

# 胶囊内左侧两条浅蓝短线：暗示串口引脚（小尺寸下自然虚化，不影响主形识别）
stripe_x0 = sw_x0 + int((sw_y1 - sw_y0) * 0.42)
stripe_w = int((sw_y1 - sw_y0) * 0.10)
for k, fy in enumerate((0.36, 0.64)):
    y = int(sw_y0 + (sw_y1 - sw_y0) * fy)
    d.rounded_rectangle([stripe_x0, y - stripe_w // 2, stripe_x0 + int((sw_y1 - sw_y0) * 0.34), y + stripe_w // 2],
                        radius=stripe_w // 2, fill=(147, 197, 253, 235))

knob_r = int(sw_r * 0.74)
knob_cx = sw_x1 - sw_r
knob_cy = (sw_y0 + sw_y1) // 2
d.ellipse([knob_cx - knob_r, knob_cy - knob_r, knob_cx + knob_r, knob_cy + knob_r],
          fill=(37, 99, 235, 255))

# ---- 导出 ----
sizes = [16, 24, 32, 48, 64, 128, 256, 512]
pngs = {}
for s in sizes:
    im = img.resize((s, s), Image.LANCZOS)
    pngs[s] = im
    im.save(f"icon-{s}.png")

# 供网页/桌面使用的常用尺寸
img.resize((192, 192), Image.LANCZOS).save("icon-192.png")
img.resize((512, 512), Image.LANCZOS).save("chpilot-icon.png")  # 通用大图

# Windows ICO（多尺寸内嵌，标题栏/任务栏/资源管理器各取所需）
ico_sizes = [16, 24, 32, 48, 64, 128, 256]
pngs[256].save("chpilot.ico", format="ICO",
               sizes=[(s, s) for s in ico_sizes])
print("生成完成：", ", ".join(f"icon-{s}.png" for s in sizes), "chpilot.ico, chpilot-icon.png")
