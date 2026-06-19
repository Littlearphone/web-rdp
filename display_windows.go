package main

import (
	"syscall"
	"unsafe"
)

// monitorInfoExW 是 MONITORINFOEXW 的 Go 映射，含设备名用于 EnumDisplaySettings。
type monitorInfoExW struct {
	cbSize     uint32
	rcMonitor  [4]int32 // RECT: left, top, right, bottom
	rcWork     [4]int32
	dwFlags    uint32
	DeviceName [32]uint16 // CCHDEVICENAME = 32
}

const enumCurrentSettings = 0xFFFFFFFF // ENUM_CURRENT_SETTINGS

// getDesktopRefreshRate 通过 GetDC(0) + GetDeviceCaps(VREFRESH) 获取主屏刷新率。
func getDesktopRefreshRate() int {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 60
	}
	defer procReleaseDC.Call(0, hdc)
	rate, _, _ := procGetDeviceCaps.Call(hdc, 116) // VREFRESH = 116
	if rate > 1 && rate <= 500 {
		return int(rate)
	}
	return 60
}

// getDisplayRefreshRate 获取指定显示器的真实刷新率。
// 使用 EnumDisplayMonitors 回调（与 screenshot 库相同的枚举顺序），
// 通过 GetMonitorInfoW 拿到设备名后调 EnumDisplaySettingsW 读取 dmDisplayFrequency。
func getDisplayRefreshRate(displayIndex int) int {
	var result int
	idx := 0

	cb := syscall.NewCallback(func(hMonitor, _, _, _ uintptr) uintptr {
		if idx == displayIndex {
			var mi monitorInfoExW
			mi.cbSize = uint32(unsafe.Sizeof(mi))
			ret, _, _ := procGetMonitorInfo.Call(hMonitor, uintptr(unsafe.Pointer(&mi)))
			if ret != 0 {
				result = readFreq(&mi.DeviceName[0])
			}
		}
		idx++
		return 1 // 继续枚举
	})

	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	if result > 0 {
		return result
	}
	return getDesktopRefreshRate()
}

// readFreq 对指定设备名调用 EnumDisplaySettingsW，读取 dmDisplayFrequency (offset 184)。
func readFreq(deviceName *uint16) int {
	var dm [256]byte
	*(*uint16)(unsafe.Pointer(&dm[68])) = 220 // dmSize

	ret, _, _ := procEnumDisplaySettings.Call(
		uintptr(unsafe.Pointer(deviceName)),
		enumCurrentSettings,
		uintptr(unsafe.Pointer(&dm[0])),
	)
	if ret != 0 {
		freq := *(*uint32)(unsafe.Pointer(&dm[184]))
		if freq > 1 && freq <= 500 {
			return int(freq)
		}
	}
	return 0
}
