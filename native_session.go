package main

import (
	"log"
	"sync"
	"time"

	"web-rdp/native"
)

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

	// 编码器首帧按采集尺寸初始化（偶数）。
	var enc *native.MFH264Encoder
	var tw, th int
	consecErr := 0 // 连续编码错误计数：超过阈值退出，避免空转打满 CPU/内存

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
		// 首次初始化编码器（偶数目标尺寸）。
		// 尺寸策略：前端 maxW>0 时按它；否则默认压到 1920 宽以内，
		// 避免 4K 源 编码/NV12 每帧 11ms+ 导致 fps 上不去。1920 在多数屏上足够清晰。
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
			if tw <= 0 || th <= 0 {
				continue
			}
			// 码率随分辨率与画质估算，避免固定低码率导致模糊。
			br := native.EstimateBitrateForQuality(tw, th, s.quality)
			enc, err = native.NewMFH264EncoderQ(tw, th, 30, br)
			if err != nil {
				log.Printf("[native] MFH264Encoder 初始化失败: %v", err)
				return
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
			// native 帧也写入 WebRTC 轨：让前端 WebRTC 路径真正收到帧，
			// 消除"后端显示 WebRTC 用户、前端却只走 wss"的状态混乱，并降低首屏延迟。
			writeWebRTCSample(s.display, frame, time.Second/30)
		}
		// 限制约 30fps，避免 DXGI 满速喂帧把软件编码器压垮
		timeSleepMs(20)
	}
}

// timeSleepMs 毫秒睡眠（供测试/日志控制）。
func timeSleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

var _ = log.Printf
