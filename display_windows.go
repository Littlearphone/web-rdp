package main

import (
	"sync"
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

// ── 刷新率缓存 ──
// 避免每次查询都创建 syscall.NewCallback（Go 在 Windows 上限 2000 个 callback，永不释放）。
// 首次枚举所有显示器后缓存，后续调用直接查表。
var (
	refreshRateCache   = make(map[int]int)
	refreshRateCacheMu sync.Mutex
)

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
// 使用 EnumDisplayMonitors + GetMonitorInfoW + EnumDisplaySettingsW 读取 dmDisplayFrequency。
// 首次调用时枚举所有显示器并缓存，后续调用直接返回缓存值。
func getDisplayRefreshRate(displayIndex int) int {
	// 缓存命中直接返回
	refreshRateCacheMu.Lock()
	if r, ok := refreshRateCache[displayIndex]; ok {
		refreshRateCacheMu.Unlock()
		if r > 0 {
			return r
		}
		return getDesktopRefreshRate()
	}

	// 未命中：枚举所有显示器，一次性填充缓存
	idx := 0
	cb := syscall.NewCallback(func(hMonitor, _, _, _ uintptr) uintptr {
		var mi monitorInfoExW
		mi.cbSize = uint32(unsafe.Sizeof(mi))
		ret, _, _ := procGetMonitorInfo.Call(hMonitor, uintptr(unsafe.Pointer(&mi)))
		if ret != 0 {
			refreshRateCache[idx] = readFreq(&mi.DeviceName[0])
		}
		idx++
		return 1 // 继续枚举
	})

	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	refreshRateCacheMu.Unlock()

	// 枚举完成后再次查缓存
	refreshRateCacheMu.Lock()
	r, ok := refreshRateCache[displayIndex]
	refreshRateCacheMu.Unlock()
	if ok && r > 0 {
		return r
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
