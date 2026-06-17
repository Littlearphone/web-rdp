package main

import (
	"bufio"
	"fmt"
	"github.com/kbinani/screenshot"
	"log"
	"os/exec"
	"strconv"
	"sync"
)

var (
	ffPool   = make(map[int]*ffSession)
	ffRefs   = make(map[int]int)
	ffPoolQ  = make(map[int]int)
	ffPoolMW = make(map[int]int)
	ffPoolMu sync.Mutex
)

func acquireFFmpeg(id, q, mw int) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	s, ok := ffPool[id]
	if ok && ffPoolQ[id] == q && ffPoolMW[id] == mw {
		ffRefs[id]++
		return s
	}
	if ok {
		s.stop()
		delete(ffPool, id)
		delete(ffRefs, id)
	}
	s, _, _ = startFFmpeg(id, q, mw)
	if s != nil {
		ffPool[id] = s
		ffRefs[id] = 1
		ffPoolQ[id] = q
		ffPoolMW[id] = mw
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
		close(f.frameCh)
	}
}

func startFFmpeg(id, q, mw int) (*ffSession, int, int) {
	b := screenshot.GetDisplayBounds(id)
	pw, ph := b.Dx(), b.Dy()
	var cx, cy, cw, ch int
	var dev string
	if hasDXGI {
		dev = "dxgigrab"
		cx = b.Min.X
		cy = b.Min.Y
		cw = pw
		ch = ph
	} else {
		z := getScreenZoom(0)
		dev = "gdigrab"
		cx = int(float64(b.Min.X) * z)
		cy = int(float64(b.Min.Y) * z)
		cw = int(float64(pw) * z)
		ch = int(float64(ph) * z)
	}
	ow, oh := cw, ch
	fq := 32 - (q-1)*31/99
	if fq < 1 {
		fq = 1
	}
	if fq > 31 {
		fq = 31
	}
	vf := "format=yuv420p"
	if mw > 0 && cw > mw {
		oh = ch * mw / cw
		ow = mw
		vf = fmt.Sprintf("scale=%d:%d:flags=fast_bilinear,format=yuv420p", ow, oh)
	}

	var args []string
	if h264Encoder != "" {
		args = []string{"-hide_banner", "-loglevel", "error", "-sws_flags", "fast_bilinear", "-f", dev, "-framerate", "60", "-draw_mouse", "1", "-offset_x", strconv.Itoa(cx), "-offset_y", strconv.Itoa(cy), "-video_size", fmt.Sprintf("%dx%d", cw, ch), "-i", "desktop", "-vf", vf, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-crf", "28", "-g", "120", "-f", "h264", "-flush_packets", "1", "pipe:1"}
	} else {
		args = []string{"-hide_banner", "-loglevel", "error", "-sws_flags", "fast_bilinear", "-f", dev, "-framerate", "60", "-draw_mouse", "1", "-offset_x", strconv.Itoa(cx), "-offset_y", strconv.Itoa(cy), "-video_size", fmt.Sprintf("%dx%d", cw, ch), "-i", "desktop", "-vf", vf, "-c:v", "mjpeg", "-q:v", strconv.Itoa(fq), "-huffman", "default", "-f", "image2pipe", "pipe:1"}
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stderr = log.Writer()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, 0
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg 失败: %v", err)
		return nil, 0, 0
	}
	log.Printf("ffmpeg: %s screen=%d %dx%d out=%dx%d", dev, id, pw, ph, ow, oh)

	ff := &ffSession{cmd: cmd, stdout: bufio.NewReaderSize(stdout, 256*1024), frameCh: make(chan []byte, 1), stopCh: make(chan struct{})}

	go func() {
		if h264Encoder != "" {
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
						if nalBuf[idx] == 0 && nalBuf[idx+1] == 0 && nalBuf[idx+2] == 0 && nalBuf[idx+3] == 1 {
							break
						}
						idx++
					}
					if idx >= len(nalBuf)-3 {
						break
					}
					sendLen := idx
					f := make([]byte, sendLen)
					copy(f, nalBuf[:sendLen])
					select {
					case ff.frameCh <- f:
					default:
						<-ff.frameCh
						ff.frameCh <- f
					}
					nalBuf = nalBuf[sendLen:]
				}
			}
		} else {
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
				f := make([]byte, len(jpg))
				copy(f, jpg)
				select {
				case ff.frameCh <- f:
				default:
					<-ff.frameCh
					ff.frameCh <- f
				}
			}
		}
	}()
	return ff, ow, oh
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
