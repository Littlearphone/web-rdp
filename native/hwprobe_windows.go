//go:build windows

package native

import (
	"fmt"
	"strings"
)

// ── 硬件编码器候选探测（地基）──
//
// 目标：找出"系统注册、能实际用起来"的 H.264 硬件编码器 MFT。
// 仅凭 MFTEnumEx 的 HARDWARE 标志不可靠——有些硬件 MFT（如本机
// NVIDIA H.264 Encoder MFT）按异步 MFT 运行，最简同步流程会报
// MF_E_TRANSFORM_ASYNC_LOCKED。因此这里对每个候选**用已验证可用的
// MFH264Encoder 真实编码多帧**，能同步逐帧产出才标记 usable。
//
// 供上层接入硬件编码器时选 primary（取第一个 usable），并把 unusable
// （异步/需 D3D11）留作后续异步模型候选。

// HWCandidate 描述一个硬件编码器候选及其可用性。
type HWCandidate struct {
	// Name 是编码器友好名（如 "NVIDIA H.264 Encoder MFT"）。
	Name string
	// Usable 表示能否用当前同步逐帧流程真实编码（非异步）。
	Usable bool
	// SyncErr 若 Usable=false 记录失败原因（用于诊断是否因异步/需 D3D11）。
	SyncErr string
}

// DetectHardwareCandidates 枚举系统硬件 H.264 编码器 MFT，逐个用 MFH264Encoder
// 真实编码验证，返回按"可用性优先 + 品牌优先级"排序的候选列表。
func DetectHardwareCandidates(width, height int) []HWCandidate {
	if width <= 0 || height <= 0 {
		width, height = 320, 240
	}
	acts, names, err := enumH264Activates()
	if err != nil {
		return nil
	}
	defer func() {
		for _, a := range acts {
			comRelease(a)
		}
	}()

	out := make([]HWCandidate, 0, len(acts))
	for i := range acts {
		c := HWCandidate{Name: names[i]}
		enc, err := tryEncoderSingleActivate(acts[i], width, height)
		if err != nil {
			c.SyncErr = err.Error()
		} else {
			// 真实编码 2 帧验证能同步产出
			ok := false
			for k := 0; k < 2; k++ {
				d, e := enc.Encode(makeNV12(width, height))
				if e == nil && len(d) > 0 {
					ok = true
					break
				}
			}
			enc.Close()
			if ok {
				c.Usable = true
			} else {
				c.SyncErr = "同步流程未产帧（可能为异步 MFT，需 D3D11/事件模型）"
			}
		}
		out = append(out, c)
	}
	stableSortHWCandidates(out)
	return out
}

// PickPrimaryCandidate 返回第一个 usable 的候选；无则返回 nil 指针值(-1 索引用 name 判)。
// 返回值：(name, usable, syncErr)。
func (c HWCandidate) Describe() string {
	if c.Usable {
		return fmt.Sprintf("%s [usable]", c.Name)
	}
	return fmt.Sprintf("%s [async/需D3D11: %s]", c.Name, c.SyncErr)
}

// stableSortHWCandidates 排序：usable 优先；同 usable 内按名称含品牌关键字优先（稳定）。
func stableSortHWCandidates(c []HWCandidate) {
	prio := func(name string) int {
		n := strings.ToLower(name)
		switch {
		case strings.Contains(n, "nvidia"):
			return 0
		case strings.Contains(n, "intel"):
			return 1
		case strings.Contains(n, "amd") || strings.Contains(n, "radeon"):
			return 2
		default:
			return 3
		}
	}
	for i := 1; i < len(c); i++ {
		for j := i; j > 0; j-- {
			a, b := c[j-1], c[j]
			if a.Usable && !b.Usable {
				break
			}
			if !a.Usable && b.Usable {
				c[j-1], c[j] = b, a
				continue
			}
			if prio(a.Name) > prio(b.Name) {
				c[j-1], c[j] = b, a
			} else {
				break
			}
		}
	}
}
