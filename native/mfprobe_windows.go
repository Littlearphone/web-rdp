//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── Windows Media Foundation 纯 syscall 绑定 ──
//
// 项目默认 CGO_ENABLED=0 且交付希望不依赖外部 C 编译器，
// 因此这里用 syscall.NewLazyDLL + syscall.SyscallN 直接调用 mfplat.dll /
// ole32.dll，并对 MF 枚举返回的 COM 对象做极简 vtable 调用（GetString / Release）。
//
// vtable 槽位推导（依据 Windows SDK 头文件 mfobjects.h / mftransform.h）：
//   IUnknown      : [0] QueryInterface [1] AddRef [2] Release
//   IMFAttributes : [3] GetItem [4] GetItemType [5] CompareItem [6] Compare
//                   [7] GetUINT32 [8] GetUINT64 [9] GetDouble [10] GetGUID
//                   [11] GetStringLength [12] GetString ...
//   IMFActivate   继承 IMFAttributes（枚举对象为 IMFActivate*）
//   因此 GetString 在 vtable 槽位 12，Release 在槽位 2。

// GUID 以内存中的原始 16 字节小端布局表示（与 C GUID 一致），
// 直接取 &[0] 作为 REFGUID 传给 syscall。
type guid [16]byte

func mkGUID(data1 uint32, data2, data3 uint16, data4 [8]byte) guid {
	var g guid
	// data1 (LE), data2 (LE), data3 (LE), data4 (原样)
	g[0] = byte(data1)
	g[1] = byte(data1 >> 8)
	g[2] = byte(data1 >> 16)
	g[3] = byte(data1 >> 24)
	g[4] = byte(data2)
	g[5] = byte(data2 >> 8)
	g[6] = byte(data3)
	g[7] = byte(data3 >> 8)
	copy(g[8:16], data4[:])
	return g
}

// ── 需要的 MF GUID ──
var (
	// MFT_CATEGORY_VIDEO_ENCODER = {f79eac7d-e545-4387-bdee-d647d7bde42a}
	guidVideoEncoder = mkGUID(0xf79eac7d, 0xe545, 0x4387,
		[8]byte{0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a})

	// MFT_FRIENDLY_NAME_Attribute = {314ffbae-5b41-4c95-9c19-4e7d586face3}
	guidFriendlyName = mkGUID(0x314ffbae, 0x5b41, 0x4c95,
		[8]byte{0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3})

	// MFMediaType_Video = {73646976-0000-0010-8000-00aa00389b71}（major type）
	guidMediaTypeVideo = mkGUID(0x73646976, 0x0000, 0x0010,
		[8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})

	// MFVideoFormat_H264 = FOURCC('H264')=0x34363248 + media GUID 基座。
	// 媒体子类型 GUID = {FOURCC, 0x0000, 0x0010, 80 00 00 aa 00 38 9b 71}。
	guidVideoH264 = mkGUID(0x34363248, 0x0000, 0x0010,
		[8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})
)

// mftRegisterTypeInfo 对应 C 的 MFT_REGISTER_TYPE_INFO { GUID major; GUID subtype }。
type mftRegisterTypeInfo struct {
	majorType guid
	subtype   guid
}

// mfplat.dll 导出
var (
	mfplat            = syscall.NewLazyDLL("mfplat.dll")
	procMFStartup     = mfplat.NewProc("MFStartup")
	procMFShutdown    = mfplat.NewProc("MFShutdown")
	procMFTEnumEx     = mfplat.NewProc("MFTEnumEx")
	ole32             = syscall.NewLazyDLL("ole32.dll")
	procCoTaskMemFree = ole32.NewProc("CoTaskMemFree")
)

// MFT_ENUM_FLAG 位掩码（来自 mftransform.h）
const (
	mftEnumSyncMFT    = 0x00000001
	mftEnumAsync      = 0x00000002
	mftEnumHardware   = 0x00000004
	mftEnumLocalMFT   = 0x00000008
	mftEnumTranscode  = 0x00000020
	mftEnumSortFilter = 0x00000040
)

// MF_API_VERSION
const mfAPIVersion = 0x0070

// mfStartup 初始化 Media Foundation。返回是否成功。
func mfStartup() bool {
	r, _, _ := procMFStartup.Call(uintptr(mfAPIVersion), 0) // MFSTARTUP_FULL = 0
	return r == 0 // S_OK
}

// mfShutdown 关闭 Media Foundation。
func mfShutdown() {
	_, _, _ = procMFShutdown.Call()
}

