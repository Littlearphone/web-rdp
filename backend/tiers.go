package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"web-rdp/native"
)

// ── per-display 共享采集 broker + 档位键控会话池 ──
//
// ffmpeg 与旧的"每连接一个采集+编码"都已让位给这个结构：
//   - DXGI DuplicateOutput 每输出只允许一个采集者 → 每显示器**一个 capture broker**
//     （引用计数），把原始 BGRA 帧广播给该显示器下的所有档位会话。
//   - 档位 = (display, 目标宽度 maxW, 帧率 fps)。画质维度已移除（码率按固定质量估算）。
//     每个档位一个编码会话（nativeSession，仅消费 broker 帧 + 自持编码器），
//     同屏不同档位共享同一路采集、各自编码；同档位多观众共享同一会话（多订阅 fan-out）。
//   - broker 打开采集失败 → 关闭各档位 feed → 会话广播 EOF → ws.go 落纯 Go JPEG。

type tierKey struct {
	display int
	maxW    int // 0 = 原始分辨率
	fps     int // 0 = 自动/默认 60
}

// brokerFrame 一帧原始桌面（BGRA + 尺寸）。broker 每帧新分配，只读共享给各档位。
type brokerFrame struct {
	bgra []byte
	w, h int
}

// captureBroker 每显示器一个。run() 采集并广播到 feeds（丢旧保新，不背压采集）。
type captureBroker struct {
	display int
	mu      sync.Mutex
	feeds   map[chan brokerFrame]struct{}
	stopCh  chan struct{}
	down    bool // 已结束（正常无订阅 或 采集失败）
	started bool
	wg      sync.WaitGroup
}

var (
	brokerMu sync.Mutex
	brokers  = map[int]*captureBroker{}
)

// acquireBrokerFeed 取得(display 的)共享 broker 的一路 feed，首次调用时启动采集 goroutine。
func acquireBrokerFeed(display int) chan brokerFrame {
	brokerMu.Lock()
	b := brokers[display]
	if b == nil {
		b = &captureBroker{display: display, feeds: map[chan brokerFrame]struct{}{}, stopCh: make(chan struct{})}
		brokers[display] = b
	}
	b.mu.Lock()
	if b.down {
		// 上一实例已结束，替换为新的（避免旧 stopCh 已被关闭）。
		delete(brokers, display)
		b = &captureBroker{display: display, feeds: map[chan brokerFrame]struct{}{}, stopCh: make(chan struct{})}
		brokers[display] = b
	}
	ch := make(chan brokerFrame, 8)
	b.feeds[ch] = struct{}{}
	first := !b.started
	if first {
		b.started = true
	}
	b.mu.Unlock()
	brokerMu.Unlock()
	if first {
		b.wg.Add(1)
		go b.run()
	}
	return ch
}

// releaseBrokerFeed 移除一路 feed；若已是最后一档，结束采集并清理。
func releaseBrokerFeed(display int, ch chan brokerFrame) {
	brokerMu.Lock()
	b := brokers[display]
	brokerMu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.feeds, ch)
	teardown := len(b.feeds) == 0 && !b.down
	if teardown {
		b.down = true
	}
	b.mu.Unlock()
	if teardown {
		close(b.stopCh)
		brokerMu.Lock()
		if brokers[display] == b {
			delete(brokers, display)
		}
		brokerMu.Unlock()
	}
}

// dispatch 把一帧广播给当前各档位 feed（非阻塞丢旧保新）。与 down/brokerDown 在同一把锁串行。
func (b *captureBroker) dispatch(fr brokerFrame) {
	b.mu.Lock()
	if b.down {
		b.mu.Unlock()
		return
	}
	for ch := range b.feeds {
		select {
		case ch <- fr:
		default: // 该档位消费慢则丢帧，不阻塞采集
		}
	}
	b.mu.Unlock()
}

// brokerDown 采集致命失败：标记 down、关闭所有 feed 通知档位 EOF、停 goroutine。
func (b *captureBroker) brokerDown() {
	b.mu.Lock()
	if b.down {
		b.mu.Unlock()
		return
	}
	b.down = true
	for ch := range b.feeds {
		close(ch)
	}
	b.mu.Unlock()
	close(b.stopCh)
	brokerMu.Lock()
	if brokers[b.display] == b {
		delete(brokers, b.display)
	}
	brokerMu.Unlock()
}

// run 采集主循环。开采集带退避重试（DXGI 偶尔瞬时不稳）；直到 stop 或致命错误退出。
func (b *captureBroker) run() {
	defer b.wg.Done()

	var cap *native.DesktopCapture
	var err error
	deadline := time.Now().Add(6 * time.Second)
	for {
		cap, err = native.NewDesktopCapture(b.display)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("[broker] 显示器%d 桌面直捕打开失败: %v", b.display, err)
			b.brokerDown()
			return
		}
		select {
		case <-b.stopCh:
			return
		case <-time.After(120 * time.Millisecond):
		}
	}
	defer cap.Close()

	for {
		// 检查是否应结束（无任何档位订阅时 stopCh 被关）。
		select {
		case <-b.stopCh:
			return
		default:
		}
		bgra, cw, ch, aerr := cap.AcquireBGRA(2000)
		if aerr != nil {
			log.Printf("[broker] 显示器%d AcquireBGRA: %v", b.display, aerr)
			timeSleepMs(50)
			continue
		}
		if bgra == nil {
			continue // 桌面静止，超时无新帧
		}
		if cw <= 0 || ch <= 0 {
			continue
		}
		b.dispatch(brokerFrame{bgra: bgra, w: cw, h: ch})
	}
}

// ── 档位会话池（(display,maxW,fps) 键控，同档位共享）──

var (
	tierMu sync.Mutex
	tiers  = map[tierKey]*nativeSession{}
)

// acquireTier 返回指定档位的共享会话（不存在则创建；已结束的过期会话会被替换新建）。
func acquireTier(display, maxW, fps int) *nativeSession {
	key := tierKey{display: display, maxW: maxW, fps: fps}
	tierMu.Lock()
	defer tierMu.Unlock()
	if s, ok := tiers[key]; ok && !s.hasEnded() {
		return s
	}
	delete(tiers, key) // 去掉已结束的过期会话
	feed := acquireBrokerFeed(display)
	s := newNativeSession(display, maxW, fps, key, feed)
	tiers[key] = s
	s.start()
	return s
}

// removeTier 档位会话停用后从池中移除（由 nativeSession.stop 调用）。
func removeTier(key tierKey, s *nativeSession) {
	tierMu.Lock()
	if tiers[key] == s {
		delete(tiers, key)
	}
	tierMu.Unlock()
}

// tierTrackKey 派生该档位的 WebRTC 视频轨键（轨道按档位细分），可读文本。
func tierTrackKey(k tierKey) string {
	return fmt.Sprintf("%d-%d-%d", k.display, k.maxW, k.fps)
}
