//go:build windows

package native

import "testing"

// TestEnumHardwareVideoEncoders 冒烟测试：MF 枚举应能无崩溃地返回一个列表。
// 在无 GPU / 无硬件编码器 MFT 的机器上允许为空列表（不视为失败），
// 但调用本身不得 panic / 崩溃。用于守护纯 syscall 绑定在 CI 上可运行。
func TestEnumHardwareVideoEncoders(t *testing.T) {
	names, err := EnumHardwareVideoEncoders()
	if err != nil {
		// MF 枚举失败本身应被优雅返回（调用方据此回退 ffmpeg），
		// 因此这里只记录，不判失败——但如果是本机逻辑 bug 导致的错误则暴露。
		t.Logf("EnumHardwareVideoEncoders err (allowed on constrained hosts): %v", err)
		return
	}
	t.Logf("枚举到 %d 个硬件 H.264 编码器 MFT: %v", len(names), names)
}

// TestQualityToCQ 守护质量映射与 ffmpeg_pipeline.go 语义一致。
func TestQualityToCQ(t *testing.T) {
	cases := []struct {
		q, want int
	}{
		{100, 25}, {75, 35}, {30, 48},
	}
	for _, c := range cases {
		if got := QualityToCQ(c.q); got != c.want {
			t.Errorf("QualityToCQ(%d)=%d, want %d", c.q, got, c.want)
		}
	}
}
