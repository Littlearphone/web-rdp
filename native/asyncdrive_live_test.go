//go:build windows

package native

import (
	"testing"
	"time"
)

// TestAsyncHWLiveCaptureEncode 真实桌面 → downscale → BGRA→NV12 → 异步硬件编码 全链路。
// 编码器按首帧目标尺寸初始化，随后各帧降采样到同一尺寸喂入（与 produce() 一致）。
func TestAsyncHWLiveCaptureEncode(t *testing.T) {
	dup, err := NewDXGIDuplicator(0)
	if err != nil {
		t.Fatalf("NewDXGIDuplicator: %v", err)
	}
	defer dup.Close()

	// 目标最大宽度
	const maxW = 1280
	var tw, th int
	var enc *asyncMFEncoder

	encoded := 0
	anyValid := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		fr, err := dup.AcquireFrame(200)
		if err != nil || fr == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		cw, ch := fr.Width, fr.Height
		if tw == 0 {
			tw, th = cw, ch
			if tw > maxW {
				tw = maxW
				th = ch * tw / cw
			}
			tw -= tw % 16
			th -= th % 16
			if tw <= 0 || th <= 0 {
				t.Fatalf("非法目标尺寸 %dx%d", tw, th)
			}
			enc, err = NewAsyncMFH264Encoder(tw, th, 4_000_000)
			if err != nil {
				t.Fatalf("NewAsyncMFH264Encoder: %v", err)
			}
			defer enc.Close()
			t.Logf("编码器初始化 %dx%d", tw, th)
		}
		var small []byte
		if cw == tw && ch == th {
			small = fr.Data
		} else {
			small = DownscaleBGRA(fr.Data, cw, ch, tw, th)
		}
		nv12 := BGRAToNV12(small, tw, th)
		out, e2 := enc.Encode(nv12)
		if e2 != nil {
			t.Fatalf("真实帧编码失败: %v", e2)
		}
		if len(out) == 0 {
			continue
		}
		if ok, _ := validateAnnexB(out); ok {
			anyValid = true
		}
		encoded++
		if encoded >= 10 {
			break
		}
	}
	if enc == nil || encoded == 0 {
		t.Skip("8s 内未采集到真实桌面帧（桌面静止）")
	}
	if !anyValid {
		t.Fatalf("采集并编码 %d 帧但无合法 H.264 输出", encoded)
	}
	t.Logf("真实桌面→硬件编码成功 %d 帧（%dx%d），输出含 IDR/SPS/PPS", encoded, tw, th)
}
