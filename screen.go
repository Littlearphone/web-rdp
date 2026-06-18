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
// 结果会被缓存，避免重复的 Windows API 调用。
func getScreenZoom(id int) float64 {
	zoomCacheMu.RLock()
	if z, ok := zoomCache[id]; ok {
		zoomCacheMu.RUnlock()
		return z
	}
	zoomCacheMu.RUnlock()
	var lw int32
	idx := 0
	cb := syscall.NewCallback(func(_ uintptr, _ uintptr, rc *RECT, _ uintptr) uintptr {
		if idx == id {
			lw = rc.Right - rc.Left
		}
		idx++
		return 1
	})
	_, _, _ = procEnumDisplayMonitors.Call(0, 0, cb, 0)
	b := screenshot.GetDisplayBounds(id)
	z := 1.0
	if pw := b.Dx(); pw > 0 && lw > 0 {
		z = float64(lw) / float64(pw)
	}
	zoomCacheMu.Lock()
	zoomCache[id] = z
	zoomCacheMu.Unlock()
	return z
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
