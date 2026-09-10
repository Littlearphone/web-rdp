package native

import (
	"os"
	"runtime"
	"strconv"
	"sync"
)

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

// ── 融合缩放 + 色彩转换（2K 高帧率关键路径）──
//
// 旧路径是两步：DownscaleBGRA(src → 中间 BGRA tw*th) 再 BGRAToNV12Into(中间 → NV12)。
// 中间那块 BGRA 缓冲（2560x1070 ≈ 10.7 MB）每帧写完再读，纯属浪费。
// 融合后一趟遍历直接出 NV12，省掉整块中间缓冲与二次遍历。
//
// ⚠ 性能陷阱（踩过）：内层循环里 `sx := x*sw/tw` 是**每像素一次整数除法**，
// 编译期无法消除（sw/tw 是运行时值）。不缩放时（sw==tw）它本该是恒等映射，
// 却仍要付 7.4M 次除法/帧（3440x1440），比原来的线性 BGRAToNV12 更慢。
// 因此这里：
//   1) 等尺寸走专用无除法线性内核（并同样支持并行）；
//   2) 需要缩放时**每帧只建一次索引表**，内层循环只做查表，不做除法。
// 索引表很小（tw + tw/2 + th + th/2 个 int32），构造成本远低于逐像素除法。

// nv12Index 是"目标像素 → 源字节偏移"的预计算表（最近邻采样）。
type nv12Index struct {
	cols   []int32 // [tw]      每个目标 x 的源像素字节偏移（行内）
	uvCols []int32 // [tw/2]    每个 UV 列的源像素字节偏移（行内）
	rows   []int32 // [th]      每个目标 y 的源行首字节偏移
	uvRows []int32 // [th/2]    每个 UV 行的源行首字节偏移
}

// buildNV12Index 构建索引表（每帧一次）。
func buildNV12Index(sw, sh, tw, th int) *nv12Index {
	idx := &nv12Index{
		cols:   make([]int32, tw),
		uvCols: make([]int32, tw/2),
		rows:   make([]int32, th),
		uvRows: make([]int32, th/2),
	}
	for x := 0; x < tw; x++ {
		sx := x * sw / tw
		if sx >= sw {
			sx = sw - 1
		}
		idx.cols[x] = int32(sx * 4)
	}
	for i := 0; i < tw/2; i++ {
		sx := (i*2 + 1) * sw / tw
		if sx >= sw {
			sx = sw - 1
		}
		idx.uvCols[i] = int32(sx * 4)
	}
	for y := 0; y < th; y++ {
		sy := y * sh / th
		if sy >= sh {
			sy = sh - 1
		}
		idx.rows[y] = int32(sy * sw * 4)
	}
	for j := 0; j < th/2; j++ {
		sy := (j*2 + 1) * sh / th
		if sy >= sh {
			sy = sh - 1
		}
		idx.uvRows[j] = int32(sy * sw * 4)
	}
	return idx
}

// scaledNV12Range 对目标行区间 [y0,y1) 做"查表缩放 + 色彩转换"。供串行与并行共用。
// dst 须已分配 len >= tw*th*3/2。内层无除法。
func scaledNV12Range(dst, src []byte, tw, th, y0, y1 int, idx *nv12Index) {
	uvBase := tw * th
	cols := idx.cols
	// Y 平面
	for y := y0; y < y1; y++ {
		rowBase := int(idx.rows[y])
		yi := y * tw
		for x := 0; x < tw; x++ {
			p := rowBase + int(cols[x])
			b := int(src[p])
			g := int(src[p+1])
			r := int(src[p+2])
			dst[yi+x] = byte(((66*r + 129*g + 25*b) >> 8) + 16)
		}
	}
	// UV 平面：每个 UV 行覆盖 2 个目标 Y 行，按 Y 区间折算避免并行重复写
	j0 := y0 / 2
	j1 := (y1 + 1) / 2
	if uvh := th / 2; j1 > uvh {
		j1 = uvh
	}
	uvw := tw / 2
	for j := j0; j < j1; j++ {
		rowBase := int(idx.uvRows[j])
		out := uvBase + j*uvw*2
		for i := 0; i < uvw; i++ {
			p := rowBase + int(idx.uvCols[i])
			b := int(src[p])
			g := int(src[p+1])
			r := int(src[p+2])
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
			dst[out+i*2] = byte(u)
			dst[out+i*2+1] = byte(v)
		}
	}
}

