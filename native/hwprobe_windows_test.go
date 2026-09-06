//go:build windows

package native

import "testing"

// TestDetectHardwareCandidates 在本机枚举硬件 H.264 编码器并分类 usable 与否。
// 这是硬件编码接入的地基验证：确认能发现哪些编码器可用同步逐帧（usable）。
func TestDetectHardwareCandidates(t *testing.T) {
	Verbose = true
	Verbose = false
	cands := DetectHardwareCandidates(0, 0)
	if len(cands) == 0 {
		t.Logf("未发现任何硬件 H.264 编码器 MFT（本机可能无）")
		return
	}
	for _, c := range cands {
		if c.Usable {
			t.Logf("  ✓ usable: %s", c.Name)
		} else {
			t.Logf("  ✗ (async/需D3D11): %s — %s", c.Name, c.SyncErr)
		}
	}
}
