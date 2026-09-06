//go:build windows

package native

import "testing"

// TestNewD3D11Device 冒烟：创建硬件 D3D11 设备应成功。
func TestNewD3D11Device(t *testing.T) {
	dev, err := NewD3D11Device()
	if err != nil {
		t.Fatalf("NewD3D11Device: %v", err)
	}
	defer dev.Close()
	if dev.Device() == nil || dev.Context() == nil {
		t.Fatalf("设备/上下文为空")
	}
	t.Logf("D3D11 设备创建成功")
}