// pureNV12Range 等尺寸（无需缩放）的纯色彩转换，对目标行区间 [y0,y1) 生效。
// 内层只有连续读写与乘加，没有索引表、没有除法 —— 这是原始分辨率档位的关键路径。
func pureNV12Range(dst, src []byte, w, h, y0, y1 int) {
	// Y 平面：src 每像素 BGRA
	si := y0 * w * 4
	yi := y0 * w
	for y := y0; y < y1; y++ {
		for x := 0; x < w; x++ {
			b := int(src[si])
			g := int(src[si+1])
			r := int(src[si+2])
			dst[yi] = byte(((66*r + 129*g + 25*b) >> 8) + 16)
			yi++
			si += 4
		}
	}
	// UV 平面：每 2x2 取一点（取右下角，与 BGRAToNV12 一致）
	uvBase := w * h
	uvw := w / 2
	j0 := y0 / 2
	j1 := (y1 + 1) / 2
	if uvh := h / 2; j1 > uvh {
		j1 = uvh
	}
	for j := j0; j < j1; j++ {
		var sy int
		if s := j*2 + 1; s >= h {
			sy = h - 1
		} else {
			sy = s
		}
		rowBase := sy * w * 4
		out := uvBase + j*uvw*2
		for i := 0; i < uvw; i++ {
			sx := i*2 + 1
			if sx >= w {
				sx = w - 1
			}
			p := rowBase + sx*4
			b := int(src[p])
			g := int(src[p+1])
			r := int(src[p+2])
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
			dst[out+i*2] = byte(u)
			dst[out+i*2+1] = byte(v)
		}
	}
}

// DownscaleBGRAToNV12Into 融合"缩放 + BGRA→NV12"，一趟遍历直出目标尺寸 NV12。
// src 为 BGRA(sw*sh)，输出写入 dst（cap 不足时内部自分配并返回）。
// 等尺寸时走无除法的纯转换内核。
func DownscaleBGRAToNV12Into(dst, src []byte, sw, sh, tw, th int) []byte {
	need := tw * th * 3 / 2
	if cap(dst) < need {
		dst = make([]byte, need)
	}
	dst = dst[:need]
	if sw == tw && sh == th {
		pureNV12Range(dst, src, tw, th, 0, th)
		return dst
	}
	scaledNV12Range(dst, src, tw, th, 0, th, buildNV12Index(sw, sh, tw, th))
	return dst
}

// DownscaleBGRAToNV12ParallelInto 在融合内核上按行分片并行。
// workers<=1 时退化为串行。分片按偶数行对齐，保证同一 UV 行不会被两个 goroutine 重复写。
// 等尺寸（原始分辨率）走无除法的纯转换内核并行；需要缩放时共用一张索引表（只建一次）。
func DownscaleBGRAToNV12ParallelInto(dst, src []byte, sw, sh, tw, th, workers int) []byte {
	need := tw * th * 3 / 2
	if cap(dst) < need {
		dst = make([]byte, need)
	}
	dst = dst[:need]

	sameSize := sw == tw && sh == th
	if workers <= 1 {
		if sameSize {
			pureNV12Range(dst, src, tw, th, 0, th)
		} else {
			scaledNV12Range(dst, src, tw, th, 0, th, buildNV12Index(sw, sh, tw, th))
		}
		return dst
	}

	var idx *nv12Index
	if !sameSize {
		idx = buildNV12Index(sw, sh, tw, th) // 每帧一次，避免内层除法
	}
	if workers > th/2 {
		workers = th / 2
	}
	if workers < 2 {
		if sameSize {
			pureNV12Range(dst, src, tw, th, 0, th)
		} else {
			scaledNV12Range(dst, src, tw, th, 0, th, idx)
		}
		return dst
	}

	chunk := (th + workers - 1) / workers
	if chunk%2 != 0 {
		chunk++ // 偶数行对齐，避免 UV 行重叠
	}
	var wg sync.WaitGroup
	for y0 := 0; y0 < th; y0 += chunk {
		y1 := y0 + chunk
		if y1 > th {
			y1 = th
		}
		wg.Add(1)
		if sameSize {
			go func(a, b int) { defer wg.Done(); pureNV12Range(dst, src, tw, th, a, b) }(y0, y1)
		} else {
			go func(a, b int) { defer wg.Done(); scaledNV12Range(dst, src, tw, th, a, b, idx) }(y0, y1)
		}
	}
	wg.Wait()
	return dst
}

// OptimalNV12Workers 返回融合内核的建议并发度（按目标高度与 CPU 核数裁剪）。
//
// 注意：并发并非越大越好——并行 worker 会与 **DXGI 采集回读**争抢内存带宽，
// 而回读才是这条流水线的真正瓶颈。因此这里默认保守（见下），
// 并支持环境变量 WEBRDP_CVT_WORKERS 覆盖以便实测调优：
//
//	WEBRDP_CVT_WORKERS=1  → 串行（内核稍慢，但完全不影响采集）
//	WEBRDP_CVT_WORKERS=N  → 固定 N 线程
//	未设置 / 0            → 自动
func OptimalNV12Workers(th int) int {
	if v := os.Getenv("WEBRDP_CVT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 1 {
				n = 1
			}
			return clampWorkers(n, th)
		}
	}
	// 自动：保守取核数/4（上限 4），避免多档位并存时把带宽从采集手里抢走
	n := runtime.NumCPU() / 4
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return clampWorkers(n, th)
}

// clampWorkers 把并发度裁剪到目标高度允许的范围。
func clampWorkers(n, th int) int {
	if n < 1 {
		n = 1
	}
	if maxW := th / 2; n > maxW {
		n = maxW
	}
	if n < 1 {
		n = 1
	}
	return n
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
