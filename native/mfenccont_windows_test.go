//go:build windows

package native

import "testing"

// TestContinuousMFH264Encoder 喂入多帧 NV12，验证连续编码器持续产出 H.264 Annex B，
// 且每个非空输出都能解析出有效 NAL 结构。这是 ws.go native 会话所需的有状态编码原语。
func TestContinuousMFH264Encoder(t *testing.T) {
	const frames = 8
	enc, err := NewMFH264Encoder(320, 240, 30)
	if err != nil {
		t.Fatalf("NewMFH264Encoder: %v", err)
	}
	defer enc.Close()

	totalBytes := 0
	totalTicks := 0
	// 喂入 N 帧并累计各 tick 已就绪输出。
	for i := 0; i < frames; i++ {
		var nv12 []byte
		if i%4 == 0 {
			nv12 = makeNV12(320, 240)
		} else {
			nv12 = makeGradientNV12(320, 240, i)
		}
		d, err := enc.Encode(nv12)
		if err != nil {
			t.Fatalf("Encode[%d]: %v", i, err)
		}
		if len(d) > 0 {
			totalTicks++
			totalBytes += len(d)
			if !looksLikeNAL(d) {
				t.Fatalf("Encode[%d] 输出不含 NAL 起始码: 前16=% X", i, d[:min(16, len(d))])
			}
		}
	}
	// Flush 取回缓冲的剩余输出
	flush, err := enc.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(flush) > 0 {
		totalTicks++
		totalBytes += len(flush)
		if !looksLikeNAL(flush) {
			t.Fatalf("Flush 输出不含 NAL 起始码")
		}
	}
	if totalBytes == 0 {
		t.Fatalf("连续编码未产出任何字节")
	}
	// 流式断言：采用"喂一帧→DRAIN→收集"范式后，每次 Encode 都应产出一帧（首个含 SPS/PPS/IDR）。
	// 若 totalTicks 远小于 frames，说明又退回"缓冲到 Flush"的老行为，需检查流式驱动。
	if totalTicks < frames/2 {
		t.Fatalf("流式逐帧产出不足: 期望每帧有输出, 实际 %d/%d", totalTicks, frames)
	}
	// 校验 flush 流含关键帧（SPS/PPS/IDR）
	if len(flush) > 0 {
		ok, detail := validateAnnexB(flush)
		t.Logf("flush: %s", detail)
		if !ok {
			// 关键帧不一定落在 flush，允许 warn 不判死
			t.Logf("warning: flush 未含完整 SPS/PPS/IDR")
		}
	}
	t.Logf("连续编码成功: 喂入 %d 帧, %d 次就绪输出, 累计 %d 字节", frames, totalTicks, totalBytes)
}

// looksLikeNAL 判断字节流是否含 H.264 Annex B 起始码。
func looksLikeNAL(d []byte) bool {
	for i := 0; i+3 < len(d); i++ {
		if d[i] == 0 && d[i+1] == 0 && d[i+2] == 1 {
			return true
		}
		if i+4 < len(d) && d[i] == 0 && d[i+1] == 0 && d[i+2] == 0 && d[i+3] == 1 {
			return true
		}
	}
	return false
}

// makeGradientNV12 生成水平渐变 NV12，用于制造编码运动差异。
func makeGradientNV12(w, h int, phase int) []byte {
	buf := make([]byte, w*h*3/2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte((x*3 + y*2 + phase*17) & 0xff)
			buf[y*w+x] = v
		}
	}
	// 色度平面置 128
	uv := w * h
	for i := uv; i < len(buf); i++ {
		buf[i] = 128
	}
	return buf
}
