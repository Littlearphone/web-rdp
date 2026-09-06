// Package native 提供不依赖 ffmpeg 的进程内桌面采集与硬件编码后端。
//
// 设计目标（详见 docs/native-no-ffmpeg-design.md）：
//   - 用 Windows 原生能力（DXGI 直捕 + Media Foundation / 厂商 SDK 硬件编码）
//     替代 ffmpeg 子进程，实现高帧率 / 低带宽 / 低延迟。
//   - 以 Encoder 接口 + 回退状态机作为骨架，把后端与调用方（ws.go 会话循环）解耦，
//     让 ffmpeg 路径与 native 路径可切换，且默认仍以现有 ffmpeg 为主链（零回归）。
//
// 当前阶段（阶段一收敛）只落地可编译 + 可降级验证的地基：
//   - Encoder 接口与后端标识（本文件）
//   - 运行时 GPU 厂商与可用编码后端探测（vendor_windows.go / probe_windows.go）
//   - MFTEnumEx 枚举可用硬件 H.264 编码器 MFT（mfprobe_windows.go，纯 syscall）
//   - 完整 IMFTransform 编码会话与 DXGI 直捕真实接入留待后续轮次。
package native

import "fmt"

// Backend 标识一个进程内编码后端。
type Backend int

const (
	// BackendUnknown 尚未确定后端。
	BackendUnknown Backend = iota
	// BackendMF 走 Windows Media Foundation 硬件编码器 MFT（跨厂商，系统自带）。
	BackendMF
	// BackendNVENC 直调 NVIDIA NvEncodeAPI（阶段二，需 cgo 或进一步 ABI 桥）。
	BackendNVENC
	// BackendAMF 直调 AMD AMF（阶段二）。
	BackendAMF
	// BackendVPL 直调 Intel OneVPL/QSV（阶段二）。
	BackendVPL
	// BackendFFmpeg 走现有 ffmpeg 子进程（默认主链，保留作兜底）。
	BackendFFmpeg
	// BackendCPUJPEG 纯 Go CPU JPEG（最终回退，仅 MJPEG 模式）。
	BackendCPUJPEG
)

func (b Backend) String() string {
	switch b {
	case BackendMF:
		return "mediafoundation-mft"
	case BackendNVENC:
		return "nvenc"
	case BackendAMF:
		return "amf"
	case BackendVPL:
		return "vpl"
	case BackendFFmpeg:
		return "ffmpeg"
	case BackendCPUJPEG:
		return "cpu-jpeg"
	default:
		return "unknown"
	}
}

// VideoFrame 表示一帧待编码图像。dirty 仅为占位说明，后续接入 DXGI 时再定型。
type VideoFrame struct {
	// DisplayID 是源显示器 ID（对应现有 displayID 语义）。
	DisplayID int
	// Width / Height 是编码目标分辨率（已含 downscale 决策）。
	Width  int
	Height int
	// Data 暂为占位。最终形态可能是 D3D11 纹理句柄（zero-copy）或 BGRA 内存。
	Data []byte
}

// Encoder 是统一的进程内编码器出口。产出必须是与 ffmpeg H.264 Annex B
// 逐字节兼容的 NAL 流，确保前端 h264.ts / pion WebRTC / WS 契约不变。
type Encoder interface {
	// Name 返回编码器后端标识。
	Name() Backend

	// Init 创建/初始化编码器会话。参数语义对齐现有 ffmpeg 路径。
	//   - displayID: 显示器 ID（每显示器独立会话，对应现有会话池语义）
	//   - quality:   用户画质滑块 30-100
	//   - maxW:      目标最大宽度（0 = 原始分辨率）
	//   - fps:       目标帧率（0 = 自动跟随显示器刷新率）
	Init(displayID, quality, maxW, fps int) error

	// Encode 编码一帧，返回 Annex B 字节。isIDR 指示是否为关键帧。
	Encode(frame *VideoFrame) (annexB []byte, isIDR bool, err error)

	// SPSPPS 返回缓存的 SPS/PPS NAL（不含起始码），供新订阅者 preConfigure。
	// 尚未就绪时返回 nil, nil。
	SPSPPS() (sps, pps []byte)

	// SetQuality 动态调整画质（滑块 30-100），尽量不重建会话。
	SetQuality(q int)

	// Reset 参数变化时重建会话（对齐现有 restartFFmpeg 语义）。
	Reset(quality, maxW, fps int) error

	// Close 释放编码器资源。
	Close()
}

// Verbose 控制 native 包内部的诊断输出（逐编码器失败原因、可用类型 dump）。
// 默认 false 保持单测安静；CLI 探测入口可置 true 以便人工排障。
var Verbose = false

// QualityToCQ 与 ffmpeg_pipeline.go 保持一致：滑块 30-100 → 编码器 CQ/CRF 值。
func QualityToCQ(q int) int {
	var hq int
	if q >= 75 {
		hq = 35 - (q-75)*10/25
	} else {
		hq = 35 + (75-q)*13/45
	}
	if hq < 1 {
		return 1
	}
	if hq > 51 {
		return 51
	}
	return hq
}

// Vendor 标识探测到的 GPU 主供应商。
type Vendor int

const (
	VendorUnknown Vendor = iota
	VendorNVIDIA
	VendorAMD
	VendorIntel
	VendorMixed
)

func (v Vendor) String() string {
	switch v {
	case VendorNVIDIA:
		return "nvidia"
	case VendorAMD:
		return "amd"
	case VendorIntel:
		return "intel"
	case VendorMixed:
		return "mixed"
	default:
		return "unknown"
	}
}

// Fallback 描述从当前可用后端中选择的决策结果。
type Fallback struct {
	// Enabled 表示是否存在进程内（非 ffmpeg）硬件编码后端可用。
	Enabled bool
	// Primary 首选后端。
	Primary Backend
	// HardwareMFTs 枚举到的硬件编码器 MFT 友好名列表（MF 路径探测结果）。
	HardwareMFTs []string
	// Reason 说明决策依据（探测失败时给出原因）。
	Reason string
}

func (f *Fallback) String() string {
	if !f.Enabled {
		return fmt.Sprintf("native 不可用: %s", f.Reason)
	}
	return fmt.Sprintf("native 可用: primary=%s, 硬件MFT=%v", f.Primary, f.HardwareMFTs)
}
