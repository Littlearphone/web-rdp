package main

import (
	"bufio"
	"bytes"
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
	ffPoolFPS  = make(map[int]int)
	ffPoolH264 = make(map[int]bool)
	ffPoolMu   sync.Mutex
)

// ffSession 表示一个正在运行的 ffmpeg 进程会话。
// 每个显示器+参数组合可共享同一个 ffmpeg 进程（引用计数管理）
type ffSession struct {
	cmd        *exec.Cmd     // ffmpeg 子进程
	stdout     *bufio.Reader // 缓冲读取 ffmpeg stdout 管道
	frameCh    chan []byte   // h264Reader/mjpegReader → fan-out goroutine
	stopCh     chan struct{} // 停止信号通道
	stderrBuf  *bytes.Buffer // ffmpeg stderr（用于诊断编码器报错）
	stderrDone chan struct{} // stderr 读取完成的信号

	subsMu sync.Mutex
	subs   map[int]chan []byte // 订阅者通道
	nextID int
}

// subscribe 注册订阅者，返回 (订阅ID, 独立帧通道)。
// 每个用户获得自己的帧通道，fan-out goroutine 复制每帧给所有订阅者。
func (f *ffSession) subscribe() (int, <-chan []byte) {
	f.subsMu.Lock()
	defer f.subsMu.Unlock()
	id := f.nextID
	f.nextID++
	ch := make(chan []byte, 3)
	if f.subs == nil {
		f.subs = make(map[int]chan []byte)
	}
	f.subs[id] = ch
	return id, ch
}

// unsubscribe 注销订阅者并关闭其通道
func (f *ffSession) unsubscribe(id int) {
	f.subsMu.Lock()
	defer f.subsMu.Unlock()
	if ch, ok := f.subs[id]; ok {
		delete(f.subs, id)
		for {
			select {
			case <-ch:
			default:
				close(ch)
				return
			}
		}
	}
}
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
// h264 决定编码格式，fps 为手动帧率（0=自动检测，仅 ddagrab 模式生效）。
func acquireFFmpeg(id, quality, maxW, fps int, h264 bool) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	s, ok := ffPool[id]
	if ok && ffPoolQ[id] == quality && ffPoolMW[id] == maxW && ffPoolFPS[id] == fps && ffPoolH264[id] == h264 {
		ffRefs[id]++
		return s
	}
	if ok {
		s.stop()
		delete(ffPool, id)
		delete(ffRefs, id)
	}
	s = startFFmpeg(id, quality, maxW, fps, h264)
	if s != nil {
		ffPool[id] = s
		ffRefs[id] = 1
		ffPoolQ[id] = quality
		ffPoolMW[id] = maxW
		ffPoolFPS[id] = fps
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
			delete(ffPoolFPS, id)
			delete(ffPoolH264, id)
		}
	}
}

// ═══════════════════════ 启动（公共） ═══════════════════════

