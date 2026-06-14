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

var (
	zoomCache   = make(map[int]float64)
	zoomCacheMu sync.RWMutex
)

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
