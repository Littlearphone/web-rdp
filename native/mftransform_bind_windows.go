//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── Media Foundation IMFTransform H.264 编码会话（纯 syscall，无 cgo）──
//
// 目标：在无 ffmpeg 的前提下，把一帧 NV12 通过 Windows 自带的硬件 H.264
// 编码器 MFT（如 "NVIDIA H.264 Encoder MFT"）编码为 H.264 Annex B。
//
// vtable 槽位均按 Windows SDK 头文件核对（mfobjects.h / mftransform.h）：
//   IUnknown      : [0] QI [1] AddRef [2] Release
//   IMFAttributes : [3..32] 共 30 个方法
//   IMFTransform  (extends IUnknown, base=3):
//       [13] GetInputAvailableType  [14] GetOutputAvailableType
//       [15] SetInputType  [16] SetOutputType  [20] GetOutputStatus
//       [23] ProcessMessage  [24] ProcessInput  [25] ProcessOutput
//   IMFActivate (extends IMFAttributes, base=33):
//       [33] ActivateObject  [34] ShutdownObject  [35] DetachObject
//   IMFMediaType (extends IMFAttributes, base=33)：属性经 IMFAttributes setter 设置。
//   IMFSample  (extends IMFAttributes, base=33): [42] AddBuffer [45] GetTotalLength ...
//   IMFMediaBuffer (extends IUnknown, base=3): [3] Lock [4] Unlock [5] GetCurrentLength [6] SetCurrentLength

// MF 属性值 GUID 常量（取自头文件注释内联值）。
var (
	// MF_MT_MAJOR_TYPE = {48eba18e-f8c9-4687-bf11-0a74c9f96a8f}
	guidMTMajorType = mkGUID(0x48eba18e, 0xf8c9, 0x4687,
		[8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f})
	// MF_MT_SUBTYPE = {f7e34c9a-42e8-4714-b74b-cb29d72c35e5}
	guidMTSubtype = mkGUID(0xf7e34c9a, 0x42e8, 0x4714,
		[8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5})
	// MF_MT_FRAME_SIZE = {1652c33d-d6b2-4012-b834-72030849a37d}
	guidMTFrameSize = mkGUID(0x1652c33d, 0xd6b2, 0x4012,
		[8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d})
	// MF_MT_FRAME_RATE = {c459a2e8-3d2c-4e44-b132-fee5156c7bb0}
	guidMTFrameRate = mkGUID(0xc459a2e8, 0x3d2c, 0x4e44,
		[8]byte{0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0})
	// MF_MT_INTERLACE_MODE = {e2724bb8-e676-4806-b4b2-a8d6efb44ccd}
	guidMTInterlace = mkGUID(0xe2724bb8, 0xe676, 0x4806,
		[8]byte{0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd})
	// MF_MT_AVG_BITRATE = {20332624-fb0d-4d9e-bd0d-cbf6786c102e}
	guidMTAvgBitrate = mkGUID(0x20332624, 0xfb0d, 0x4d9e,
		[8]byte{0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e})
	// MF_MT_MPEG2_PROFILE = {ad76a80b-2d5c-4e0b-b375-64e520137036}（UINT32）
	guidMTMPEG2Profile = mkGUID(0xad76a80b, 0x2d5c, 0x4e0b,
		[8]byte{0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36})

	// IID_IMFTransform = {bf94c121-5b05-4e6f-8000-ba598961414d}
	iidIMFTransform = mkGUID(0xbf94c121, 0x5b05, 0x4e6f,
		[8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d})

	// 视频媒体类型基座 GUID：{00000000-0000-0010-8000-00aa00389b71}
	// 子类型 = {FOURCC(data1), 0x0000, 0x0010, 80 00 00 aa 00 38 9b 71}
)

// videoSubtype 由 FOURCC 生成媒体子类型 GUID（如 'NV12'=0x3231564E, 'H264'=0x34363248）。
func videoSubtype(fourcc uint32) guid {
	return mkGUID(fourcc, 0x0000, 0x0010,
		[8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})
}

