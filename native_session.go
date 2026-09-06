package main

import (
	"log"
	"sync"
	"time"

	"web-rdp/native"
)

// nv12Enc 统一进程内 H.264 编码器（硬件异步 MF 或软件同步 MF），逐帧同步契约。
type nv12Enc interface {
	Encode(nv12 []byte) ([]byte, error)
	Close()
}

// VerboseNative 控制 native 产帧会话的诊断日志（编码后端选择等）。
var VerboseNative = false

// ── nativeSession：满足 streamer 契约的进程内 MF 编码会话 ──
//
// 以 goroutine 持续做 DXGI 直捕 → NV12 → MFH264Encoder（逐帧）→ fan-out 到订阅者。
// 产出的是与 ffmpeg 相同的 H.264 Annex B，前端契约不变。
// 本会话为待办 C 的 native provider 服务端原语；默认 ws.go 仍走 ffmpeg（零回归），
// 仅当 sessionPoolProvider 明确选择 native 时使用。

type nativeSession struct {
	mu       sync.Mutex
	subs     map[int]chan []byte
	nextID   int
	display  int
	sent     bool
	closed   bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	width    int
	height   int
	maxW     int
	quality  int
	fps      int // 目标帧率（>0 时节流产帧到该值；0 = 不强制，跟随采集）
}

func newNativeSession(display, quality, maxW, fps int) *nativeSession {
	if fps <= 0 {
		fps = 60 // 默认平滑目标帧率；用户可下调
	}
	s := &nativeSession{
		subs:    make(map[int]chan []byte),
		display: display,
		stopCh:  make(chan struct{}),
		quality: quality,
		maxW:    maxW,
		fps:     fps,
	}
	s.start()
	return s
}

// subscribe 实现 streamer。
func (s *nativeSession) subscribe() (int, <-chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ch := make(chan []byte, 8)
	s.subs[id] = ch
	return id, ch
}

// unsubscribe 实现 streamer。
func (s *nativeSession) unsubscribe(id int) {
	s.mu.Lock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
	// 无订阅者且未显式关闭时自动停掉产帧 goroutine，避免泄漏。
	autoStop := !s.closed && len(s.subs) == 0
	s.mu.Unlock()
	if autoStop {
		s.stop()
	}
}

// stop 通知产帧 goroutine 退出并清理资源。
// 不死等：produce 可能在 AcquireBGRA(超时≤2s) 或编码调用里短暂停留，
// 这里给一个有限等待，超时则记录并返回，绝不永久阻塞调用方（防止断连堆积）。
func (s *nativeSession) stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stopCh)
	s.mu.Unlock()

	waitCh := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-time.After(3 * time.Second):
		log.Printf("[native] stop 等待产帧 goroutine 超时（可能在阻塞调用中），已返回")
	}
	s.mu.Lock()
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.mu.Unlock()
}

// hasSubscribers 报告当前是否有活跃订阅者。
func (s *nativeSession) hasSubscribers() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs) > 0
}

func (s *nativeSession) hasSentFrames() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func (s *nativeSession) sourceDisplay() int { return s.display }

func (s *nativeSession) h264Mode() bool { return true }

// start 启动产帧 goroutine。
func (s *nativeSession) start() {
	s.wg.Add(1)
	go s.produce()
}

// fanout 把一帧分发给所有订阅者（丢旧保新）。
func (s *nativeSession) fanout(frame []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(frame) > 0 {
		s.sent = true
	}
	for _, ch := range s.subs {
		if len(frame) == 0 {
			continue
		}
		select {
		case ch <- frame:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- frame:
			default:
			}
		}
	}
}

