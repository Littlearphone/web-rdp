//go:build windows

package native

import (
	"fmt"
	"unsafe"
)

// configTransform 为 transform 配置 H.264 输出 + NV12 输入 + BEGIN_STREAMING。
func configTransform(tr unsafe.Pointer, width, height int) error {
	outMT, err := buildVideoType(0x34363248, uint32(width), uint32(height), 30, 1, true, 4_000_000)
	if err != nil {
		return err
	}
	err = tSetOutputType(tr, 0, outMT)
	comRelease(outMT)
	if err != nil {
		mt2, e2 := pickAvailableOutputType(tr, 0)
		if e2 != nil {
			return err
		}
		e2 = completeTypeSize(mt2, uint32(width), uint32(height), 30, 1)
		if e2 != nil {
			comRelease(mt2)
			return e2
		}
		e2 = tSetOutputType(tr, 0, mt2)
		comRelease(mt2)
		if e2 != nil {
			return e2
		}
	}
	inMT, err := buildVideoType(0x3231564e, uint32(width), uint32(height), 30, 1, true, 0) // NV12
	if err != nil {
		return err
	}
	err = tSetInputType(tr, 0, inMT)
	comRelease(inMT)
	if err != nil {
		return err
	}
	return tProcessMessage(tr, mftMsgNotifyBeginStreaming, 0)
}

// HWEncoderUnlock 探测：创建 D3D11 设备 + DXGI manager，激活 NVIDIA 硬件 H.264 MFT，
// 尝试 SetD3DManager 并 SetOutputType/SetInputType，验证能否从"异步锁定"转为可配置。
// 这是硬件编码路径的第一道可行性门：若 SetD3DManager 后 SetType 成功 → 硬件路径可行。
// 返回一段诊断文本。
func HWEncoderUnlock() string {
	dev, err := NewD3D11Device()
	if err != nil {
		return fmt.Sprintf("D3D11 设备失败: %v", err)
	}
	defer dev.Close()

	mgr, err := NewMFDXGIDeviceManager(dev)
	if err != nil {
		return fmt.Sprintf("DXGI manager 失败: %v", err)
	}
	defer mgr.Close()

	acts, names, err := enumH264Activates()
	if err != nil || len(acts) == 0 {
		return "无硬件 H.264 MFT"
	}
	defer func() {
		for _, a := range acts {
			comRelease(a)
		}
	}()

	// 优先用 NVIDIA 硬件（含 nvidia 名字）
	idx := -1
	for i, n := range names {
		if containsFoldStr(n, "nvidia") {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = 0
	}
	tr, err := activateTransform(acts[idx])
	if err != nil {
		return fmt.Sprintf("activate %s 失败: %v", names[idx], err)
	}
	defer comRelease(tr)

	// SetD3DManager
	if err := mgr.SetOnTransform(tr); err != nil {
		return fmt.Sprintf("[%s] SetD3DManager 失败: %v", names[idx], err)
	}
	// 尝试配置输出/输入类型（320x240 小尺寸便于探测）
	if err := configTransform(tr, 320, 240); err != nil {
		return fmt.Sprintf("[%s] SetD3DManager 后仍无法配置: %v", names[idx], err)
	}
	return fmt.Sprintf("[%s] SetD3DManager 后配置成功 → 硬件路径可行", names[idx])
}

func containsFoldStr(s, sub string) bool {
	return len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	ls := len(s)
	lsub := len(sub)
	if lsub == 0 || lsub > ls {
		return -1
	}
	for i := 0; i+ lsub <= ls; i++ {
		eq := true
		for j := 0; j < lsub; j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 32
			}
			if 'A' <= b && b <= 'Z' {
				b += 32
			}
			if a != b {
				eq = false
				break
			}
		}
		if eq {
			return i
		}
	}
	return -1
}
