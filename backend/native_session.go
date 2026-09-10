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

// 画质维度已移除；码率按固定质量估算（默认中高画质）。
const tierQuality = 80

// nativeSession 满足 streamer 契约的"档位编码会话"。
// 它本身不做采集：帧来自所属显示器的 captureBroker（feed），仅负责
// 编码(硬件异步 MFT 优先→软件 MF 回退) + fan-out 到同档位订阅者 + 写档位 WebRTC 轨。
// 同档位多观众共享同一会话（各自 subscribe 拿独立通道，丢旧保新）。
type nativeSession struct {
	mu           sync.Mutex
	subs         map[int]chan []byte
	nextID       int
	display      int
	key          tierKey
	feed         chan brokerFrame
	trackKey     string
	sent         bool
	produceEnded bool
	closed       bool
	stopCh       chan struct{}
	wg           sync.WaitGroup
	width        int
	height       int
	maxW         int
	quality      int
	fps          int // 目标帧率（>0 节流；0=默认 60）
}

func newNativeSession(display, maxW, fps int, key tierKey, feed chan brokerFrame) *nativeSession {
	if fps <= 0 {
		fps = 60
	}
	return &nativeSession{
		subs:     make(map[int]chan []byte),
		display:  display,
		key:      key,
		feed:     feed,
		trackKey: tierTrackKey(key),
		stopCh:   make(chan struct{}),
		quality:  tierQuality,
		maxW:     maxW,
		fps:      fps,
	}
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

// unsubscribe 实现 streamer。无订阅者时自动停会话（同档位最后一个观众离开即释放）。
func (s *nativeSession) unsubscribe(id int) {
	s.mu.Lock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
	autoStop := !s.closed && len(s.subs) == 0
	s.mu.Unlock()
	if autoStop {
		s.stop()
	}
}

// stop 停会话并清理：关 stopCh → 等产帧/投递 goroutine → 关订阅通道 → 释放档位池与 broker feed。
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
		log.Printf("[tier] stop 等待产帧 goroutine 超时")
	}
	s.mu.Lock()
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.mu.Unlock()

	removeTier(s.key, s)
	releaseBrokerFeed(s.display, s.feed)
}

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

func (s *nativeSession) hasEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.produceEnded
}

func (s *nativeSession) notifyEOF() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.produceEnded = true
	for _, ch := range s.subs {
		select {
		case ch <- nil:
		default:
		}
	}
}

func (s *nativeSession) sourceDisplay() int { return s.display }

func (s *nativeSession) h264Mode() bool { return true }

func (s *nativeSession) ended() bool { return s.hasEnded() }

// start 启动档位产帧 goroutine。
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

// encodeState 每档位会话的编码器状态，跨帧保持（编码器只在首帧后按尺寸初始化一次）。
type encodeState struct {
	enc      nv12Enc
	encName  string
	tw, th   int
	isPush   bool // 硬件异步走 Push/Next 流水线
	lastSend time.Time
	workers  int // 融合缩放+色彩转换的并发度（1=串行）
}

