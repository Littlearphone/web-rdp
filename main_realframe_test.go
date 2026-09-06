package main

import (
	"image"
	"testing"

	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
	"web-rdp/native"
)

// resizeEven 双线性缩放到指定目标，返回 *image.RGBA。目标宽高须为偶数。
func resizeEven(src *image.RGBA, tw, th int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// rgbaToBGRA 把 image.RGBA 的 Pix(RGBA序) 转成 BGRA 序（BGRAToNV12 的输入基准）。
func rgbaToBGRA(pix []byte) []byte {
	out := make([]byte, len(pix))
	for i := 0; i+3 < len(pix); i += 4 {
		out[i] = pix[i+2] // B
		out[i+1] = pix[i+1]
		out[i+2] = pix[i] // R
		out[i+3] = pix[i+3]
	}
	return out
}

// TestRealFrameCaptureEncodeMF 端到端头less 验证"真实桌面帧 → NV12 → MF H.264"链路：
//   1) GDI 截取真实桌面帧（显示 0）
//   2) 缩放到偶数尺寸
//   3) BGRA→NV12
//   4) MFH264Encoder 编码多帧
//   5) 校验产出合法 H.264 Annex B
// 这是 ④b 的关键前提：证明 native（无 ffmpeg/cgo）能对真实画面做有状态硬件编码。
func TestRealFrameCaptureEncodeMF(t *testing.T) {
	if screenshot.NumActiveDisplays() == 0 {
		t.Skip("无活跃显示器")
	}
	img, err := screenshot.CaptureDisplay(0)
	if err != nil {
		t.Fatalf("CaptureDisplay: %v", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		t.Fatalf("空尺寸")
	}
	// 缩小到最大 640 宽，确保偶数尺寸（NV12 4:2:0 与 H.264 均要求偶数）
	tw := 640
	th := h * tw / w
	if tw%2 != 0 {
		tw--
	}
	if th%2 != 0 {
		th--
	}
	small := resizeEven(img, tw, th)

	// 编码器一次配置、连续喂入若干真实帧（用同一真实帧喂 3 次以验证持续会话可用）。
	enc, err := native.NewMFH264Encoder(tw, th, 30)
	if err != nil {
		t.Fatalf("NewMFH264Encoder(%dx%d): %v", tw, th, err)
	}
	defer enc.Close()

	total := 0
	firstFrame := []byte{}
	nv12 := native.BGRAToNV12(rgbaToBGRA(small.Pix), tw, th)
	for i := 0; i < 3; i++ {
		d, err := enc.Encode(nv12)
		if err != nil {
			t.Fatalf("Encode[%d]: %v", i, err)
		}
		if len(d) > 0 {
			total += len(d)
			if len(firstFrame) == 0 {
				firstFrame = append([]byte{}, d...)
			}
		}
	}
	// 首帧通常含 SPS/PPS/IDR（关键帧）；取首帧校验完整 Annex B。
	validate := firstFrame
	if len(validate) == 0 {
		t.Fatalf("无编码输出")
	}
	ok, detail := native.ValidateAnnexB(validate)
	t.Logf("真实帧编码: 源 %dx%d → %dx%d, 总输出 %d 字节, %s", w, h, tw, th, total, detail)
	if !ok {
		t.Fatalf("真实帧输出非合法 Annex B: %s", detail)
	}
}

// ValidateAnnexB 是 native.ValidateAnnexB 的导出别名（便于 package main 测试断言）。
