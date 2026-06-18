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
	ffPool     = make(map[int]*ffSession)
	ffRefs     = make(map[int]int)
	ffPoolQ    = make(map[int]int)
	ffPoolMW   = make(map[int]int)
	ffPoolH264 = make(map[int]bool)
	ffPoolMu   sync.Mutex
)

// ffSession 表示一个正在运行的 ffmpeg 进程会话。
// 每个显示器+参数组合可共享同一个 ffmpeg 进程（引用计数管理）
type ffSession struct {
	cmd     *exec.Cmd     // ffmpeg 子进程
	stdout  *bufio.Reader // 缓冲读取 ffmpeg stdout 管道
	frameCh chan []byte   // 解码后的帧数据通道
	stopCh  chan struct{} // 停止信号通道
}

// stop 终止 ffmpeg 进程并关闭帧通道
func (f *ffSession) stop() {
	if f.cmd != nil {
		close(f.stopCh)
		_ = f.cmd.Process.Kill()
		_ = f.cmd.Wait()
		close(f.frameCh)
	}
}

// acquireFFmpeg 获取或创建指定显示器的 ffmpeg 会话。
// 如果参数匹配现有会话则复用（引用计数+1），否则停止旧会话并创建新会话。
// h264 参数决定使用 H.264 还是 MJPEG 编码。
func acquireFFmpeg(id, quality, maxW int, h264 bool) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	s, ok := ffPool[id]
	if ok && ffPoolQ[id] == quality && ffPoolMW[id] == maxW && ffPoolH264[id] == h264 {
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
		ffPoolH264[id] = h264
	}
	return s
}

// releaseFFmpeg 释放对指定显示器 ffmpeg 会话的引用。
// 当引用计数降至 0 时，停止 ffmpeg 进程并清理资源
func releaseFFmpeg(id int) {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	ffRefs[id]--
	if ffRefs[id] <= 0 {
		if s, ok := ffPool[id]; ok {
			s.stop()
			delete(ffPool, id)
			delete(ffRefs, id)
			delete(ffPoolQ, id)
			delete(ffPoolMW, id)
			delete(ffPoolH264, id)
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
		outW = maxW
		outH = capH * outW / capW
		// yuv420p / H.264 要求宽高均为偶数，奇数会导致 libx264 拒绝编码
		if outW%2 != 0 {
			outW--
		}
		if outH%2 != 0 {
			outH--
		}
		vf = fmt.Sprintf("scale=%d:%d:flags=fast_bilinear,format=yuv420p", outW, outH)
	}

	var args []string
	if h264 {
		args = h264Args(device, capX, capY, capW, capH, vf)
		// h264Args 返回 nil 表示编码器列表已耗尽，应回退到 MJPEG
		if args == nil {
			return nil
		}
	} else {
		args = mjpegArgs(device, capX, capY, capW, capH, vf, ffQ)
	}

	cmd := exec.Command(ffmpegPath, args...)
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
		frameCh: make(chan []byte, 16),
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

// 根据检测到的 H.264 编码器构建 ffmpeg 参数。
// GPU 编码器优先（低延迟、低 CPU 占用），libx264 作为最终回退。
// 当编码器列表耗尽时返回 nil，由调用方回退到 MJPEG。
func h264Args(device string, cx, cy, cw, ch int, vf string) []string {
	base := []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
		"-f", device, "-framerate", "60",
		"-draw_mouse", "1",
		"-offset_x", strconv.Itoa(cx),
		"-offset_y", strconv.Itoa(cy),
		"-video_size", fmt.Sprintf("%dx%d", cw, ch),
		"-i", "desktop",
		"-vf", vf,
	}
	enc := currentH264Encoder()
	switch enc {
	case "h264_nvenc":
		// NVIDIA GPU：p1=最快速度, ll=低延迟, vbr+cq=可变码率恒定质量
		base = append(base, "-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll",
			"-rc", "vbr", "-cq", "28", "-g", "120")
	case "h264_amf":
		// AMD GPU：speed=最快速度, cqp=恒定质量
		base = append(base, "-c:v", "h264_amf", "-quality", "speed",
			"-rc", "cqp", "-qp_p", "28", "-qp_i", "28", "-g", "120")
	case "h264_qsv":
		// Intel Quick Sync：look_ahead=0 关闭前瞻减少延迟
		base = append(base, "-c:v", "h264_qsv", "-preset", "veryfast", "-look_ahead", "0",
			"-g", "120", "-global_quality", "28")
	case "h264_mf":
		// Windows Media Foundation：系统自带（quality=画质优先）
		base = append(base, "-c:v", "h264_mf", "-preset", "quality",
			"-rc", "vbr", "-qp", "28", "-g", "120")
	case "libx264":
		// CPU 软件编码回退：ultrafast + zerolatency + 单 slice
		base = append(base, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-crf", "28", "-g", "120", "-x264opts", "slices=1:threads=1")
	default:
		// 编码器列表已耗尽（currentH264Encoder() 返回 ""），无可用的 H.264 编码器
		return nil
	}
	base = append(base, "-f", "h264", "-flush_packets", "1", "pipe:1")
	return base
}

// findStartCode 在 data[start:] 中查找 H.264 起始码
// 支持 3 字节 (0x00 0x00 0x01) 和 4 字节 (0x00 0x00 0x00 0x01)
// 返回 (位置, 长度)，未找到返回 (-1, 0)
func findStartCode(data []byte, start int) (int, int) {
	for i := start; i < len(data)-1; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if i+2 < len(data) && data[i+2] == 1 {
				return i, 3
			}
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				return i, 4
			}
		}
	}
	return -1, 0
}

