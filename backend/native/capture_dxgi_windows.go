//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── DXGI Desktop Duplication 直捕（纯 syscall，无 cgo）──
//
// 经 D3D11 设备 → IDXGIDevice::GetAdapter → IDXGIAdapter::EnumOutputs →
// IDXGIOutput1::DuplicateOutput → IDXGIOutputDuplication::AcquireNextFrame
// 取得桌面帧 D3D 纹理，拷贝到 staging 纹理并 Map 回读为 BGRA 字节。
//
// vtable 槽位（均按 SDK 头文件核对）：
//   IDXGIDevice(extends IDXGIObject=IUnknown+4): [7] GetAdapter
//   IDXGIAdapter(extends IDXGIObject):          [7] EnumOutputs
//   IDXGIOutput1(extends IDXGIOutput=IDXGIObject+12): [22] DuplicateOutput
//   IDXGIOutputDuplication(extends IDXGIObject): [7]GetDesc [8]AcquireNextFrame [14]ReleaseFrame
//   ID3D11DeviceContext(extends IUnknown+DeviceChild4): [14] Map [15] Unmap [47] CopyResource
//   ID3D11Texture2D(extends ID3D11Resource=...): 在从 duplication 得到的 IDXGIResource 上 QI 获取

// DXGI IID
var (
	// IID_IDXGIDevice = {54ec77fa-1377-44e6-8c32-88fd5f44c84c}
	iidIDXGIDevice = mkGUID(0x54ec77fa, 0x1377, 0x44e6,
		[8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c})
	// IID_IDXGIOutput1 = {00cddea8-939b-4b83-a340-a685226666cc}
	iidIDXGIOutput1 = mkGUID(0x00cddea8, 0x939b, 0x4b83,
		[8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc})
	// IID_IDXGIResource = {035f3ab4-482e-4e50-b41f-8a7f8bd8960b}
	iidIDXGIResource = mkGUID(0x035f3ab4, 0x482e, 0x4e50,
		[8]byte{0xb4, 0x1f, 0x8a, 0x7f, 0x8b, 0xd8, 0x96, 0x0b})
	// IID_ID3D11Texture2D = {6f15aaf2-d208-4e89-9ab4-489535d34f9c}
	iidD3D11Texture2D = mkGUID(0x6f15aaf2, 0xd208, 0x4e89,
		[8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c})
)

// dxgiFrame 是一次成功捕获的桌面帧（BGRA 内存）。
type dxgiFrame struct {
	Data   []byte // BGRA，长度 w*h*4
	Width  int
	Height int
}

// dxgiDuplicator 封装一次 Output Duplication 会话。
type dxgiDuplicator struct {
	dev        *D3D11Device
	dup        unsafe.Pointer // IDXGIOutputDuplication*
	outputIdx  int
	gotDesc    bool
	width      int
	height     int
}

// NewDXGIDuplicator 用主适配器上指定 output（默认 0）建立 Desktop Duplication。
func NewDXGIDuplicator(outputIdx int) (*dxgiDuplicator, error) {
	dev, err := NewD3D11Device()
	if err != nil {
		return nil, err
	}
	d := &dxgiDuplicator{dev: dev, outputIdx: outputIdx}

	adapter, err := d.deviceAdapter()
	if err != nil {
		dev.Close()
		return nil, err
	}
	defer comRelease(adapter)

	out, err := adapterOutput(adapter, outputIdx)
	if err != nil {
		dev.Close()
		return nil, err
	}

	// QI output → IDXGIOutput1
	qi := comMethod(out, 0)
	var out1 unsafe.Pointer
	r, _, _ := syscall.SyscallN(qi, uintptr(out),
		uintptr(unsafe.Pointer(&iidIDXGIOutput1)), uintptr(unsafe.Pointer(&out1)))
	comRelease(out)
	if r != 0 || out1 == nil {
		dev.Close()
		return nil, fmt.Errorf("QI IDXGIOutput1 HRESULT=0x%x", r)
	}

	// DuplicateOutput(ID3D11Device*, IDXGIOutputDuplication**)
	dupFn := comMethod(out1, 22)
	var dup unsafe.Pointer
	r2, _, _ := syscall.SyscallN(dupFn, uintptr(out1), uintptr(dev.Device()), uintptr(unsafe.Pointer(&dup)))
	comRelease(out1)
	if r2 != 0 || dup == nil {
		dev.Close()
		return nil, fmt.Errorf("DuplicateOutput HRESULT=0x%x", r2)
	}
	d.dup = dup
	return d, nil
}

