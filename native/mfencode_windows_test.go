//go:build windows

package native

import "testing"

// TestEncodeSingleFrameMF 真机头less 编码冒烟：编码一帧 NV12 为 H.264，
// 校验产出合法 Annex B（含 SPS/PPS/IDR）。需机器具备 MF H.264 编码器 MFT。
// 若系统确实无任何可用编码器则跳过；否则必须成功（防止回归）。
func TestEncodeSingleFrameMF(t *testing.T) {
	data, info, err := EncodeSingleFrameSmoke(320, 240)
	if err != nil {
		t.Fatalf("MF H.264 编码失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("编码返回空数据")
	}
	ok, detail := validateAnnexB(data)
	t.Logf("info: %s", info)
	t.Logf("validate: %s", detail)
	if !ok {
		t.Fatalf("输出非合法 Annex B: %s", detail)
	}
	// 帧不应为空壳；真实编码一帧 320x240 至少几百字节。
	if len(data) < 100 {
		t.Fatalf("输出过小 (%d B)，疑似未真正编码", len(data))
	}
}
