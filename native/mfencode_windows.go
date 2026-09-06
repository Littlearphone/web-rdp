//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── 单帧 H.264 编码冒烟（头less，纯 syscall）──
//
// 流程（对齐 Windows Media Foundation 硬件编码器惯例）：
//   1) MFTEnumEx 枚举硬件视频编码器 MFT，取首选（优先 NVIDIA H.264）。
//   2) IMFActivate::ActivateObject → IMFTransform。
//   3) 设置 output type = H264（Video + subtype H264 + 帧尺寸/帧率）。
//   4) 设置 input type = NV12（Video + subtype NV12 + 帧尺寸/帧率/渐进）。
//   5) IMFTransform::ProcessMessage(BEGIN_STREAMING)。
//   6) 构造 IMFSample 装入一帧 NV12 → ProcessInput。
//   7) ProcessMessage(COMMAND_DRAIN) 后循环 ProcessOutput 取回编码数据，
//      把所有 H.264 样本（含 SPS/PPS/IDR）拼为 Annex B。
//   8) 校验：含 NAL 起始码且至少一个 IDR。

// MFT 常量
const (
	// MFT_OUTPUT_STATUS_SAMPLE_READY = 0x1
	// ProcessMessage 消息值
	mftMsgNotifyBeginStreaming = 0x10000000
	mftMsgNotifyEndStreaming   = 0x10000001
	mftMsgCommandDrain         = 0x1
	mftMsgCommandFlush         = 0x0

	// MFVideoInterlace_Progressive = 2
	mfVideoInterlaceProgressive = 2
)

// mftOutputDataBuffer 对应 C 的 MFT_OUTPUT_DATA_BUFFER。
type mftOutputDataBuffer struct {
	streamID uint32
	pSample  unsafe.Pointer // IMFSample*
	dwStatus uint32
	pEvents  unsafe.Pointer // IMFCollection*
}

// ── IMFTransform 方法调用（槽位：15 SetInputType, 16 SetOutputType,
//    23 ProcessMessage, 24 ProcessInput, 25 ProcessOutput）──

func tSetOutputType(t unsafe.Pointer, streamID uint32, mt unsafe.Pointer) error {
	fn := comMethod(t, 16)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(streamID), uintptr(mt), 0)
	if r != 0 {
		return fmt.Errorf("SetOutputType HRESULT=0x%x", r)
	}
	return nil
}

func tSetInputType(t unsafe.Pointer, streamID uint32, mt unsafe.Pointer) error {
	fn := comMethod(t, 15)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(streamID), uintptr(mt), 0)
	if r != 0 {
		return fmt.Errorf("SetInputType HRESULT=0x%x", r)
	}
	return nil
}

// tGetOutputAvailableType 调用 GetOutputAvailableType（槽位 14）。
func tGetOutputAvailableType(t unsafe.Pointer, streamID, dwIndex uint32) (unsafe.Pointer, error) {
	var mt unsafe.Pointer
	fn := comMethod(t, 14)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(streamID), uintptr(dwIndex), uintptr(unsafe.Pointer(&mt)))
	if r != 0 {
		return nil, fmt.Errorf("GetOutputAvailableType[%d] HRESULT=0x%x", dwIndex, r)
	}
	return mt, nil
}

// tGetOutputCurrentType 调用 GetOutputCurrentType（槽位 18）。
func tGetOutputCurrentType(t unsafe.Pointer, streamID uint32) (unsafe.Pointer, error) {
	var mt unsafe.Pointer
	fn := comMethod(t, 18)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(streamID), uintptr(unsafe.Pointer(&mt)))
	if r != 0 {
		return nil, fmt.Errorf("GetOutputCurrentType HRESULT=0x%x", r)
	}
	return mt, nil
}

// subtypeOf 读取媒体类型的 MF_MT_SUBTYPE。
func subtypeOf(mt unsafe.Pointer) (guid, error) {
	var sub guid
	if err := attrGetGUID(mt, &guidMTSubtype, &sub); err != nil {
		return guid{}, err
	}
	return sub, nil
}