// deviceAdapter 返回设备的主适配器（IDXGIAdapter*）。
func (d *dxgiDuplicator) deviceAdapter() (unsafe.Pointer, error) {
	qi := comMethod(d.dev.Device(), 0)
	var dxgidev unsafe.Pointer
	r, _, _ := syscall.SyscallN(qi, uintptr(d.dev.Device()),
		uintptr(unsafe.Pointer(&iidIDXGIDevice)), uintptr(unsafe.Pointer(&dxgidev)))
	if r != 0 || dxgidev == nil {
		return nil, fmt.Errorf("QI IDXGIDevice HRESULT=0x%x", r)
	}
	defer comRelease(dxgidev)
	ga := comMethod(dxgidev, 7) // GetAdapter
	var ad unsafe.Pointer
	r2, _, _ := syscall.SyscallN(ga, uintptr(dxgidev), uintptr(unsafe.Pointer(&ad)))
	if r2 != 0 || ad == nil {
		return nil, fmt.Errorf("GetAdapter HRESULT=0x%x", r2)
	}
	return ad, nil
}

// adapterOutput 返回适配器第 idx 个 IDXGIOutput*。
func adapterOutput(adapter unsafe.Pointer, idx int) (unsafe.Pointer, error) {
	eo := comMethod(adapter, 7) // EnumOutputs
	var out unsafe.Pointer
	r, _, _ := syscall.SyscallN(eo, uintptr(adapter), uintptr(idx), uintptr(unsafe.Pointer(&out)))
	if r != 0 || out == nil {
		return nil, fmt.Errorf("EnumOutputs[%d] HRESULT=0x%x", idx, r)
	}
	return out, nil
}

// AcquireFrame 等待并取下一帧桌面，回读为 BGRA。timeoutMs=INFINITE 用 -1。
func (d *dxgiDuplicator) AcquireFrame(timeoutMs int) (*dxgiFrame, error) {
	if d.dup == nil {
		return nil, fmt.Errorf("duplicator 未就绪")
	}
	acq := comMethod(d.dup, 8) // AcquireNextFrame(timeout, DXGI_OUTDUPL_FRAME_INFO*, IDXGIResource**)
	var frameInfo [32]byte
	var res unsafe.Pointer
	r, _, _ := syscall.SyscallN(acq, uintptr(d.dup), uintptr(timeoutMs),
		uintptr(unsafe.Pointer(&frameInfo[0])), uintptr(unsafe.Pointer(&res)))
	if r != 0 {
		if uint32(r) == 0x887a0027 { // DXGI_ERROR_WAIT_TIMEOUT
			return nil, nil
		}
		return nil, fmt.Errorf("AcquireNextFrame HRESULT=0x%x", r)
	}
	if res == nil {
		return nil, fmt.Errorf("AcquireNextFrame 返回空资源")
	}
	// QI resource → ID3D11Texture2D
	qi := comMethod(res, 0)
	var tex unsafe.Pointer
	r2, _, _ := syscall.SyscallN(qi, uintptr(res),
		uintptr(unsafe.Pointer(&iidD3D11Texture2D)), uintptr(unsafe.Pointer(&tex)))
	comRelease(res)
	if r2 != 0 || tex == nil {
		d.ReleaseFrame()
		return nil, fmt.Errorf("QI Texture2D HRESULT=0x%x", r2)
	}
	frame, err := d.readbackTexture(tex)
	comRelease(tex)
	d.ReleaseFrame()
	if err != nil {
		return nil, err
	}
	return frame, nil
}

// D3D11_TEXTURE2D_DESC 布局（64 位进程）：对齐到 4 字节，各字段顺序见 d3d11.h。
// 我们只需 Width/Height：结构如下（各 4 字节，除后三项较大）：
//   Width UINT @0, Height UINT @4, MipLevels @8, ArraySize @12, Format @16,
//   SampleDesc{Count@20,Quality@24}, Usage@28, BindFlags@32, CPUAccessFlags@36, MiscFlags@40
type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

