package main

import (
	"encoding/binary"
	"image"
	"math"
	"sync"
	"syscall"

	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
)

// ── 缩放比缓存 ──
// 缓存每个显示器的 DPI 缩放比（zoom factor），避免重复调用 EnumDisplayMonitors API
var (
	zoomCache   = make(map[int]float64)
	zoomCacheMu sync.RWMutex
)

// getScreenZoom 获取指定显示器的 DPI 缩放比。
// 通过 EnumDisplayMonitors 获取逻辑分辨率，与 screenshot 物理分辨率对比计算出缩放因子。
// 首次调用时枚举所有显示器并缓存，后续调用直接返回缓存值。
// 注意：syscall.NewCallback 只能在包初始化或缓存未命中时调用一次，
// 因为 Go 在 Windows 上限 2000 个 callback 且永不释放。
func getScreenZoom(id int) float64 {
	zoomCacheMu.RLock()
	if z, ok := zoomCache[id]; ok {
		zoomCacheMu.RUnlock()
		return z
	}
	zoomCacheMu.RUnlock()

	// 未命中：枚举所有显示器，一次性填充缓存
	zoomCacheMu.Lock()
	// 双重检查，避免重复枚举
	if z, ok := zoomCache[id]; ok {
		zoomCacheMu.Unlock()
		return z
	}
	idx := 0
	cb := syscall.NewCallback(func(_ uintptr, _ uintptr, rc *RECT, _ uintptr) uintptr {
		lw := rc.Right - rc.Left
		b := screenshot.GetDisplayBounds(idx)
		z := 1.0
		if pw := b.Dx(); pw > 0 && lw > 0 {
			z = float64(lw) / float64(pw)
		}
		zoomCache[idx] = z
		idx++
		return 1
	})
	_, _, _ = procEnumDisplayMonitors.Call(0, 0, cb, 0)
	zoomCacheMu.Unlock()

	// 枚举完成后查缓存
	zoomCacheMu.RLock()
	z, ok := zoomCache[id]
	zoomCacheMu.RUnlock()
	if ok {
		return z
	}
	return 1.0
}

// encodeFrame 将 MJPEG 帧数据与元数据打包为网络传输格式。
// 二进制布局: [ox(4B)] [oy(4B)] [pw(4B)] [ph(4B)] [zoom(8B)] [JPEG数据]
// H.264 模式不使用此函数，直接裸发 NAL 单元
func encodeFrame(ox, oy, pw, ph int32, zoom float64, jpg []byte) []byte {
	buf := make([]byte, 24+len(jpg))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(ox))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(oy))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(pw))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(ph))
	binary.LittleEndian.PutUint64(buf[16:24], math.Float64bits(zoom))
	copy(buf[24:], jpg)
	return buf
}

// downscale 使用双线性插值将图像缩放到指定最大宽度（等比缩放）。
// 仅在纯 Go 截图回退路径中使用；ffmpeg 路径在命令行中完成缩放。
func downscale(img *image.RGBA, maxW int) *image.RGBA {
	if maxW <= 0 || img.Bounds().Dx() <= maxW {
		return img
	}
	newW := maxW
	newH := img.Bounds().Dy() * maxW / img.Bounds().Dx()
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}