// h264Reader 持续从 ffmpeg stdout 读取 H.264 Annex B 原始流，
// 按起始码切分 NAL 单元并通过 frameCh 发送。遇到读取错误时通知主循环回退。
func h264Reader(ff *ffSession) {
	raw := make([]byte, 64*1024)
	nalBuf := make([]byte, 0, 256*1024)
	sentFrames := false // 是否曾成功发送帧（用于判断 ffmpeg 是否异常退出）

	for {
		select {
		case <-ff.stopCh:
			return // 主动停止，正常退出
		default:
		}
		n, err := ff.stdout.Read(raw)
		if err != nil {
			// ffmpeg 进程退出（驱动不支持、参数错误等）
			// 如果未发送任何帧，通知主循环即时回退，无需等 5 秒超时
			if !sentFrames {
				select {
				case ff.frameCh <- nil:
				default:
				}
			}
			return
		}
		nalBuf = append(nalBuf, raw[:n]...)

		for len(nalBuf) > 3 {
			firstSC, firstLen := findStartCode(nalBuf, 0)
			if firstSC < 0 {
				break
			}
			if firstSC > 0 {
				nalBuf = nalBuf[firstSC:]
				continue
			}
			nextSC, _ := findStartCode(nalBuf, firstLen)
			if nextSC < 0 {
				break
			}
			frame := make([]byte, nextSC)
			copy(frame, nalBuf[:nextSC])
			ff.frameCh <- frame
			sentFrames = true
			nalBuf = nalBuf[nextSC:]
		}
	}
}

// ═══════════════════════ MJPEG ═══════════════════════

// mjpegArgs 构建 MJPEG 编码的 ffmpeg 命令行参数。
// 使用 image2pipe 容器直接输出 JPEG 数据流，前端通过 SOI/EOI 标记分割帧。
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

// mjpegReader 持续从 ffmpeg stdout 读取 MJPEG 数据流，
// 按 SOI (0xFFD8) / EOI (0xFFD9) 标记分割为独立的 JPEG 帧并通过 frameCh 发送。
// 帧通道满时丢弃旧帧保留新帧，确保低延迟。
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

// readJPEG 从 bufio.Reader 中读取单个完整的 JPEG 帧。
// 通过扫描 SOI (0xFFD8) 和 EOI (0xFFD9) 标记定位帧边界。
// 返回值共享传入的 buf 底层数组以减少内存分配。
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