func tProcessMessage(t unsafe.Pointer, msg uint32, param uintptr) error {
	fn := comMethod(t, 23)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(msg), param)
	if r != 0 {
		return fmt.Errorf("ProcessMessage(0x%x) HRESULT=0x%x", msg, r)
	}
	return nil
}

func tProcessInput(t unsafe.Pointer, streamID uint32, sample unsafe.Pointer, flags uint32) error {
	fn := comMethod(t, 24)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(streamID), uintptr(sample), uintptr(flags))
	if r != 0 {
		return fmt.Errorf("ProcessInput HRESULT=0x%x", r)
	}
	return nil
}

// tProcessOutput 尝试取一帧编码输出。返回 sample（可能为 nil 表示无就绪帧）。
// 输出缓冲区由调用方创建/释放。
func tProcessOutput(t unsafe.Pointer, ob *mftOutputDataBuffer) (unsafe.Pointer, error) {
	// 单输出流：count=1
	var status uint32
	fn := comMethod(t, 25)
	// ProcessOutput(dwFlags, cOutputBufferCount=1, pOutputSamples=&ob, pdwStatus)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), 0, 1,
		uintptr(unsafe.Pointer(ob)), uintptr(unsafe.Pointer(&status)))
	// MF_E_TRANSFORM_NEED_MORE_INPUT = 0xC00D6D72：无就绪输出帧，正常需更多输入。
	if uint32(r) == 0xc00d6d72 {
		return nil, nil
	}
	if r != 0 {
		return nil, fmt.Errorf("ProcessOutput HRESULT=0x%x", r)
	}
	return ob.pSample, nil
}

// ── 媒体类型构造 ──

// buildVideoType 构造视频媒体类型：major=Video，subtype=指定 FOURCC，并设帧尺寸/帧率。
// interlace 为 true 时写 MF_MT_INTERLACE_MODE=Progressive；bitrate>0 时写 MF_MT_AVG_BITRATE
//（微软软件 H.264 编码器 MFT 的 SetOutputType 要求 AVG_BITRATE，缺省报 ATTRIBUTENOTFOUND）。
func buildVideoType(subtypeFOURCC uint32, width, height uint32, num, den uint32, interlace bool, bitrate uint32) (unsafe.Pointer, error) {
	mt, err := makeMediaType()
	if err != nil {
		return nil, err
	}
	sub := videoSubtype(subtypeFOURCC)
	if err := attrSetGUID(mt, &guidMTMajorType, &guidMediaTypeVideo); err != nil {
		comRelease(mt)
		return nil, err
	}
	if err := attrSetGUID(mt, &guidMTSubtype, &sub); err != nil {
		comRelease(mt)
		return nil, err
	}
	// MF_MT_FRAME_SIZE: UINT64 = (width<<32)|height
	frameSize := uint64(width)<<32 | uint64(height)
	if err := attrSetUINT64(mt, &guidMTFrameSize, frameSize); err != nil {
		comRelease(mt)
		return nil, err
	}
	// MF_MT_FRAME_RATE: UINT64 = (num<<32)|den
	frameRate := uint64(num)<<32 | uint64(den)
	if err := attrSetUINT64(mt, &guidMTFrameRate, frameRate); err != nil {
		comRelease(mt)
		return nil, err
	}
	if interlace {
		if err := attrSetUINT32(mt, &guidMTInterlace, mfVideoInterlaceProgressive); err != nil {
			comRelease(mt)
			return nil, err
		}
	}
	if bitrate > 0 {
		if err := attrSetUINT32(mt, &guidMTAvgBitrate, bitrate); err != nil {
			comRelease(mt)
			return nil, err
		}
	}
	return mt, nil
}

// ── IMFSample / IMFMediaBuffer 封装 ──

// makeSample 创建空 IMFSample。
func makeSample() (unsafe.Pointer, error) {
	var p unsafe.Pointer
	r, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&p)))
	if r != 0 {
		return nil, fmt.Errorf("MFCreateSample HRESULT=0x%x", r)
	}
	return p, nil
}

