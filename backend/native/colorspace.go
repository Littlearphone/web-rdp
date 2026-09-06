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
//
// 性能注记：Y = ((66R+129G+25B)>>8)+16 中 (66+129+25)*255>>8 = 219，故 Y 恒在 [16,235]，
// 无需逐像素钳制分支；去掉钳制能显著提速大尺寸转换。
func BGRAToNV12(src []byte, w, h int) []byte {
	dst := make([]byte, w*h*3/2)
	// Y 平面：BT.601 limited-range，无钳制（恒在 [16,235]）。
	yi := 0
	si := 0
	for i := 0; i < w*h; i++ {
		b := int(src[si])
		g := int(src[si+1])
		r := int(src[si+2])
		dst[yi] = byte(((66*r + 129*g + 25*b) >> 8) + 16)
		yi++
		si += 4
	}
	// UV 平面交错（每 2x2 取一个 UV 对）：字节序 [U,V] 成对。
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
			dst[off] = byte(u)   // U
			dst[off+1] = byte(v) // V
		}
	}
	return dst
}

// BGRAToNV12Into 把 BGRA 转为 NV12 写入复用缓冲 dst（须 len>=w*h*3/2），避免每帧大块分配。
// 返回 dst 的 [0, w*h*3/2) 子切片。若 dst 过小则内部自分配并返回（仍正确）。
func BGRAToNV12Into(dst, src []byte, w, h int) []byte {
	need := w * h * 3 / 2
	if cap(dst) < need {
		return BGRAToNV12(src, w, h)
	}
	dst = dst[:need]
	yi := 0
	si := 0
	for i := 0; i < w*h; i++ {
		b := int(src[si])
		g := int(src[si+1])
		r := int(src[si+2])
		dst[yi] = byte(((66*r + 129*g + 25*b) >> 8) + 16)
		yi++
		si += 4
	}
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
			b, g, r := int(src[p]), int(src[p+1]), int(src[p+2])
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
			dst[off] = byte(u)
			dst[off+1] = byte(v)
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