// produce 消费 broker feed 并编码 fan-out。到 stop 或出错退出（广播 EOF）。
func (s *nativeSession) produce() {
	defer s.wg.Done()
	es := &encodeState{}
	defer s.notifyEOF()
	defer func() {
		if es.enc != nil {
			es.enc.Close()
		}
	}()

	var lastEncode time.Time
	consecErr := 0
	target := time.Second / time.Duration(s.fps)
	// 帧率节流用"配额累计"而不是"距上一帧是否够久"：
	// 后者在源帧率 > 目标时会稳定砍半（105Hz 源 → ~52fps），源帧率恰好等于目标时
	// 抖动还会让它间歇性砍半（60Hz 源 → 30~60fps 摆动），这正是"上不去 60fps"的原因。
	// 配额法把每帧的时间增量存进 credit，攒够一帧就编一帧，长期稳定在目标帧率。
	credit := target

	for {
		if !s.hasSubscribers() {
			select {
			case <-s.stopCh:
				return
			case <-time.After(80 * time.Millisecond):
				continue
			}
		}
		select {
		case <-s.stopCh:
			return
		case fr, ok := <-s.feed:
			if !ok {
				log.Printf("[tier] 显示器%d 采集 broker 已结束，档位会话退出", s.display)
				return // broker 关闭（采集失败/无档位）→ EOF
			}
			// 帧率节流：配额不足则丢弃该帧（不编码）。首帧 credit 已给满，必定编码以初始化。
			now := time.Now()
			if !lastEncode.IsZero() {
				credit += now.Sub(lastEncode)
				if credit > 2*target {
					credit = 2 * target // 静止/卡顿后不补爆发，避免一串追赶帧
				}
			} else {
				credit = target
			}
			lastEncode = now
			if credit < target {
				continue // 配额不足：丢弃该帧
			}
			credit -= target

			// 首次初始化编码器（尺寸取自首帧；maxW>0 压到目标宽度，否则用采集全分辨率）。
			if es.enc == nil {
				if !s.initEncoder(es, fr) {
					return // 编码器均不可用 → EOF → 落 JPEG
				}
			}
			// 融合"缩放+色彩转换"一趟直出 NV12：省掉中间 BGRA 缓冲（2560x1070 约 10.7MB）
			// 与二次遍历，并按行分片并行（实测串行 9.5ms → 6 线程 1.9ms，输出逐字节一致）。
			// 注意：不能复用 NV12 缓冲——硬件异步编码器的 Push 只入队，缓冲区要等 pump
			// 拷进 IMFSample 后才可复用，复用会导致帧撕裂（见 asyncencoder.go 契约）。
			nv12 := native.DownscaleBGRAToNV12ParallelInto(
				nil, fr.bgra, fr.w, fr.h, es.tw, es.th, es.workers)
			if es.isPush {
				hw, _ := es.enc.(pushH264)
				hw.Push(nv12)
			} else {
				frame, e2 := es.enc.Encode(nv12)
				if e2 != nil {
					consecErr++
					if consecErr > 30 {
						log.Printf("[tier] 编码连续失败 %d 次(%v)，退出档位会话", consecErr, e2)
						return
					}
					timeSleepMs(30)
					continue
				}
				consecErr = 0
				if len(frame) > 0 {
					dur := measuredFrameDur(&es.lastSend)
					s.fanout(frame)
					writeTierWebRTCSample(s.trackKey, frame, dur)
				}
				timeSleepMs(15)
			}
		case <-time.After(600 * time.Millisecond):
			// broker 存活但桌面静止/无新帧：保持呼吸，等待画面活动。
		}
	}
}

// initEncoder 按首帧尺寸创建/配置编码器（硬件异步 MFT 优先，软件 MF 回退）。
func (s *nativeSession) initEncoder(es *encodeState, fr brokerFrame) bool {
	cw, ch := fr.w, fr.h
	if cw <= 0 || ch <= 0 {
		return false
	}
	tw, th := cw, ch
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
	if tw <= 0 || th <= 0 {
		return false
	}
	// 融合内核并发度：按 CPU 核数与目标高度裁剪（多档位并存时避免过度抢占）。
	es.workers = native.OptimalNV12Workers(th)
	br := native.EstimateBitrateForQuality(tw, th, s.quality)
	hw, hwerr := native.NewAsyncMFH264Encoder(tw, th, br)
	if hwerr == nil {
		es.enc = hw
		es.encName = "hardware-async-MF"
		es.isPush = true
		es.tw, es.th = tw, th
		s.width, s.height = tw, th
		s.wg.Add(1)
		go s.pushDeliver(es, hw)
		log.Printf("[tier] 显示器%d 编码后端=hardware-async-MF 源=%dx%d 编码=%dx%d maxW=%d fps=%d cvt线程=%d", s.display, cw, ch, tw, th, s.maxW, s.fps, es.workers)
		return true
	}
	sw, swerr := native.NewMFH264EncoderQ(tw, th, 30, br)
	if swerr != nil {
		log.Printf("[tier] 显示器%d 硬件(%v)与软件(%v)编码器均初始化失败", s.display, hwerr, swerr)
		return false
	}
	es.enc = sw
	es.encName = "software-sync-MF"
	es.tw, es.th = tw, th
	s.width, s.height = tw, th
	log.Printf("[tier] 显示器%d 编码后端=software-sync-MF(硬件不可用 %v) 源=%dx%d 编码=%dx%d maxW=%d fps=%d cvt线程=%d", s.display, hwerr, cw, ch, tw, th, s.maxW, s.fps, es.workers)
	return true
}

// pushDeliver 独立 goroutine：从硬件异步编码器取编码输出并投递(fanout + 档位 WebRTC)。
func (s *nativeSession) pushDeliver(es *encodeState, pe pushH264) {
	defer s.wg.Done()
	frames := 0
	start := time.Now()
	for {
		b, err := pe.Next()
		if err != nil {
			return // 编码器已停止
		}
		if len(b) == 0 {
			continue
		}
		frames++
		dur := measuredFrameDur(&es.lastSend)
		s.fanout(b)
		writeTierWebRTCSample(s.trackKey, b, dur)
		if time.Since(start) >= 3*time.Second {
			log.Printf("[tier] 显示器%d 投递 %.1f fps @ %dx%d (target=%d)", s.display, float64(frames)/time.Since(start).Seconds(), s.width, s.height, s.fps)
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
