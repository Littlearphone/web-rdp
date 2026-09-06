package native

// ValidateAnnexB 导出 validateAnnexB，供包外（main 端到端测试）断言 H.264 合法性。
func ValidateAnnexB(data []byte) (bool, string) {
	return validateAnnexB(data)
}

// ── BGRA → NV12 转换 ──
//
// screenshot.CaptureDisplay（GDI）返回 image.RGBA（内存为 R,G,B,A 顺序）。
// MF 编码器输入需要 NV12（Y 平面 + 交错的 UV 平面）。
// 这里实现带 BT.601 色域转换的 BGRA→NV12。缩放交由调用方先行完成。

// BGRAToNV12 把 w*h 的 BGRA 像素（B,G,R,A 序，如 DXGI 桌面捕获）转为 NV12。
// src 长度须 >= w*h*4。
func BGRAToNV12(src []byte, w, h int) []byte {
	dst := make([]byte, w*h*3/2)
	// Y 平面：BT.601 limited-range。Y = 16 + (66R+129G+25B)/256，钳 16..235。
	yi := 0
	for y := 0; y < h; y++ {
		row := y * w
		for x := 0; x < w; x++ {
			p := (row + x) * 4
			// BGRA：b=src[p], g=src[p+1], r=src[p+2]
			b, g, r := int(src[p]), int(src[p+1]), int(src[p+2])
			yuv := ((66*r + 129*g + 25*b) >> 8) + 16
			if yuv < 16 {
				yuv = 16
			} else if yuv > 235 {
				yuv = 235
			}
			dst[yi] = byte(yuv)
			yi++
		}
	}
	// UV 平面交错（每 2x2 取一个 UV 对）：NV12 布局 = Y 平面后紧跟 w/2*h/2 个 UV 交错字节。
	// 每个色度采样对应 2x2 亮度块：字节序 [U,V] 成对交替。
	// 色度用 BT.601 limited-range 公式（U/V 中心 128，范围约 16..240）：
	//   U = 128 - 0.1687*R - 0.3313*G + 0.5000*B
	//   V = 128 + 0.5000*R - 0.4187*G - 0.0813*B
	// 定点实现（>>8）：U = 128 + (-43R - 85G + 128B + 128)>>8
	//              V = 128 + (128R - 107G - 21B + 128)>>8
	uvw := w / 2
	uvh := h / 2
	uvBase := w * h
	for j := 0; j < uvh; j++ {
		for i := 0; i < uvw; i++ {
			sx := i*2 + 1
			sy := j*2 + 1
			if sx >= w {
				sx = w - 1
			}
			if sy >= h {
				sy = h - 1
			}
			p := (sy*w + sx) * 4
			b, g, r := int(src[p]), int(src[p+1]), int(src[p+2]) // BGRA
			u := 128 + ((-43*r - 85*g + 128*b + 128) >> 8)
			v := 128 + ((128*r - 107*g - 21*b + 128) >> 8)
			if u < 0 {
				u = 0
			} else if u > 255 {
				u = 255
			}
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			off := uvBase + (j*uvw+i)*2
			dst[off] = byte(u)     // U
			dst[off+1] = byte(v)   // V
		}
	}
	return dst
}

// DownscaleBGRA 双线性缩放到 tw*th（BGRA），避免最近邻产生的锯齿/模糊。
// 坐标映射为整数几何：目标像素 (x,y) 对应源 (sx,sy)，sx=floor(x*sw/tw)。
// 采用 2x2 盒式/双线性近似改善文本清晰度。
func DownscaleBGRA(src []byte, sw, sh, tw, th int) []byte {
	dst := make([]byte, tw*th*4)
	for y := 0; y < th; y++ {
		sy := y * sh / th
		if sy >= sh {
			sy = sh - 1
		}
		sy2 := sy + 1
		if sy2 >= sh {
			sy2 = sy
		}
		fy := y*sh*2 - sy*2*th // 误差（单位 2*th），0..2*th 未用，简化为最近行
		_ = fy
		for x := 0; x < tw; x++ {
			sx := x * sw / tw
			if sx >= sw {
				sx = sw - 1
			}
			sx2 := sx + 1
			if sx2 >= sw {
				sx2 = sx
			}
			// 2x2 平均（四邻像素）近似双线性，平滑锯齿
			var r, g, b int
			for dy := 0; dy < 2; dy++ {
				syy := sy
				if dy == 1 {
					syy = sy2
				}
				for dx := 0; dx < 2; dx++ {
					sxx := sx
					if dx == 1 {
						sxx = sx2
					}
					p := (syy*sw + sxx) * 4
					r += int(src[p])
					g += int(src[p+1])
					b += int(src[p+2])
				}
			}
			r, g, b = r>>2, g>>2, b>>2
			do := (y*tw + x) * 4
			dst[do], dst[do+1], dst[do+2], dst[do+3] = byte(r), byte(g), byte(b), 255
		}
	}
	return dst
}
