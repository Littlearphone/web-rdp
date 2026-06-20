package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"time"

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

	subsMu     sync.Mutex
	subs       map[int]chan []byte // 订阅者通道
	nextID     int
	sentFrames bool // h264Reader 是否成功发送过帧（用于判断编码器真实可用性）
	display    int  // 显示器 ID（供 fan-out goroutine 写入 WebRTC 轨时使用）
	isH264     bool // 当前是否为 H.264 编码（供 fan-out goroutine 判断是否写 WebRTC）
	fps        int  // 实际捕获帧率（供 fan-out 计算 WebRTC RTP 时间戳间隔）
}

// subscribe 注册订阅者，返回 (订阅ID, 独立帧通道)。
// 每个用户获得自己的帧通道，fan-out goroutine 复制每帧给所有订阅者。
func (f *ffSession) subscribe() (int, <-chan []byte) {
	f.subsMu.Lock()
	defer f.subsMu.Unlock()
	id := f.nextID
	f.nextID++
	ch := make(chan []byte, 5)
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
// 多用户场景下，已有会话无条件复用（参数以首个创建者为准），
// 仅控制者可调用 restartFFmpeg 强制重建。
func acquireFFmpeg(id, quality, maxW, fps int, h264 bool) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()
	if s, ok := ffPool[id]; ok {
		ffRefs[id]++
		return s
	}
	s := startFFmpeg(id, quality, maxW, fps, h264)
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

// restartFFmpeg 强制停止旧会话并创建新会话（仅控制者调用）。
// 会关闭所有订阅者的通道，订阅者收到 nil 后重新 join 新会话。
//
// 重要：调用者必须在调用前已通过 releaseFFmpeg 释放自己对旧会话的引用，
// 因此 ffRefs[id] 仅反映其他订阅者的引用。必须保留这些引用并迁移到新会话，
// 否则其他订阅者在收到 nil 后调用 releaseFFmpeg 会将新会话的引用计数减到 0，
// 导致新会话被意外杀死（详见 ffmpeg_pipeline.go 顶部关于并发查看花屏的修复）。
func restartFFmpeg(id, quality, maxW, fps int, h264 bool) *ffSession {
	ffPoolMu.Lock()
	defer ffPoolMu.Unlock()

	// 保存其他订阅者的引用计数（调用者已释放自己的引用）
	remainingRefs := 0
	if s, ok := ffPool[id]; ok {
		s.stop()
		remainingRefs = ffRefs[id]
		delete(ffPool, id)
		delete(ffRefs, id)
	}

	s := startFFmpeg(id, quality, maxW, fps, h264)
	if s != nil {
		ffPool[id] = s
		// 新会话的引用 = 其他订阅者 + 当前调用者（本次 subscribe 对应）
		ffRefs[id] = remainingRefs + 1
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
	if fps > 0 {
		// 手动指定帧率：前端下拉框传明确值，不压上限
		refreshRate = fps
	} else {
		// fps=0：自动检测当前显示器刷新率（WebRTC 模式可直接跑满）
		if r := getDisplayRefreshRate(id); r > 0 {
			refreshRate = r
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
		frameCh:    make(chan []byte, 6),
		stopCh:     make(chan struct{}),
		stderrBuf:  new(bytes.Buffer),
		stderrDone: make(chan struct{}),
		display:    id,
		isH264:     h264,
		fps:        refreshRate,
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
			// WebRTC 路径：H.264 帧额外写入全局视频轨（非阻塞，静默丢弃）
			// 按实际帧率计算帧间隔，确保 RTP 时间戳与真实帧率匹配
			if frame != nil && ff.isH264 && ff.fps > 0 {
				writeWebRTCSample(frame, time.Second/time.Duration(ff.fps))
			}
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

	// ── 画质映射：滑块 30-100 → 编码器 CQ/CRF 值（0-51，越小画质越高）──
	//   100 → 25（曾为 12，过高画质导致编码器过载）
	//    75 → 35（默认中点）
	//    30 → 48（最低画质）
	var hq int
	if quality >= 75 {
		hq = 35 - (quality-75)*10/25
	} else {
		hq = 35 + (75-quality)*13/45
	}
	if hq < 1 {
		hq = 1
	}
	if hq > 51 {
		hq = 51
	}
	hqs := strconv.Itoa(hq)

	// ── 码率上限：基于像素率和画质的动态 maxrate ──
	// 必须配合 bufsize 使用，否则 VBR/CRF 模式完全忽略 maxrate。
	// bufsize = maxrate * 0.2s，够容纳大 IDR 帧（200-400KB），
	// 但远低于秒级，不会引发多秒 VBV 缓冲延迟。
	// 下限 2000k（250KB）确保 4K 分辨率下 IDR 帧不撑爆 VBV。
	pxPerSec := cw * ch * refreshRate
	bpp := 0.03 + float64(quality)*0.0012
	maxBr := int(bpp * float64(pxPerSec) / 1_000_000)
	if maxBr < 1 {
		maxBr = 1
	}
	maxRateStr := strconv.Itoa(maxBr) + "M"
	bufSize := maxBr * 200 // kbits，约 0.2 秒缓冲
	if bufSize < 2000 {
		bufSize = 2000 // 最低 250KB，确保大 IDR 帧能放进 VBV
	}
	if bufSize > 16000 {
		bufSize = 16000 // 最高 2MB，防止极端高码率下 VBV 过大
	}
	bufSizeStr := strconv.Itoa(bufSize) + "k"

	enc := currentH264Encoder()
	switch enc {
	case "h264_nvenc":
		// NVIDIA：p1=最快, ll=低延迟, vbr+cq+maxrate+bufsize(1帧缓冲)
		base = append(base, "-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll",
			"-rc", "vbr", "-cq", hqs, "-maxrate", maxRateStr, "-bufsize", bufSizeStr, "-g", "60")
	case "h264_amf":
		// AMD：speed=最快, cqp+bufsize 约束峰值
		base = append(base, "-c:v", "h264_amf", "-quality", "speed",
			"-rc", "cqp", "-qp_p", hqs, "-qp_i", hqs, "-maxrate", maxRateStr, "-bufsize", bufSizeStr, "-g", "60")
	case "h264_qsv":
		// Intel：veryfast, look_ahead=0 减少延迟
		base = append(base, "-c:v", "h264_qsv", "-preset", "veryfast", "-look_ahead", "0",
			"-async_depth", "1", "-g", "60", "-global_quality", hqs, "-maxrate", maxRateStr, "-bufsize", bufSizeStr)
	case "libx264":
		// CPU 回退：ultrafast+zerolatency+crf+maxrate+bufsize
		base = append(base, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-crf", hqs, "-g", "60", "-x264opts", "slices=1:threads=1",
			"-maxrate", maxRateStr, "-bufsize", bufSizeStr)
	default:
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

// isIDRFrame 检测 H.264 Annex B 帧中是否包含 IDR 切片（关键帧）。
// IDR 帧是解码器参考链的起点，丢弃 IDR 帧会导致画面花屏直到下一个 IDR。
// 因此我们保护 IDR 帧不被丢弃，只丢弃非 IDR（P/B）帧来缓解管道拥塞。
func isIDRFrame(data []byte) bool {
	for i := 0; i < len(data)-3; i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		var nalType byte
		if data[i+2] == 1 && i+3 < len(data) {
			nalType = data[i+3] & 0x1F
		} else if i+4 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			nalType = data[i+4] & 0x1F
		} else {
			continue
		}
		if nalType == 5 { // IDR slice
			return true
		}
	}
	return false
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
		// 丢弃最旧帧保留最新帧。IDR 丢失仅短暂花屏（~0.85s），
		// 远好过硬背压导致 ffmpeg stdout 堵塞的多秒卡死。
		select {
		case ff.frameCh <- frame:
		default:
			select {
			case <-ff.frameCh:
			default:
			}
			ff.frameCh <- frame
		}
		sentFrames = true
		ff.sentFrames = true
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