// readbackTexture 回读桌面帧为 BGRA。只分配一次、只拷贝一次：
// 旧实现先建中间缓冲（19.8MB）再由 AcquireBGRA 整块拷到结果，多做了一趟全量拷贝。
func (d *dxgiDuplicator) readbackTexture(tex unsafe.Pointer) (*dxgiFrame, error) {
	// ID3D11Texture2D::GetDesc 槽位 10（GetDesc 返回 void，不检查返回值）
	getDesc := comMethod(tex, 10)
	var desc d3d11Texture2DDesc
	_, _, _ = syscall.SyscallN(getDesc, uintptr(tex), uintptr(unsafe.Pointer(&desc)))
	w, h := int(desc.Width), int(desc.Height)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("非法尺寸 %dx%d", w, h)
	}
	// D3D11_USAGE_STAGING=3, CPU_ACCESS_READ=0x20000
	stagingDesc := d3d11Texture2DDesc{
		Width: desc.Width, Height: desc.Height, MipLevels: 1, ArraySize: 1,
		Format: desc.Format, SampleCount: 1, SampleQuality: 0,
		Usage: 3, CPUAccessFlags: 0x20000,
	}
	// ID3D11Device::CreateTexture2D 槽位 5：CreateTexture2D(&desc, pInitialData=NULL, &staging)
	createTex := comMethod(d.dev.Device(), 5)
	var staging unsafe.Pointer
	r2, _, _ := syscall.SyscallN(createTex, uintptr(d.dev.Device()),
		uintptr(unsafe.Pointer(&stagingDesc)), 0, uintptr(unsafe.Pointer(&staging)))
	if r2 != 0 || staging == nil {
		return nil, fmt.Errorf("CreateTexture2D HRESULT=0x%x", r2)
	}
	defer comRelease(staging)

	// ID3D11DeviceContext::CopyResource 槽位 47：CopyResource(dst, src)
	copyRes := comMethod(d.dev.Context(), 47)
	r3, _, _ := syscall.SyscallN(copyRes, uintptr(d.dev.Context()), uintptr(staging), uintptr(tex))
	if r3 != 0 {
		return nil, fmt.Errorf("CopyResource HRESULT=0x%x", r3)
	}

	// D3D11_MAPPED_SUBRESOURCE { pData; RowPitch; DepthPitch }
	var mapped [24]byte
	// ID3D11DeviceContext::Map 槽位 14：Map(res, subresource=0, D3D11_MAP_READ=1, flags=0, &mapped)
	mapFn := comMethod(d.dev.Context(), 14)
	r4, _, _ := syscall.SyscallN(mapFn, uintptr(d.dev.Context()), uintptr(staging),
		0, 1, 0, uintptr(unsafe.Pointer(&mapped[0])))
	if r4 != 0 {
		return nil, fmt.Errorf("Map HRESULT=0x%x", r4)
	}
	defer func() {
		unmap := comMethod(d.dev.Context(), 15)
		_, _, _ = syscall.SyscallN(unmap, uintptr(d.dev.Context()), uintptr(staging), 0)
	}()

	pData := *(*unsafe.Pointer)(unsafe.Pointer(&mapped[0]))
	rowPitch := *(*uint32)(unsafe.Pointer(&mapped[8]))
	if pData == nil || rowPitch == 0 {
		return nil, fmt.Errorf("Map 返回空数据")
	}
	// 拷贝：逐行（行尾对齐），每像素 4 字节 BGRA。单一缓冲，一次拷贝。
	buf := make([]byte, w*h*4)
	src := unsafe.Slice((*byte)(pData), int(rowPitch)*h)
	if int(rowPitch) == w*4 {
		copy(buf, src[:w*h*4]) // 行距紧凑 → 一次整块拷贝
	} else {
		for row := 0; row < h; row++ {
			copy(buf[row*w*4:], src[row*int(rowPitch):row*int(rowPitch)+w*4])
		}
	}
	return &dxgiFrame{Data: buf, Width: w, Height: h}, nil
}

// ReleaseFrame 释放当前帧。
func (d *dxgiDuplicator) ReleaseFrame() {
	if d.dup != nil {
		rel := comMethod(d.dup, 14)
		_, _, _ = syscall.SyscallN(rel, uintptr(d.dup))
	}
}

// DesktopCapture 长期持有的桌面直捕会话，供逐帧推流循环使用（避免每帧重建 duplicator）。
type DesktopCapture struct {
	dup *dxgiDuplicator
}

// NewDesktopCapture 打开指定输出的桌面直捕。
func NewDesktopCapture(outputIdx int) (*DesktopCapture, error) {
	dup, err := NewDXGIDuplicator(outputIdx)
	if err != nil {
		return nil, err
	}
	return &DesktopCapture{dup: dup}, nil
}

// AcquireBGRA 取下一帧并返回 BGRA 内存副本 + 尺寸。timeoutMs 内无新帧返回 (nil,0,0,nil)。
// 回读直接产出最终缓冲，不再做第二次全量拷贝（3440x1440 省约 19.8MB/帧）。
func (c *DesktopCapture) AcquireBGRA(timeoutMs int) ([]byte, int, int, error) {
	fr, err := c.dup.AcquireFrame(timeoutMs)
	if err != nil {
		return nil, 0, 0, err
	}
	if fr == nil {
		return nil, 0, 0, nil
	}
	return fr.Data, fr.Width, fr.Height, nil
}

// Close 释放直捕会话。
func (c *DesktopCapture) Close() { c.dup.Close() }

// CaptureFrameBGRA 便捷封装：创建（或复用）duplicator 抓一帧并返回 BGRA + 尺寸。
// 供一次性/低频采样使用；逐帧推流请用 DesktopCapture。
func CaptureFrameBGRA(outputIdx, timeoutMs int) (data []byte, w, h int, err error) {
	c, err := NewDesktopCapture(outputIdx)
	if err != nil {
		return nil, 0, 0, err
	}
	defer c.Close()
	return c.AcquireBGRA(timeoutMs)
}

// Close 释放 duplication 与设备。
func (d *dxgiDuplicator) Close() {
	if d.dup != nil {
		comRelease(d.dup)
		d.dup = nil
	}
	if d.dev != nil {
		d.dev.Close()
		d.dev = nil
	}
}
