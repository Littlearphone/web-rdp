//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── D3D11 与 MF 互操作：IMFDXGIDeviceManager（硬件异步 MFT 的 D3D11 输入入口）──

var (
	// mfplat 导出（补充 mfprobe_windows.go 未声明的）
	procMFCreateDXGIDeviceManager = mfplat.NewProc("MFCreateDXGIDeviceManager")
)

// mfD3DManager 封装 IMFDXGIDeviceManager*，并绑定到 D3D11 设备。
// IMFDXGIDeviceManager 直接继承 IUnknown，vtable 槽位(基=3)：
//   [3]CloseDeviceHandle [4]GetVideoService [5]LockDevice [6]OpenDeviceHandle
//   [7]ResetDevice [8]TestDevice [9]UnlockDevice
type mfD3DManager struct {
	mgr   unsafe.Pointer // IMFDXGIDeviceManager*
	reset uint32
}

// NewMFDXGIDeviceManager 创建 manager 并 ResetDevice 关联 D3D11 设备。
func NewMFDXGIDeviceManager(dev *D3D11Device) (*mfD3DManager, error) {
	if !mfStartup() {
		return nil, fmt.Errorf("MFStartup 失败")
	}
	var mgr unsafe.Pointer
	var reset uint32
	r, _, _ := procMFCreateDXGIDeviceManager.Call(
		uintptr(unsafe.Pointer(&reset)),
		uintptr(unsafe.Pointer(&mgr)))
	if r != 0 {
		mfShutdown()
		return nil, fmt.Errorf("MFCreateDXGIDeviceManager HRESULT=0x%x", r)
	}
	if mgr == nil {
		mfShutdown()
		return nil, fmt.Errorf("MFCreateDXGIDeviceManager 返回空")
	}
	m := &mfD3DManager{mgr: mgr, reset: reset}
	// ResetDevice(dev, resetToken)：device 需作为 IUnknown*（D3D11 设备已实现 IUnknown）。
	if err := m.resetDevice(dev.Device()); err != nil {
		m.Close()
		mfShutdown()
		return nil, err
	}
	return m, nil
}

// resetDevice 调用 IMFDXGIDeviceManager::ResetDevice（槽位 7）。
func (m *mfD3DManager) resetDevice(dev unsafe.Pointer) error {
	fn := comMethod(m.mgr, 7)
	r, _, _ := syscall.SyscallN(fn, uintptr(m.mgr), uintptr(dev), uintptr(m.reset))
	if r != 0 {
		return fmt.Errorf("ResetDevice HRESULT=0x%x", r)
	}
	return nil
}

// SetOnTransform 给 IMFTransform 设置 D3D manager（MFT_MESSAGE_SET_D3D_MANAGER=0x2）。
// param 传 manager 指针。返回 HRESULT。
func (m *mfD3DManager) SetOnTransform(tr unsafe.Pointer) error {
	return tProcessMessage(tr, 0x2, uintptr(m.mgr))
}

// Close 释放 manager。
func (m *mfD3DManager) Close() {
	if m.mgr != nil {
		comRelease(m.mgr)
		m.mgr = nil
	}
}