func startFFmpeg(id, quality, maxW, fps int, h264 bool) *ffSession {
	bounds := screenshot.GetDisplayBounds(id)
	physW, physH := bounds.Dx(), bounds.Dy()

	var capX, capY, capW, capH int
	var useDDAGrab bool
	if hasDDAGrab {
		// ddagrab 滤镜：通过 DXGI Desktop Duplication API 捕获，零 GDI 开销
		// 坐标是显示器相对坐标（output_idx 选定目标显示器）
		useDDAGrab = true
		capX, capY = 0, 0
		capW, capH = physW, physH
	} else {
		// gdigrab 回退：传统 GDI 捕获，坐标是虚拟桌面绝对坐标，需乘 DPI 缩放
		z := getScreenZoom(0)
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
	// ddagrab 的 hwdownload,format=bgra 已移入 lavfi 输入滤镜图内，
	// -vf 链只处理后续纯系统内存的格式转换，GPU 编码器可直接接受 bgra 帧。

	refreshRate := 60
	if useDDAGrab && fps > 0 {
		// 手动指定帧率：不压上限，用户知道自己在干什么
		refreshRate = fps
	} else {
		if useDDAGrab {
			if r := getDisplayRefreshRate(id); r > 0 {
				refreshRate = r // ddagrab 自动检测屏幕刷新率
			}
		}
		// 分辨率自适应帧率上限：自动模式下统一生效（ddagrab 和 gdigrab），
		// 避免编码器帧积压。700M 像素/秒对 NVENC/Turing+ 安全，
		// 4K 约 84fps，1440p 约 140fps。
		const maxPPS = 700_000_000
		if px := capW * capH; px > 0 {
			if capFPS := maxPPS / px; refreshRate > capFPS {
				refreshRate = capFPS
			}
		}
		// MJPEG: cap auto framerate at 60fps. JPEG has no inter-frame
		// compression; higher rates waste bandwidth and overwhelm the client.
		if !h264 && refreshRate > 60 {
			refreshRate = 60
		}
		if refreshRate < 60 {
			refreshRate = 60
		}
	}

	var args []string
	if h264 {
		args = h264Args(useDDAGrab, id, refreshRate, capX, capY, capW, capH, vf, quality)
		if args == nil {
			return nil
		}
	} else {
		args = mjpegArgs(useDDAGrab, id, refreshRate, capX, capY, capW, capH, vf, ffQ)
	}

	cmd := exec.Command(ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg 失败: %v", err)
		return nil
	}
	devName := "gdigrab"
	if useDDAGrab {
		devName = "ddagrab"
	}
	log.Printf("ffmpeg: %s %dx%d @%dHz out=%dx%d", devName, physW, physH, refreshRate, outW, outH)

	ff := &ffSession{
		cmd:        cmd,
		stdout:     bufio.NewReaderSize(stdout, 256*1024),
		frameCh:    make(chan []byte, 3),
		stopCh:     make(chan struct{}),
		stderrBuf:  new(bytes.Buffer),
		stderrDone: make(chan struct{}),
	}
	// 异步读取 stderr 用于诊断
	go func() {
		defer close(ff.stderrDone)
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				ff.stderrBuf.Write(buf[:n])
			}
			if err != nil {
				// 限制 stderr 缓冲区大小，防止异常情况下无限增长
				if ff.stderrBuf.Len() > 64*1024 {
					tail := ff.stderrBuf.Bytes()
					ff.stderrBuf.Reset()
					ff.stderrBuf.Write(tail[len(tail)-4096:])
				}
				return
			}
		}
	}()

	if h264 {
		go h264Reader(ff)
	} else {
		go mjpegReader(ff)
	}

	// 扇出 goroutine：读取 frameCh 的每帧，复制给所有订阅者。
	// 解决多用户共享 ffSession 时 Go channel 单消费者导致帧被"瓜分"、
	// H.264 解码器缺参考帧花屏的问题。
	go func() {
		for frame := range ff.frameCh {
			ff.subsMu.Lock()
			for _, ch := range ff.subs {
				if frame == nil {
					// ffmpeg 异常退出通知 — 转发给所有订阅者
					select {
					case ch <- nil:
					default:
					}
				} else {
					// 丢掉最旧帧保留最新帧, 确保低延迟
					select {
					case ch <- frame:
					default:
						select {
						case <-ch:
						default:
						}
						ch <- frame
					}
				}
			}
			ff.subsMu.Unlock()
		}
		// frameCh 关闭 → 关闭所有订阅通道
		ff.subsMu.Lock()
		for _, ch := range ff.subs {
			close(ch)
		}
		ff.subs = nil
		ff.subsMu.Unlock()
	}()

	return ff
}

// ═══════════════════════ H.264 ═══════════════════════

