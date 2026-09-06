//go:build windows

package native

import (
	"strings"
	"syscall"
	"unsafe"
)

// ── GPU 厂商运行时探测（替代 ffmpeg_install.go 里基于 wmic 的 detectGPUVendor）──
//
// 为避免依赖外部命令 (wmic)，这里进程内用 EnumDisplayDevicesW 枚举显示适配器，
// 通过设备名字符串判断供应商。等价于项目既有 detectGPUVendor 的语义，
// 且为后续换成 DXGI EnumAdapters 读 VendorId（更可靠）留出升级位。

// DISPLAY_DEVICE 结构（与 Windows SDK wingdi.h 一致，32 位版）
type displayDevice struct {
	cb           uint32
	DeviceName   [32]uint16
	DeviceString [128]uint16
	StateFlags   uint32
	DeviceID     [128]uint16
	DeviceKey    [128]uint16
}

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	procEnumDisplayDevicesW  = user32.NewProc("EnumDisplayDevicesW")
	// 显示设备状态标志：DISPLAY_DEVICE_ATTACHED_TO_DESKTOP = 0x1
)

// enumerateAdapters 返回所有已连接显示适配器的名称字符串。
func enumerateAdapters() []string {
	var out []string
	var dd displayDevice
	dd.cb = uint32(unsafe.Sizeof(dd))
	for i := uint32(0); ; i++ {
		r, _, _ := procEnumDisplayDevicesW.Call(
			uintptr(0), // lpDevice = NULL（枚举显示适配器）
			uintptr(i),
			uintptr(unsafe.Pointer(&dd)),
			0)
		if r == 0 {
			break
		}
		name := utf16ToString(dd.DeviceString[:])
		// 重置 cb（Windows 文档要求每次调用前 cb 为 sizeof）
		dd = displayDevice{}
		dd.cb = uint32(unsafe.Sizeof(dd))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// DetectGPU returns the primary GPU vendor by scanning attached display adapters.
// 与现有 detectGPUVendor 命名语义一致，但改在进程内完成、不依赖 wmic。
func DetectGPU() Vendor {
	adapters := enumerateAdapters()
	if len(adapters) == 0 {
		return VendorUnknown
	}
	vendors := make(map[Vendor]bool)
	low := ""
	for _, a := range adapters {
		low = strings.ToLower(a)
		v := VendorUnknown
		switch {
		case strings.Contains(low, "nvidia"):
			v = VendorNVIDIA
		case strings.Contains(low, "amd") || strings.Contains(low, "radeon") || strings.Contains(low, "ati"):
			v = VendorAMD
		case strings.Contains(low, "intel"):
			v = VendorIntel
		}
		if v != VendorUnknown {
			vendors[v] = true
		}
	}
	if len(vendors) == 0 {
		return VendorUnknown
	}
	if len(vendors) > 1 {
		return VendorMixed
	}
	for v := range vendors {
		return v
	}
	return VendorUnknown
}
