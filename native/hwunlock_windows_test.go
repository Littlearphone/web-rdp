//go:build windows

package native

import "testing"

// TestHWEncoderUnlock 探测硬件 MFT 在设置 D3D manager 后能否解锁配置，
// 判定进程内 GPU 硬件编码路径是否可行。
func TestHWEncoderUnlock(t *testing.T) {
	t.Log(HWEncoderUnlock())
}