// 根据检测到的 H.264 编码器构建 ffmpeg 参数。
// GPU 编码器优先（低延迟、低 CPU 占用），libx264 作为最终回退。
// quality 为用户画质滑块值（10-100），映射到各编码器的质量/码率参数。
// 当编码器列表耗尽时返回 nil，由调用方回退到 MJPEG。
func h264Args(useDDAGrab bool, id, refreshRate, cx, cy, cw, ch int, vf string, quality int) []string {
	base := []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
	}
	if useDDAGrab {
		// ddagrab 输出 D3D11 纹理；hwdownload+format=bgra 放同一滤镜图内，
		// 否则 -vf 链无法跨滤镜图访问硬件上下文，导致 GPU 编码器报错退出。
		base = append(base,
			"-f", "lavfi",
			"-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:draw_mouse=1,hwdownload,format=bgra", id, refreshRate),
		)
	} else {
		base = append(base,
			"-f", "gdigrab", "-framerate", strconv.Itoa(refreshRate),
			"-draw_mouse", "1",
			"-offset_x", strconv.Itoa(cx),
			"-offset_y", strconv.Itoa(cy),
			"-video_size", fmt.Sprintf("%dx%d", cw, ch),
			"-i", "desktop",
		)
	}
	base = append(base, "-vf", vf)

	// 画质映射：滑块 30-100 → 编码器质量参数（0-51，值越小画质越高）
	//   滑块 100 → 12（极致画质）
	//   滑块  75 → 28（默认，与旧版硬编码值一致）
	//   滑块  30 → 48（最低画质，节省流量）
	var hq int
	if quality >= 75 {
		hq = 28 - (quality-75)*16/25
	} else {
		hq = 28 + (75-quality)*20/45
	}
	if hq < 1 {
		hq = 1
	}
	if hq > 51 {
		hq = 51
	}
	hqs := strconv.Itoa(hq)

	enc := currentH264Encoder()
	switch enc {
	case "h264_nvenc":
		// NVIDIA GPU：p1=最快速度, ll=低延迟, vbr+cq=可变码率恒定质量
		base = append(base, "-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll",
			"-rc", "vbr", "-cq", hqs, "-g", "120")
	case "h264_amf":
		// AMD GPU：speed=最快速度, cqp=恒定质量
		base = append(base, "-c:v", "h264_amf", "-quality", "speed",
			"-rc", "cqp", "-qp_p", hqs, "-qp_i", hqs, "-g", "120")
	case "h264_qsv":
		// Intel Quick Sync：look_ahead=0 关闭前瞻减少延迟
		base = append(base, "-c:v", "h264_qsv", "-preset", "veryfast", "-look_ahead", "0",
			"-async_depth", "1", "-g", "120", "-global_quality", hqs)
	case "libx264":
		// CPU 软件编码回退：ultrafast + zerolatency + 单 slice
		base = append(base, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-crf", hqs, "-g", "120", "-x264opts", "slices=1:threads=1")
	default:
		// 编码器列表已耗尽（currentH264Encoder() 返回 ""），无可用的 H.264 编码器
		return nil
	}
	base = append(base, "-bsf:v", "h264_metadata=aud=insert", "-f", "h264", "-flush_packets", "1", "pipe:1")
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
// 按 AUD (Access Unit Delimiter) 帧边界将 NAL 打包为完整编码帧，
// 通过 frameCh 发送。每帧含 AUD + SPS/PPS/SEI + Slice 全部 NAL，
// 前端一次 WebSocket 消息即可完整解码，消除 NAL 碎片化延迟。
// 遇到读取错误时通知主循环回退。
func h264Reader(ff *ffSession) {
	raw := make([]byte, 64*1024)
	nalBuf := make([]byte, 0, 256*1024)
	sentFrames := false

	// batch 累积同一编码帧的所有 NAL 单元，AUD 为帧边界
	var batch [][]byte

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		total := 0
		for _, nal := range batch {
			total += len(nal)
		}
		frame := make([]byte, total)
		off := 0
		for _, nal := range batch {
			copy(frame[off:], nal)
			off += len(nal)
		}
		ff.frameCh <- frame
		sentFrames = true
		batch = batch[:0]
	}

	for {
		select {
		case <-ff.stopCh:
			return // 主动停止，正常退出
		default:
		}
		n, err := ff.stdout.Read(raw)
		if err != nil {
			if ff.stderrBuf.Len() > 0 {
				log.Printf("ffmpeg stderr: %s", string(ff.stderrBuf.Bytes()))
			}
			flushBatch() // 发送最后的不完整帧
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
			nal := make([]byte, nextSC)
			copy(nal, nalBuf[:nextSC])

			// AUD (NAL type 9) 标记新帧开始，此前累积的 NAL 属于上一帧
			if nal[firstLen]&0x1F == 9 {
				flushBatch()
			}
			batch = append(batch, nal)

			nalBuf = nalBuf[nextSC:]
		}
	}
}

// ═══════════════════════ MJPEG ═══════════════════════

// mjpegArgs 构建 MJPEG 编码的 ffmpeg 命令行参数。
// 使用 image2pipe 容器直接输出 JPEG 数据流，前端通过 SOI/EOI 标记分割帧。
func mjpegArgs(useDDAGrab bool, id, refreshRate, cx, cy, cw, ch int, vf string, ffQ int) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
	}
	if useDDAGrab {
		args = append(args,
			"-f", "lavfi",
			"-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:draw_mouse=1,hwdownload,format=bgra", id, refreshRate),
		)
	} else {
		args = append(args,
			"-f", "gdigrab", "-framerate", strconv.Itoa(refreshRate),
			"-draw_mouse", "1",
			"-offset_x", strconv.Itoa(cx),
			"-offset_y", strconv.Itoa(cy),
			"-video_size", fmt.Sprintf("%dx%d", cw, ch),
			"-i", "desktop",
		)
	}
	args = append(args,
		"-vf", vf,
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(ffQ),
		"-huffman", "default",
		"-flush_packets", "1",
		"-f", "image2pipe", "pipe:1",
	)
	return args
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
			if ff.stderrBuf.Len() > 0 {
				log.Printf("ffmpeg stderr: %s", string(ff.stderrBuf.Bytes()))
			}
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
