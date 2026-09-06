//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── D3D11 设备/上下文（纯 syscall，无 cgo）──
//
// 跨厂商硬件编码（MF 硬件编码器 MFT / DXGI 直捕）都需要 D3D11 设备。
// 这里封装 D3D11CreateDevice，持有设备与立即上下文，用后须 Close() 释放。
//
// 依据 d3d11.h：
//   HRESULT D3D11CreateDevice(
//     IDXGIAdapter* pAdapter, D3D_DRIVER_TYPE DriverType, HMODULE Software,
//     UINT Flags, const D3D_FEATURE_LEVEL* pFeatureLevels, UINT FeatureLevels,
//     UINT SDKVersion, ID3D11Device** ppDevice, D3D_FEATURE_LEVEL* pFeatureLevel,
//     ID3D11DeviceContext** ppImmediateContext);
//   D3D11_SDK_VERSION = 7
//   D3D_DRIVER_TYPE_HARDWARE = 1
//   D3D11_CREATE_DEVICE_BGRA_SUPPORT = 0x20

const (
	d3dDriverTypeHardware = 1
	d3d11SDKVersion       = 7
	d3d11CreateBGRASupport = 0x20
)

// D3D11 接口 IID
var (
	// IID_ID3D11Device = {db6f6ddb-ac77-4e88-8255-819df9bb5f02}
	iidD3D11Device = mkGUID(0xdb6f6ddb, 0xac77, 0x4e88,
		[8]byte{0x82, 0x55, 0x81, 0x9d, 0xf9, 0xbb, 0x5f, 0x02})
)

var (
	d3d11dll          = syscall.NewLazyDLL("d3d11.dll")
	procD3D11CreateDevice = d3d11dll.NewProc("D3D11CreateDevice")
)

// D3D11Device 封装一个 D3D11 硬件设备与立即上下文。
type D3D11Device struct {
	device unsafe.Pointer // ID3D11Device*
	ctx    unsafe.Pointer // ID3D11DeviceContext*（立即上下文）
}

// NewD3D11Device 创建硬件 D3D11 设备（pAdapter=NULL, DriverType=HARDWARE, BGRA support）。
func NewD3D11Device() (*D3D11Device, error) {
	var dev, ctx unsafe.Pointer
	// D3D11CreateDevice(NULL, HARDWARE, NULL, BGRA, NULL, 0, SDK7, &dev, NULL, &ctx)
	r, _, _ := procD3D11CreateDevice.Call(
		0, // pAdapter = NULL（用默认适配器）
		uintptr(d3dDriverTypeHardware),
		0, // Software module
		uintptr(d3d11CreateBGRASupport),
		0, // pFeatureLevels = NULL（默认层级）
		0, // FeatureLevels
		uintptr(d3d11SDKVersion),
		uintptr(unsafe.Pointer(&dev)),
		0, // pFeatureLevel = NULL
		uintptr(unsafe.Pointer(&ctx)))
	if r != 0 {
		return nil, fmt.Errorf("D3D11CreateDevice HRESULT=0x%x", r)
	}
	if dev == nil || ctx == nil {
		return nil, fmt.Errorf("D3D11CreateDevice 返回空句柄")
	}
	return &D3D11Device{device: dev, ctx: ctx}, nil
}

// Device 返回 ID3D11Device*（供编码器/DXGI 使用）。
func (d *D3D11Device) Device() unsafe.Pointer { return d.device }

// Context 返回 ID3D11DeviceContext*（供纹理拷贝使用）。
func (d *D3D11Device) Context() unsafe.Pointer { return d.ctx }

// Close 释放立即上下文与设备。
func (d *D3D11Device) Close() {
	if d.ctx != nil {
		comRelease(d.ctx)
		d.ctx = nil
	}
	if d.device != nil {
		comRelease(d.device)
		d.device = nil
	}
}