// coTaskMemFree 释放 MFTEnumEx 返回的 IMFActivate 数组。
func coTaskMemFree(p unsafe.Pointer) {
	_, _, _ = procCoTaskMemFree.Call(uintptr(p))
}

// comMethod 返回 COM 接口 iface 的 vtable 槽位 slot 上的方法函数指针。
// iface 在整个调用期间保持存活（指向堆/COM 分配的对象），
// 仅在作为 syscall 实参时临时转成 uintptr，符合 unsafe.Pointer 安全规则。
func comMethod(iface unsafe.Pointer, slot int) uintptr {
	// 接口内存布局：首字段是 *vtable。
	vtblPtr := *(*unsafe.Pointer)(iface)
	// 把 vtable 看作 uintptr 数组切片，按槽位取值，避免手写指针运算。
	tab := unsafe.Slice((*uintptr)(vtblPtr), slot+1)
	return tab[slot]
}

// comRelease 调用 IUnknown::Release（vtable 槽位 2）。返回引用计数值。
func comRelease(iface unsafe.Pointer) uintptr {
	fn := comMethod(iface, 2)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface))
	return r
}

// comGetString 调用 IMFAttributes::GetString(iface, key, ...)（vtable 槽位 12）。
// 返回 S_OK 且读取成功时返回 UTF-16 字符串。
func comGetString(iface unsafe.Pointer, key *guid) (string, bool) {
	fn := comMethod(iface, 12)
	buf := make([]uint16, 256)
	var cch uint32
	// GetString(this, REFGUID, LPWSTR, cchBufSize, UINT32* pcchLength)
	r, _, _ := syscall.SyscallN(fn, uintptr(iface),
		uintptr(unsafe.Pointer(key)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&cch)))
	if r != 0 {
		return "", false
	}
	// buf 是 UTF-16 缓冲；截取到 cch。
	return syscall.UTF16ToString(buf[:cch]), true
}

// utf16ToString 把 UTF-16 码元转为 string（syscall.UTF16ToString 依赖 unsafe，
// 这里手动实现，避免额外依赖即可）。
func utf16ToString(u []uint16) string {
	return syscall.UTF16ToString(u)
}

// ── 枚举硬件视频编码器 MFT ──

// EnumHardwareVideoEncoders 调用 MFTEnumEx 枚举"硬件 + 同步"类别的视频编码器 MFT。
// 返回每个编码器的友好名称。失败时返回 error（便于调用方回退 ffmpeg）。
func EnumHardwareVideoEncoders() ([]string, error) {
	if !mfStartup() {
		return nil, fmt.Errorf("MFStartup 失败")
	}
	defer mfShutdown()

	flags := uintptr(mftEnumSyncMFT | mftEnumHardware | mftEnumLocalMFT)

	var pArr unsafe.Pointer // IMFActivate** pppMFTActivate（接收数组首指针，保持指针型避免 unsafe.Pointer 往返）
	var count uint32
	// 限定输出类型为 H.264 视频，仅枚举能产出 H.264 的编码器 MFT。
	outType := mftRegisterTypeInfo{majorType: guidMediaTypeVideo, subtype: guidVideoH264}

	// MFTEnumEx(REFGUID category, UINT32 flags, pInputType, pOutputType, &arr, &count)
	r, _, _ := procMFTEnumEx.Call(
		uintptr(unsafe.Pointer(&guidVideoEncoder)),
		flags,
		0, // pInputType = NULL（不限输入类型）
		uintptr(unsafe.Pointer(&outType)),
		uintptr(unsafe.Pointer(&pArr)),
		uintptr(unsafe.Pointer(&count)))
	if r != 0 {
		return nil, fmt.Errorf("MFTEnumEx 失败 HRESULT=0x%x", r)
	}
	if count == 0 || pArr == nil {
		return nil, nil // 无硬件 H.264 视频编码器 MFT
	}

	names := make([]string, 0, count)
	// 把返回的 IMFActivate* 数组视为 unsafe.Pointer 切片逐个读取（指针型索引，规避 checkptr）。
	acts := unsafe.Slice((*unsafe.Pointer)(pArr), count)
	for _, obj := range acts {
		if obj == nil {
			continue
		}
		// 读友好名（固定 256 UTF-16 缓冲 + GetString）。
		if s, ok := comGetString(obj, &guidFriendlyName); ok {
			names = append(names, s)
		} else {
			names = append(names, "encoder")
		}
		comRelease(obj) // Release 每个 IMFActivate
	}
	coTaskMemFree(pArr)
	return names, nil
}
