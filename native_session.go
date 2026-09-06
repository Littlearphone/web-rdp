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
}

func newNativeSession(display, quality, maxW, fps int) *nativeSession {
	s := &nativeSession{
		subs:    make(map[int]chan []byte),
		display: display,
		stopCh:  make(chan struct{}),
		quality: quality,
		maxW:    maxW,
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

	capture, err := native.NewDesktopCapture(s.display)
	if err != nil {
		log.Printf("[native] 桌面直捕打开失败: %v", err)
		return
	}
	defer capture.Close()

	// 编码器：硬件异步 MFT 优先，初始化失败则回退软件同步 MF。
	var enc nv12Enc
	var encName string
	var tw, th int
	var lastSend time.Time // 上次发送帧的时刻，用于按真实间隔给 RTP 时长
	consecErr := 0         // 连续编码错误计数：超过阈值退出，避免空转打满 CPU/内存

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

		bgra, cw, ch, err := capture.AcquireBGRA(2000)
		if err != nil {
			log.Printf("[native] AcquireBGRA: %v", err)
			timeSleepMs(50)
			continue
		}
		if bgra == nil {
			continue // 超时无新帧（桌面静止）
		}
		if cw <= 0 || ch <= 0 {
			continue
		}
		// 首次初始化编码器（偶数、16 对齐目标尺寸）。
		// 尺寸策略：前端 maxW>0 时按它；否则默认压到 1920 宽以内（硬件编码快但避免
		// 过高分辨率 + 高帧率过载，用户可在前端下拉选更高分辨率提升清晰度）。
		if enc == nil {
			tw = cw
			th = ch
			maxTarget := s.maxW
			if maxTarget <= 0 {
				maxTarget = 1920
			}
			if tw > maxTarget {
				tw = maxTarget
				th = ch * tw / cw
			}
			if tw%2 != 0 {
				tw--
			}
			if th%2 != 0 {
				th--
			}
			// 硬件 MFT 按 16×16 宏块处理，需对齐到 16 的倍数，否则编码时越界崩溃。
			tw -= tw % 16
			th -= th % 16
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
			if VerboseNative {
				log.Printf("[native] 编码后端=%s %dx%d", encName, tw, th)
			}
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
		frame, err := enc.Encode(nv12)
		if err != nil {
			consecErr++
			if consecErr > 30 {
				// 编码器持续失败（状态坏了），退出避免每 20ms 空转打满 CPU/内存。
				log.Printf("[native] 编码连续失败 %d 次(%v)，退出产帧循环", consecErr, err)
				enc.Close()
				return
			}
			log.Printf("[native] MF 编码: %v", err)
			timeSleepMs(50) // 退避，别高频空转
			continue
		}
		consecErr = 0 // 成功则清零
		if len(frame) > 0 {
			s.fanout(frame)
			// 按真实帧间隔给 RTP 时长：硬件路径按采集速度推帧，若固定用 33ms(30fps)
			// 会造成"时间戳比真实时间走得慢"→ 浏览器播放缓冲持续累积 → 延迟递增、
			// 显示帧率下降。用实测间隔做 Duration 让 RTP 时间戳与真实时间对齐，消除漂移。
			dur := measuredFrameDur(&lastSend)
			writeWebRTCSample(s.display, frame, dur)
		}
		// 节流：软件编码器慢，需 20ms 保护避免积压；硬件编码器极快，交给采集帧率驱动
		//（DXGI 只在新帧变化时返回），仅留 1ms 防极速空转，以支持高帧率。
		if encName == "software-sync-MF" {
			timeSleepMs(20)
		} else {
			timeSleepMs(1)
		}
	}
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
