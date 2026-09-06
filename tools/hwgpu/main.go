// tools/hwgpu：进程内 GPU 编码实验 - 基础环境自检
//
// 纯 Go、不依赖 ffmpeg。验证硬件编码所需的底层栈是否就绪：
//   1) D3D11 硬件设备能否创建
//   2) DXGI Desktop Duplication 能否抓到真实桌面帧
//   3) MF DXGI DeviceManager 能否建立
//   4) 系统硬件 H.264 MFT 候选及其 async/usable 分类
//
// 用法：go run ./tools/hwgpu
package main

import (
	"fmt"

	"web-rdp/native"
)

func main() {
	fmt.Println("=== 进程内 GPU 编码基础自检 ===")

	// 1) D3D11 设备
	dev, err := native.NewD3D11Device()
	if err != nil {
		fmt.Printf("✗ D3D11 硬件设备创建失败: %v\n", err)
	} else {
		fmt.Printf("✓ D3D11 硬件设备创建成功\n")
		dev.Close()
	}

	// 2) MF DXGI DeviceManager
	dev2, err := native.NewD3D11Device()
	if err == nil {
		mgr, err := native.NewMFDXGIDeviceManager(dev2)
		if err != nil {
			fmt.Printf("✗ MF DXGI DeviceManager 建立失败: %v\n", err)
		} else {
			fmt.Printf("✓ MF DXGI DeviceManager 建立成功 (reset token)\n")
			mgr.Close()
		}
		dev2.Close()
	}

	// 3) DXGI 桌面直捕
	cap, err := native.NewDesktopCapture(0)
	if err != nil {
		fmt.Printf("✗ DXGI 桌面直捕打开失败: %v\n", err)
	} else {
		bgra, w, h, err := cap.AcquireBGRA(3000)
		if err != nil || bgra == nil {
			fmt.Printf("✗ 桌面直捕取帧失败: %v\n", err)
		} else {
			fmt.Printf("✓ DXGI 直捕取到 %dx%d (%d B)\n", w, h, len(bgra))
		}
		cap.Close()
	}

	// 4) 硬件 H.264 MFT 候选
	fmt.Println("--- 硬件 H.264 MFT 候选分类 ---")
	cands := native.DetectHardwareCandidates(0, 0)
	for _, c := range cands {
		fmt.Printf("  %s\n", c.Describe())
	}

	fmt.Println("=== 自检结束 ===")
}