// produce 持续采集桌面并编码，逐帧 fan-out。到 stop 或出错退出。
func (s *nativeSession) produce() {
	defer s.wg.Done()

	// 打开桌面采集。重启/切换场景下旧会话可能尚未释放同一显示器的 DXGI 采集
	// (DuplicateOutput 一次只允许一个)，故失败时带退避重试，等旧采集释放后接管，
	// 避免新会话因撞车而空白/冻结，也是消除崩溃的关键。
	var capture *native.DesktopCapture
	var derr error
	deadline := time.Now().Add(6 * time.Second)
	for {
		capture, derr = native.NewDesktopCapture(s.display)
		if derr == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("[native] 桌面直捕打开失败: %v", derr)
			return
		}
		select {
		case <-s.stopCh:
			return
		case <-time.After(120 * time.Millisecond):
		}
	}
	defer capture.Close()

	// 编码器：硬件异步 MFT 优先，初始化失败则回退软件同步 MF。
	var enc nv12Enc
	var encName string
	var tw, th int
	var lastSend time.Time    // 投递帧时刻（投递 goroutine 更新），用于 RTP 时长
	var lastProduce time.Time // 节流用：采集/入队节奏
	consecErr := 0            // 连续编码错误计数：超过阈值退出，避免空转打满 CPU/内存
	isPush := false           // 硬件异步编码器走 Push/Next 流水线
	feedN := 0                // 已入队(喂)帧计数，用于核对采集/入队是否跟上
	feedT := time.Now()       // 入队统计起点
	var acqTotal time.Duration // AcquireBGRA 累计耗时
	var acqN int               // AcquireBGRA 调用次数

	for {
		// 无订阅者时不必采集/编码，省 CPU 且避免空转；但需快速响应 stop。
		if !s.hasSubscribers() {
			select {
			case <-s.stopCh:
				if enc != nil {
					enc.Close()
				}
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		select {
		case <-s.stopCh:
			if enc != nil {
				enc.Close()
			}
			return
		default:
		}

		acqStart := time.Now()
		bgra, cw, ch, err := capture.AcquireBGRA(2000)
		if err != nil {
			log.Printf("[native] AcquireBGRA: %v", err)
			timeSleepMs(50)
			continue
		}
		if bgra == nil {
			continue // 超时无新帧（桌面静止）
		}
		acqTotal += time.Since(acqStart)
		acqN++
		if cw <= 0 || ch <= 0 {
			continue
		}
		// 首次初始化编码器（偶数、16 对齐目标尺寸）。
		// 尺寸策略：前端 maxW>0 时按它；否则默认压到 1920 宽以内（硬件编码快但避免
		// 过高分辨率 + 高帧率过载，用户可在前端下拉选更高分辨率提升清晰度）。
		if enc == nil {
			tw = cw
			th = ch
			// 尺寸策略：maxW>0 时压到指定宽度；maxW<=0(选"原始")用采集全分辨率，
			// 保证"最高"即真实原始分辨率，不再默认压到 1920 导致放大模糊。
			if s.maxW > 0 && tw > s.maxW {
				tw = s.maxW
				th = ch * tw / cw
			}
			if tw%2 != 0 {
				tw--
			}
			if th%2 != 0 {
				th--
			}
			// 硬件编码器只要求偶数(NV12/宏块内部自衬垫)；16 对齐非必需，且会让 1080p 源
			// 触发无谓的近 1:1 downscale(1080→1072)白耗 CPU。只做偶数对齐。
			if tw <= 0 || th <= 0 {
				continue
			}
			// 码率随分辨率与画质估算，避免固定低码率导致模糊。
			br := native.EstimateBitrateForQuality(tw, th, s.quality)
			// 硬件异步 MFT 优先（跨厂商，系统自带；低每帧开销、高帧率）。
			hw, hwerr := native.NewAsyncMFH264Encoder(tw, th, br)
			if hwerr == nil {
				enc = hw
				encName = "hardware-async-MF"
				isPush = true
				// 硬件流式编码器：独立投递 goroutine 取输出，采集 goroutine 只 Push，
				// 二者解耦→编码器可流水线化，避免每帧同步等输出把 fps 压死。
				s.wg.Add(1)
				go s.pushDeliver(hw, &lastSend)
			} else {
				// 无硬件 MFT 或初始化失败 → 回退软件 MF 编码器。
				sw, swerr := native.NewMFH264EncoderQ(tw, th, 30, br)
				if swerr != nil {
					log.Printf("[native] 硬件(%v)与软件(%v)编码器均初始化失败", hwerr, swerr)
					return
				}
				enc = sw
				encName = "software-sync-MF"
				if VerboseNative {
					log.Printf("[native] 硬件编码器不可用(%v)，回退软件 MF", hwerr)
				}
			}
			// 记录每次编码器/分辨率选择的实际尺寸（始终打印，便于对照前端实际收到的分辨率）
			log.Printf("[native] 编码后端=%s 源=%dx%d 编码=%dx%d maxW=%d targetFPS=%d", encName, cw, ch, tw, th, s.maxW, s.fps)
			s.width = tw
			s.height = th
		}

		// 若采集尺寸与编码尺寸不同则双线性下采样（避免最近邻锯齿）。
		var small []byte
		if cw == tw && ch == th {
			small = bgra
		} else {
			small = native.DownscaleBGRA(bgra, cw, ch, tw, th)
		}
		nv12 := native.BGRAToNV12(small, tw, th)
		if isPush {
			// 流水线：非阻塞入队。编码器繁忙(队列满)则丢帧保新，避免反向背压阻塞采集。
			hw, _ := enc.(pushH264)
			if hw.Push(nv12) {
				feedN++
			}
		} else {
			frame, e2 := enc.Encode(nv12)
			if e2 != nil {
				consecErr++
				if consecErr > 30 {
					// 编码器持续失败（状态坏了），退出避免空转打满 CPU/内存。
					log.Printf("[native] 编码连续失败 %d 次(%v)，退出产帧循环", consecErr, e2)
					enc.Close()
					return
				}
				log.Printf("[native] MF 编码: %v", e2)
				timeSleepMs(50) // 退避，别高频空转
				continue
			}
			consecErr = 0 // 成功则清零
			if len(frame) > 0 {
				// 按真实帧间隔给 RTP 时长，让时间戳与真实时间对齐，避免播放缓冲累积。
				dur := measuredFrameDur(&lastSend)
				s.fanout(frame)
				writeWebRTCSample(s.display, frame, dur)
			}
		}
		// 节流：软件编码器慢，20ms 保护；硬件按目标帧率 s.fps 限制采集/入队速率，
		// 让"帧率选择"真正影响投递速率（画面活动时最多按所选 fps 产帧）。
		if encName == "software-sync-MF" {
			timeSleepMs(20)
			continue
		}
		target := time.Second / time.Duration(s.fps)
		if !lastProduce.IsZero() {
			if since := time.Since(lastProduce); since < target {
				time.Sleep(target - since)
			}
		}
		lastProduce = time.Now()
		if isPush && feedN > 0 && time.Since(feedT) >= 3*time.Second {
			avgAcq := float64(acqTotal.Milliseconds()) / float64(max(acqN, 1))
			log.Printf("[native] 入队(喂) %.1f fps @ %dx%d (target=%d) 采集平均%.1fms/帧 采到%d帧", float64(feedN)/time.Since(feedT).Seconds(), s.width, s.height, s.fps, avgAcq, acqN)
			feedN, feedT, acqTotal, acqN = 0, time.Now(), 0, 0
		}
	}
}

// pushDeliver 独立 goroutine：从硬件异步编码器持续取编码输出并投递(fanout + WebRTC)。
// 采集 goroutine 只 Push，二者解耦，让编码器内部流水线化以支撑高帧率。
func (s *nativeSession) pushDeliver(pe pushH264, last *time.Time) {
	defer s.wg.Done()
	frames := 0
	start := time.Now()
	for {
		b, err := pe.Next()
		if err != nil {
			return // 编码器已停止或致命错误
		}
		if len(b) == 0 {
			continue
		}
		frames++
		dur := measuredFrameDur(last)
		s.fanout(b)
		writeWebRTCSample(s.display, b, dur)
		if time.Since(start) >= 3*time.Second {
			log.Printf("[native] 投递 %.1f fps @ %dx%d (target=%d)", float64(frames)/time.Since(start).Seconds(), s.width, s.height, s.fps)
			frames, start = 0, time.Now()
		}
	}
}

// pushH264 硬件异步编码器的流水线接口（Push 非阻塞入队 + Next 阻塞取输出）。
type pushH264 interface {
	Push(nv12 []byte) bool
	Next() ([]byte, error)
}

// measuredFrameDur 计算距上次发送的实测间隔作为本帧 RTP 时长，并更新 last。
// 首个帧无基准时给 ~16ms(≈60fps 的合理占位)。间隔钳制在 [1ms, 250ms] 防止异常。
func measuredFrameDur(last *time.Time) time.Duration {
	now := time.Now()
	if last.IsZero() {
		*last = now
		return time.Second / 60
	}
	d := now.Sub(*last)
	*last = now
	if d < time.Millisecond {
		return time.Millisecond
	}
	if d > 250*time.Millisecond {
		return 250 * time.Millisecond
	}
	return d
}

// timeSleepMs 毫秒睡眠（供测试/日志控制）。
func timeSleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

var _ = log.Printf
