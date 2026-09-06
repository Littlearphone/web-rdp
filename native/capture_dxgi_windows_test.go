//go:build windows

package native

import "testing"

// TestDXGIDuplicationReadback 验证 DXGI Desktop Duplication 直捕：取真实桌面帧并回读 BGRA。
// 这是跨厂商硬件路径的采集地基（D3D11 设备→adapter→output→duplicate→texture→Map）。
func TestDXGIDuplicationReadback(t *testing.T) {
	dup, err := NewDXGIDuplicator(0)
	if err != nil {
		t.Fatalf("NewDXGIDuplicator: %v", err)
	}
	defer dup.Close()
	fr, err := dup.AcquireFrame(3000)
	if err != nil {
		t.Fatalf("AcquireFrame: %v", err)
	}
	if fr == nil {
		t.Skip("未在超时内取到帧（静止桌面可能无新帧）")
	}
	if fr.Width <= 0 || fr.Height <= 0 || len(fr.Data) != fr.Width*fr.Height*4 {
		t.Fatalf("异常帧: %dx%d len=%d", fr.Width, fr.Height, len(fr.Data))
	}
	t.Logf("DXGI 直捕帧: %dx%d BGRA=%d 字节", fr.Width, fr.Height, len(fr.Data))
}
