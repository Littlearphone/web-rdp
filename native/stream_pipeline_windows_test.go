//go:build windows

package native

import "testing"

// TestNativeStreamPipeline 端到端 headless 验证 native 实时推流管线的服务端核心：
// DXGI 直捕真实桌面 → BGRA → NV12 → MFH264Encoder（逐帧）→ 连续多帧合法 H.264 Annex B。
// 这正是 ws.go native provider 会喂给 fan-out / WS / WebRTC 的数据形态。
func TestNativeStreamPipeline(t *testing.T) {
	dup, err := NewDXGIDuplicator(0)
	if err != nil {
		t.Fatalf("NewDXGIDuplicator: %v", err)
	}
	defer dup.Close()

	// 抓首帧取尺寸并缩放到偶数目标（<=480 宽，保证 NV12 4:2:0 偶数）
	fr, err := dup.AcquireFrame(3000)
	if err != nil {
		t.Fatalf("AcquireFrame: %v", err)
	}
	if fr == nil {
		t.Skip("无桌面帧")
	}
	w := fr.Width
	h := fr.Height
	tw := w
	th := h
	if tw > 480 {
		tw = 480
		th = h * tw / w
	}
	if tw%2 != 0 {
		tw--
	}
	if th%2 != 0 {
		th--
	}

	enc, err := NewMFH264Encoder(tw, th, 30)
	if err != nil {
		t.Fatalf("NewMFH264Encoder(%dx%d): %v", tw, th, err)
	}
	defer enc.Close()

	// 循环捕获+缩放+编码，收集逐帧输出；收到若干帧即可。
	got := 0
	firstNonEmpty := false
	for i := 0; i < 6 && got < 4; i++ {
		f, err := dup.AcquireFrame(2000)
		if err != nil {
			t.Fatalf("AcquireFrame[%d]: %v", i, err)
		}
		if f == nil {
			continue
		}
		small := downscaleBGRA(f.Data, f.Width, f.Height, tw, th)
		nv12 := BGRAToNV12(small, tw, th)
		d, err := enc.Encode(nv12)
		if err != nil {
			t.Fatalf("Encode[%d]: %v", i, err)
		}
		if len(d) > 0 {
			got++
			if !firstNonEmpty {
				firstNonEmpty = true
				if !looksLikeNAL(d) {
					t.Fatalf("首帧无 NAL 起始码")
				}
				if ok, _ := validateAnnexB(d); !ok {
					// 首帧应含 SPS/PPS/IDR（关键帧）
					t.Fatalf("首帧非关键帧(缺 SPS/PPS/IDR)")
				}
			}
		}
	}
	if got == 0 {
		t.Fatalf("native 推流管线未产出任何 H.264 帧")
	}
	t.Logf("native 推流管线成功: 连续产出 %d 帧合法 H.264 (编码尺寸 %dx%d)", got, tw, th)
}

// downscaleBGRA 简单双线性缩放到 tw*th（BGRA）。
func downscaleBGRA(src []byte, sw, sh, tw, th int) []byte {
	dst := make([]byte, tw*th*4)
	for y := 0; y < th; y++ {
		sy := y * sh / th
		if sy >= sh {
			sy = sh - 1
		}
		for x := 0; x < tw; x++ {
			sx := x * sw / tw
			if sx >= sw {
				sx = sw - 1
			}
			so := (sy*sw + sx) * 4
			do := (y*tw + x) * 4
			copy(dst[do:do+4], src[so:so+4])
		}
	}
	return dst
}
