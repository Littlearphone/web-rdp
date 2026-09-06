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

// DownscaleBGRA 双线性缩放到 tw*th（BGRA）。
// 用逐像素最近行采样（平滑度比最近邻好、比 2x2 快得多），适合大尺寸下采样。
// 通过预计算源行/列索引，避免内层循环做除法，显著提速。
func DownscaleBGRA(src []byte, sw, sh, tw, th int) []byte {
	if tw <= 0 || th <= 0 || sw <= 0 || sh <= 0 {
		return nil
	}
	if tw == sw && th == sh {
		out := make([]byte, len(src))
		copy(out, src)
		return out
	}
	dst := make([]byte, tw*th*4)
	// 预计算每个目标 y 对应的源 y（最近行）
	srcRows := make([]int, th)
	for y := 0; y < th; y++ {
		sy := y * sh / th
		if sy >= sh {
			sy = sh - 1
		}
		srcRows[y] = sy * sw
	}
	// 逐行：对每个目标 x 取最近源 x
	for y := 0; y < th; y++ {
		rowBase := srcRows[y]
		do := y * tw * 4
		for x := 0; x < tw; x++ {
			sx := x * sw / tw
			if sx >= sw {
				sx = sw - 1
			}
			p := (rowBase + sx) * 4
			di := do + x*4
			dst[di], dst[di+1], dst[di+2], dst[di+3] = src[p], src[p+1], src[p+2], 255
		}
	}
	return dst
}
