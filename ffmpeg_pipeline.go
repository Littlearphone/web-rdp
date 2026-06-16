package main

import (
	"bufio"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"sync"

	"github.com/kbinani/screenshot"
)

// ── ffmpeg 管线（按屏幕 ID 共享，引用计数）──

var (
	ffPool   = make(map[int]*ffSession)
	ffRefs   = make(map[int]int)
	ffPoolQ  = make(map[int]int)
	ffPoolMW = make(map[int]int)
	ffPoolMu sync.Mutex
)

func acquireFFmpeg(id, quality, maxW int) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	s, ok := ffPool[id]
	if ok && ffPoolQ[id] == quality && ffPoolMW[id] == maxW {
		ffRefs[id]++
		return s
	}
	// 参数变了或无缓存 → 重建
	if ok {
		s.stop()
		delete(ffPool, id)
		delete(ffRefs, id)
	}
	s, _, _ = startFFmpeg(id, quality, maxW)
	if s != nil {
		ffPool[id] = s
		ffRefs[id] = 1
		ffPoolQ[id] = quality
		ffPoolMW[id] = maxW
	}
	return s
}

func releaseFFmpeg(id int) {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	ffRefs[id]--
	if ffRefs[id] <= 0 {
		if s, ok := ffPool[id]; ok {
			s.stop()
			delete(ffPool, id)
			delete(ffRefs, id)
		}
	}
}

type ffSession struct {
	cmd     *exec.Cmd
	stdout  *bufio.Reader
	frameCh chan []byte
	stopCh  chan struct{}
}

func (f *ffSession) stop() {
	if f.cmd != nil {
		close(f.stopCh)
		_ = f.cmd.Process.Kill()
		_ = f.cmd.Wait()
		close(f.frameCh) // 唤醒阻塞在 frameCh 上的 reader
	}
}

func startFFmpeg(id, quality, maxW int) (*ffSession, int, int) {
	bounds := screenshot.GetDisplayBounds(id)
	physW := bounds.Dx()
	physH := bounds.Dy()

	var capX, capY, capW, capH int
	var device string

	if hasDXGI {
		device = "dxgigrab"
		capX = bounds.Min.X
		capY = bounds.Min.Y
		capW = physW
		capH = physH
	} else {
		z := getScreenZoom(0)
		device = "gdigrab"
		capX = int(float64(bounds.Min.X) * z)
		capY = int(float64(bounds.Min.Y) * z)
		capW = int(float64(physW) * z)
		capH = int(float64(physH) * z)
	}

	outW := capW
	outH := capH

	ffQ := 32 - (quality-1)*31/99
	if ffQ < 1 {
		ffQ = 1
	}
	if ffQ > 31 {
		ffQ = 31
	}

	vf := "format=yuv420p" // 色度半采样，带宽减半，画质几乎无损
	if maxW > 0 && capW > maxW {
		outH = capH * maxW / capW
		outW = maxW
		vf = fmt.Sprintf("scale=%d:%d:flags=fast_bilinear,format=yuv420p", outW, outH)
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
		"-f", device, "-framerate", "60", // 最大 60fps，超过会导致 CPU 100% 卡死
		"-draw_mouse", "1", // 捕获远程光标，前端隐藏本地光标
		"-offset_x", strconv.Itoa(capX),
		"-offset_y", strconv.Itoa(capY),
		"-video_size", fmt.Sprintf("%dx%d", capW, capH),
		"-i", "desktop",
		"-vf", vf,
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(ffQ),
		"-huffman", "default", // 优化哈夫曼表，体积再减 5-10%
		"-f", "image2pipe", "pipe:1",
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stderr = log.Writer()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, 0
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg 启动失败: %v", err)
		return nil, 0, 0
	}

	log.Printf("ffmpeg: %s screen=%d phys=%dx%d@(%d,%d) cap=%dx%d@(%d,%d) out=%dx%d",
		device, id, physW, physH, bounds.Min.X, bounds.Min.Y,
		capW, capH, capX, capY, outW, outH)

	ff := &ffSession{
		cmd:     cmd,
		stdout:  bufio.NewReaderSize(stdout, 256*1024),
		frameCh: make(chan []byte, 1),
		stopCh:  make(chan struct{}),
	}

	go func() {
		buf := make([]byte, 0, 512*1024)
		for {
			select {
			case <-ff.stopCh:
				return
			default:
			}
			jpg, err := readJPEG(ff.stdout, buf)
			if err != nil {
				return
			}
			buf = jpg
			frame := make([]byte, len(jpg))
			copy(frame, jpg)
			select {
			case ff.frameCh <- frame:
			default:
				<-ff.frameCh
				ff.frameCh <- frame
			}
		}
	}()

	return ff, outW, outH
}

func readJPEG(br *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	prev := byte(0)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if prev == 0xFF && b == 0xD8 {
			buf = append(buf, 0xFF, 0xD8)
			break
		}
		prev = b
	}
	prev = 0
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		buf = append(buf, b)
		if prev == 0xFF && b == 0xD9 {
			break
		}
		prev = b
	}
	return buf, nil
}
