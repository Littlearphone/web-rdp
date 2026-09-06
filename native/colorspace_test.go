//go:build windows

package native

import "testing"

// TestBGRAToNV12Color 验证 BGRAToNV12（输入为 BGRA 字节序）的色度转换正确。
// 回归守护1：曾因色度公式错误全绿屏。
// 回归守护2：曾把输入误当 RGBA 读，导致红↔蓝互换（用户实测红变蓝/蓝变红）。
func TestBGRAToNV12Color(t *testing.T) {
	const w, h = 64, 64
	bgra := make([]byte, w*h*4)
	set := func(r, g, b byte) {
		for i := 0; i < w*h; i++ {
			bgra[i*4+0] = b // B
			bgra[i*4+1] = g // G
			bgra[i*4+2] = r // R
			bgra[i*4+3] = 255
		}
	}
	uv := w * h

	// 纯蓝 (R0,G0,B255) → BGRA 里 p0=B=255
	set(0, 0, 255)
	nv := BGRAToNV12(bgra, w, h)
	u, v := int(nv[uv]), int(nv[uv+1])
	if u < 200 || u > 255 {
		t.Errorf("纯蓝 U=%d，期望≈240", u)
	}
	if v < 80 || v > 150 {
		t.Errorf("纯蓝 V=%d，期望≈110", v)
	}

	// 纯红 (R255,G0,B0) → BGRA 里 p2=R=255
	set(255, 0, 0)
	nv2 := BGRAToNV12(bgra, w, h)
	u2, v2 := int(nv2[uv]), int(nv2[uv+1])
	if u2 < 60 || u2 > 130 {
		t.Errorf("纯红 U=%d，期望≈90", u2)
	}
	if v2 < 200 || v2 > 255 {
		t.Errorf("纯红 V=%d，期望≈240", v2)
	}
	// 关键：红与蓝不可互换（若误当 RGBA，纯红会得到蓝的 U/V，两者区间不同故上面已能区分）

	// 中灰 (128,128,128) → U≈128, V≈128
	set(128, 128, 128)
	nv3 := BGRAToNV12(bgra, w, h)
	u3, v3 := int(nv3[uv]), int(nv3[uv+1])
	if abs(u3-128) > 8 || abs(v3-128) > 8 {
		t.Errorf("中灰 U=%d V=%d，期望≈128", u3, v3)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
