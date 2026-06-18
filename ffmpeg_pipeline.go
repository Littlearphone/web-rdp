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

// ═══════════════════════ 共享池 ═══════════════════════

var (
	ffPool   = make(map[int]*ffSession)
	ffRefs   = make(map[int]int)
	ffPoolQ  = make(map[int]int)
	ffPoolMW = make(map[int]int)
	ffPoolMu sync.Mutex
)

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
		close(f.frameCh)
	}
}

func acquireFFmpeg(id, quality, maxW int, h264 bool) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	s, ok := ffPool[id]
	if ok && ffPoolQ[id] == quality && ffPoolMW[id] == maxW {
		ffRefs[id]++
		return s
	}
	if ok {
		s.stop()
		delete(ffPool, id)
		delete(ffRefs, id)
	}
	s = startFFmpeg(id, quality, maxW, h264)
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

// ═══════════════════════ 启动（公共） ═══════════════════════

func startFFmpeg(id, quality, maxW int, h264 bool) *ffSession {
	bounds := screenshot.GetDisplayBounds(id)
	physW, physH := bounds.Dx(), bounds.Dy()

	var capX, capY, capW, capH int
	var device string
	if hasDXGI {
		device = "dxgigrab"
		capX, capY = bounds.Min.X, bounds.Min.Y
		capW, capH = physW, physH
	} else {
		z := getScreenZoom(0)
		device = "gdigrab"
		capX = int(float64(bounds.Min.X) * z)
		capY = int(float64(bounds.Min.Y) * z)
		capW = int(float64(physW) * z)
		capH = int(float64(physH) * z)
	}

	outW, outH := capW, capH
	ffQ := 32 - (quality-1)*31/99
	if ffQ < 1 {
		ffQ = 1
	}
	if ffQ > 31 {
		ffQ = 31
	}

	vf := "format=yuv420p"
	if maxW > 0 && capW > maxW {
		outH = capH * maxW / capW
		outW = maxW
		vf = fmt.Sprintf("scale=%d:%d:flags=fast_bilinear,format=yuv420p", outW, outH)
	}

	var args []string
	if h264 {
		args = h264Args(device, capX, capY, capW, capH, vf)
	} else {
		args = mjpegArgs(device, capX, capY, capW, capH, vf, ffQ)
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stderr = log.Writer()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg 失败: %v", err)
		return nil
	}
	log.Printf("ffmpeg: %s %dx%d out=%dx%d", device, physW, physH, outW, outH)

	ff := &ffSession{
		cmd:     cmd,
		stdout:  bufio.NewReaderSize(stdout, 256*1024),
		frameCh: make(chan []byte, 1),
		stopCh:  make(chan struct{}),
	}

	if h264 {
		go h264Reader(ff)
	} else {
		go mjpegReader(ff)
	}
	return ff
}

// ═══════════════════════ H.264 ═══════════════════════

func h264Args(device string, cx, cy, cw, ch int, vf string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
		"-f", device, "-framerate", "60",
		"-draw_mouse", "1",
		"-offset_x", strconv.Itoa(cx),
		"-offset_y", strconv.Itoa(cy),
		"-video_size", fmt.Sprintf("%dx%d", cw, ch),
		"-i", "desktop",
		"-vf", vf,
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-crf", "28", "-g", "120",
		"-f", "h264", "-flush_packets", "1",
		"pipe:1",
	}
}

func h264Reader(ff *ffSession) {
	raw := make([]byte, 64*1024)
	nalBuf := make([]byte, 0, 256*1024)

	for {
		select {
		case <-ff.stopCh:
			return
		default:
		}
		n, err := ff.stdout.Read(raw)
		if err != nil {
			return
		}
		nalBuf = append(nalBuf, raw[:n]...)

		for len(nalBuf) > 4 {
			idx := 4
			for idx < len(nalBuf)-3 {
				if nalBuf[idx] == 0 && nalBuf[idx+1] == 0 &&
					nalBuf[idx+2] == 0 && nalBuf[idx+3] == 1 {
					break
				}
				idx++
			}
			if idx >= len(nalBuf)-3 {
				break
			}
			frame := make([]byte, idx)
			copy(frame, nalBuf[:idx])
			select {
			case ff.frameCh <- frame:
			default:
				<-ff.frameCh
				ff.frameCh <- frame
			}
			nalBuf = nalBuf[idx:]
		}
	}
}

// ═══════════════════════ MJPEG ═══════════════════════

func mjpegArgs(device string, cx, cy, cw, ch int, vf string, ffQ int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
		"-f", device, "-framerate", "60",
		"-draw_mouse", "1",
		"-offset_x", strconv.Itoa(cx),
		"-offset_y", strconv.Itoa(cy),
		"-video_size", fmt.Sprintf("%dx%d", cw, ch),
		"-i", "desktop",
		"-vf", vf,
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(ffQ),
		"-huffman", "default",
		"-f", "image2pipe", "pipe:1",
	}
}

func mjpegReader(ff *ffSession) {
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