// mfplat 工厂导出
var (
	procMFCreateMediaType = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSample    = mfplat.NewProc("MFCreateSample")
	procMFCreateMemBuffer = mfplat.NewProc("MFCreateMemoryBuffer")
)

// ── 通用 COM 调用（指针安全版）──

// comVtable 返回接口 iface 的 vtable 指针。
func comVtable(iface unsafe.Pointer) unsafe.Pointer {
	return *(*unsafe.Pointer)(iface)
}

// attrSetGUID 调用 IMFAttributes::SetGUID(iface,key,val)（槽位 24）。iface 可为 IMFMediaType*。
func attrSetGUID(iface unsafe.Pointer, key *guid, val *guid) error {
	fn := comMethod(iface, 24)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(val)))
	if r != 0 {
		return fmt.Errorf("SetGUID HRESULT=0x%x", r)
	}
	return nil
}

// attrSetUINT64 调用 IMFAttributes::SetUINT64（槽位 22）。
func attrSetUINT64(iface unsafe.Pointer, key *guid, val uint64) error {
	fn := comMethod(iface, 22)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface), uintptr(unsafe.Pointer(key)), uintptr(val))
	if r != 0 {
		return fmt.Errorf("SetUINT64 HRESULT=0x%x", r)
	}
	return nil
}

// attrSetUINT32 调用 IMFAttributes::SetUINT32（槽位 21）。
func attrSetUINT32(iface unsafe.Pointer, key *guid, val uint32) error {
	fn := comMethod(iface, 21)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface), uintptr(unsafe.Pointer(key)), uintptr(val))
	if r != 0 {
		return fmt.Errorf("SetUINT32 HRESULT=0x%x", r)
	}
	return nil
}

// attrGetGUID 调用 IMFAttributes::GetGUID（槽位 10）。
func attrGetGUID(iface unsafe.Pointer, key *guid, out *guid) error {
	fn := comMethod(iface, 10)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(out)))
	if r != 0 {
		return fmt.Errorf("GetGUID HRESULT=0x%x", r)
	}
	return nil
}

// attrGetUINT32 调用 IMFAttributes::GetUINT32（槽位 7）。
func attrGetUINT32(iface unsafe.Pointer, key *guid, out *uint32) error {
	fn := comMethod(iface, 7)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(out)))
	if r != 0 {
		return fmt.Errorf("GetUINT32 HRESULT=0x%x", r)
	}
	return nil
}

// makeMediaType 通过 MFCreateMediaType 创建一个空 IMFMediaType*。
func makeMediaType() (unsafe.Pointer, error) {
	var p unsafe.Pointer
	r, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&p)))
	if r != 0 {
		return nil, fmt.Errorf("MFCreateMediaType HRESULT=0x%x", r)
	}
	return p, nil
}

// mediaTypeIsVideoAndH264 返回该 IMFMediaType 是否为 Video + 指定 subtype。
func mediaTypeSubtype(t unsafe.Pointer) (guid, error) {
	var sub guid
	if err := attrGetGUID(t, &guidMTSubtype, &sub); err != nil {
		return guid{}, err
	}
	return sub, nil
}

// tTransformActivate 通过 IMFActivate::ActivateObject 请求 IMFTransform 接口。
// activate 是枚举得到的 IMFActivate*。返回的 transform 指针用后需 comRelease。
func activateTransform(activate unsafe.Pointer) (unsafe.Pointer, error) {
	fn := comMethod(activate, 33) // IMFActivate::ActivateObject
	var ppv unsafe.Pointer
	r, _, _ := syscall.SyscallN(fn, uintptr(activate),
		uintptr(unsafe.Pointer(&iidIMFTransform)),
		uintptr(unsafe.Pointer(&ppv)))
	if r != 0 {
		return nil, fmt.Errorf("ActivateObject HRESULT=0x%x", r)
	}
	if ppv == nil {
		return nil, fmt.Errorf("ActivateObject 返回空接口")
	}
	return ppv, nil
}
