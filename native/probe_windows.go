//go:build windows

package native

import (
	"fmt"
	"sync"
)

var (
	h264AvailOnce sync.Once
	h264AvailCache bool
)

// H264Available 报告进程内 MF 是否可用 H.264 编码器 MFT（探测一次并缓存）。
func H264Available() bool {
	h264AvailOnce.Do(func() {
		names, err := EnumHardwareVideoEncoders()
		h264AvailCache = err == nil && len(names) > 0
	})
	return h264AvailCache
}

// DetectBackend 运行时探测可用的进程内编码后端，返回决策。
// 顺序：
//   1) 枚举硬件视频编码器 MFT（MF 路径，跨厂商通用）→ 有则 primary=MF
//   2) 尚无 ffmpeg 之外的硬件编码后端可实例化 → native 不可用（沿用 ffmpeg 主链）
//
// 说明：NVENC/AMF/VPL 直调需 cgo/额外 ABI 桥，属阶段二，此探测暂不枚举。
// 调用方应据返回值决定是否启用 native 路径；返回前不擅自改全局状态。
func DetectBackend() *Fallback {
	// 优先探测 MF 硬件编码器 MFT（最通用、跨 NVIDIA/AMD/Intel）。
	names, err := EnumHardwareVideoEncoders()
	if err != nil {
		return &Fallback{
			Enabled: false,
			Primary: BackendUnknown,
			Reason:  "MF 枚举失败: " + err.Error(),
		}
	}
	if len(names) > 0 {
		return &Fallback{
			Enabled:      true,
			Primary:      BackendMF,
			HardwareMFTs: names,
			Reason:       "发现硬件编码器 MFT",
		}
	}
	return &Fallback{
		Enabled: false,
		Primary: BackendUnknown,
		Reason:  "未发现硬件视频编码器 MFT，沿用 ffmpeg 主链",
	}
}

// Probe 打印一份可供人工核对/排障的后端探测报告。返回探测到的 GPU 厂商。
func Probe() Vendor {
	// 进程内 GPU 厂商探测（EnumDisplayDevices，替代 wmic）。
	// 注：真实 DXGI 适配器枚举（CreateDXGIFactory1→EnumAdapters）在虚拟/远程显示会话下
	// 不可靠，故此处用已验证可用的 DetectGPU；DXGI 路径留作普通桌面上的增强项。
	v := DetectGPU()
	fmt.Printf("GPU 厂商: %s\n", v)

	fb := DetectBackend()
	fmt.Printf("Native 编码后端: %s\n", fb.String())
	if len(fb.HardwareMFTs) > 0 {
		fmt.Println("可用硬件编码器 MFT:")
		for _, n := range fb.HardwareMFTs {
			fmt.Printf("  - %s\n", n)
		}
	}
	return v
}