// makeBuffer 创建 cbMaxLength 大小的内存 IMFMediaBuffer。
func makeBuffer(cbMaxLength int) (unsafe.Pointer, error) {
	var p unsafe.Pointer
	r, _, _ := procMFCreateMemBuffer.Call(uintptr(cbMaxLength), uintptr(unsafe.Pointer(&p)))
	if r != 0 {
		return nil, fmt.Errorf("MFCreateMemoryBuffer HRESULT=0x%x", r)
	}
	return p, nil
}

// bufferSetCurrentLength 调用 IMFMediaBuffer::SetCurrentLength（槽位 6）。
func bufferSetCurrentLength(b unsafe.Pointer, n int) error {
	fn := comMethod(b, 6)
	r, _, _ := syscall.SyscallN(fn, uintptr(b), uintptr(n))
	if r != 0 {
		return fmt.Errorf("SetCurrentLength HRESULT=0x%x", r)
	}
	return nil
}

// sampleAddBuffer 调用 IMFSample::AddBuffer（槽位 42）。
func sampleAddBuffer(s, b unsafe.Pointer) error {
	fn := comMethod(s, 42)
	r, _, _ := syscall.SyscallN(fn, uintptr(s), uintptr(b))
	if r != 0 {
		return fmt.Errorf("IMFSample::AddBuffer HRESULT=0x%x", r)
	}
	return nil
}

// sampleSetTime 调用 IMFSample::SetSampleTime（槽位 36）。单位 100ns。
func sampleSetTime(s unsafe.Pointer, t100ns int64) error {
	fn := comMethod(s, 36)
	r, _, _ := syscall.SyscallN(fn, uintptr(s), uintptr(t100ns))
	if r != 0 {
		return fmt.Errorf("IMFSample::SetSampleTime HRESULT=0x%x", r)
	}
	return nil
}

// sampleSetDuration 调用 IMFSample::SetSampleDuration（槽位 38）。单位 100ns。
func sampleSetDuration(s unsafe.Pointer, d100ns int64) error {
	fn := comMethod(s, 38)
	r, _, _ := syscall.SyscallN(fn, uintptr(s), uintptr(d100ns))
	if r != 0 {
		return fmt.Errorf("IMFSample::SetSampleDuration HRESULT=0x%x", r)
	}
	return nil
}

// sampleGetTotalLength 调用 IMFSample::GetTotalLength（槽位 45）。
func sampleGetTotalLength(s unsafe.Pointer) (int, error) {
	var n uint32
	fn := comMethod(s, 45)
	r, _, _ := syscall.SyscallN(fn, uintptr(s), uintptr(unsafe.Pointer(&n)))
	if r != 0 {
		return 0, fmt.Errorf("GetTotalLength HRESULT=0x%x", r)
	}
	return int(n), nil
}

// sampleGetBufferByIndex 调用 IMFSample::GetBufferByIndex（槽位 40），返回第 idx 个 buffer。
func sampleGetBufferByIndex(s unsafe.Pointer, idx uint32) (unsafe.Pointer, error) {
	var b unsafe.Pointer
	fn := comMethod(s, 40)
	r, _, _ := syscall.SyscallN(fn, uintptr(s), uintptr(idx), uintptr(unsafe.Pointer(&b)))
	if r != 0 {
		return nil, fmt.Errorf("GetBufferByIndex HRESULT=0x%x", r)
	}
	return b, nil
}

// bufferCopyOut 读取 buffer 内容到 Go []byte（Lock → copy → Unlock）。
func bufferCopyOut(b unsafe.Pointer) ([]byte, error) {
	// IMFMediaBuffer::Lock（槽位 3）
	fnLock := comMethod(b, 3)
	var ppb unsafe.Pointer
	var maxLen, curLen uint32
	r, _, _ := syscall.SyscallN(fnLock, uintptr(b), uintptr(unsafe.Pointer(&ppb)),
		uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&curLen)))
	if r != 0 {
		return nil, fmt.Errorf("Lock HRESULT=0x%x", r)
	}
	defer func() {
		fnUnlock := comMethod(b, 4)
		_, _, _ = syscall.SyscallN(fnUnlock, uintptr(b))
	}()
	if curLen == 0 {
		return nil, nil
	}
	dst := make([]byte, curLen)
	src := unsafe.Slice((*byte)(ppb), int(curLen))
	copy(dst, src)
	return dst, nil
}
