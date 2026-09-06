//go:build windows

package native

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ── AsyncMFH264Encoder：异步硬件 MFT 的同步式封装 ──
//
// 异步硬件编码器（NVIDIA H.264 Encoder MFT 等）本身是事件驱动流式模型：
//   METransformNeedInput(601)  → 喂一帧 NV12
//   METransformHaveOutput(602) → 取一帧编码输出
// 与 MFH264Encoder（软件，同步 feed→drain→collect）不同，硬件编码器不应每帧 drain；
// 它应在同一会话内连续流式编码，这正是低每帧开销、高帧率的关键。
//
// 本封装把这些异步语义藏在后台泵 goroutine 里，对外提供与 MFH264Encoder 相同
// 的同步 Encode(nv12 []byte) ([]byte, error) 契约，使 native_session 无需改造成
// 完全异步即可接硬件编码器。每个 Encode 喂入一帧并等待其对应的一帧输出返回。
//
// 注意：调用方必须每帧新建 NV12 切片（本封装不拷贝，直接引用到喂入完成）。

type asyncMFEncoder struct {
	sess   *asyncHWEncoder
	feedCh chan []byte // 调用方入队的待编码 NV12 帧
	outCh  chan []byte // 已编码输出（按入队顺序）
	errCh  chan error  // 泵 goroutine 致命错误
	stop   chan struct{}
	wg     sync.WaitGroup

	mu    sync.Mutex
	sps   []byte
	pps   []byte
	closed bool
}

// NewAsyncMFH264Encoder 建立异步硬件 H.264 编码会话并启动泵 goroutine。
func NewAsyncMFH264Encoder(width, height int, bitrate uint32) (*asyncMFEncoder, error) {
	enc := &asyncMFEncoder{
		feedCh: make(chan []byte, 4),
		outCh:  make(chan []byte, 4),
		errCh:  make(chan error, 2),
		stop:   make(chan struct{}),
	}
	sess, err := newAsyncEncoderSession(width, height, bitrate)
	if err != nil {
		return nil, err
	}
	enc.sess = sess
	enc.wg.Add(1)
	go enc.pump()
	return enc, nil
}

// newAsyncEncoderSession 激活+解锁+设D3D+配置类型+启动流式，返回持久的异步会话。
func newAsyncEncoderSession(width, height int, bitrate uint32) (*asyncHWEncoder, error) {
	enc, err := activateAsyncEncoder(width, height)
	if err != nil {
		return nil, err
	}
	if err := enc.acquireEventGenerator(); err != nil {
		enc.Close()
		return nil, err
	}
	if err := enc.attachD3D(); err != nil {
		enc.Close()
		return nil, err
	}
	enc.bitrate = bitrate
	if err := enc.configureTypes(); err != nil {
		enc.Close()
		return nil, err
	}
	if err := enc.startStreaming(); err != nil {
		enc.Close()
		return nil, err
	}
	return enc, nil
}

// pump 后台事件循环：处理 NeedInput/HaveOutput，喂入队帧、送出输出。
func (e *asyncMFEncoder) pump() {
	defer e.wg.Done()
	var cur []byte    // 当前待喂帧
	awaiting := false // MFT 已发出 NeedInput、等待输入中
	for {
		select {
		case <-e.stop:
			_ = tProcessMessage(e.sess.tr, mftMsgCommandDrain, 0)
			e.drainRest()
			return
		default:
		}
		// 排空当前事件
		for {
			typ, ok, hr := e.sess.getEventNoWait()
			if !ok {
				if hr != mfENoEventsAvailable {
					select {
					case e.errCh <- errors.New("GetEvent 失败"):
					default:
					}
					return
				}
				break // 无事件
			}
			switch typ {
			case evTransformNeedInput:
				awaiting = true
			case evTransformHaveOutput:
				acc, err := e.sess.collectOutput(nil)
				if err != nil {
					select {
					case e.errCh <- err:
					default:
					}
					return
				}
				if len(acc) > 0 {
					e.cacheSPSPPS(acc)
					select {
					case e.outCh <- acc:
					default:
						select {
						case <-e.outCh:
						default:
						}
						select {
						case e.outCh <- acc:
						default:
						}
					}
				}
			case evError:
				select {
				case e.errCh <- errors.New("异步硬件编码器 MEError"):
				default:
				}
				return
			}
		}
		// MFT 等待输入且有帧就绪 → 补喂（不依赖新的 NeedInput 事件）
		if awaiting {
			if cur == nil {
				select {
				case cur = <-e.feedCh:
				default:
				}
			}
			if cur != nil {
				if e2 := e.feedOne(cur); e2 == nil {
					cur = nil
					awaiting = false // 已喂入，等下一次 NeedInput
				} else if !strings.Contains(e2.Error(), "0xc00d6d76") {
					select {
					case e.errCh <- e2:
					default:
					}
					return
				}
				// NOTACCEPTING：保持 cur & awaiting，稍后重试
			}
		}
		time.Sleep(150 * time.Microsecond)
	}
}

// feedOne 把一帧 NV12 交给 MFT（构造 IMFSample + ProcessInput）。
func (e *asyncMFEncoder) feedOne(nv12 []byte) error {
	sample, err := makeSample()
	if err != nil {
		return err
	}
	if err := putNV12InSample(sample, nv12); err != nil {
		comRelease(sample)
		return err
	}
	_ = sampleSetTime(sample, int64(1e7)/60)
	_ = sampleSetDuration(sample, int64(1e7)/60)
	e2 := tProcessInput(e.sess.tr, e.sess.inID, sample, 0)
	comRelease(sample)
	return e2
}

// Encode 喂入一帧 NV12，阻塞等待其编码输出返回（同步契约）。
func (e *asyncMFEncoder) Encode(nv12 []byte) ([]byte, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("编码器已关闭")
	}
	e.mu.Unlock()
	select {
	case e.feedCh <- nv12:
	case <-time.After(3 * time.Second):
		return nil, errors.New("喂帧超时（管道阻塞）")
	case <-e.stop:
		return nil, errors.New("编码器停止")
	}
	select {
	case out := <-e.outCh:
		return out, nil
	case err := <-e.errCh:
		return nil, err
	case <-time.After(5 * time.Second):
		return nil, errors.New("取输出超时")
	case <-e.stop:
		return nil, errors.New("编码器停止")
	}
}

// cacheSPSPPS 从含 SPS/PPS 的输出里缓存参数集（供 Encoder 接口/新订阅者）。
func (e *asyncMFEncoder) cacheSPSPPS(acc []byte) {
	var sps, pps []byte
	for i := 0; i+3 < len(acc); {
		nalType := byte(255)
		step := 1
		if acc[i] == 0 && acc[i+1] == 0 && acc[i+2] == 1 {
			nalType = acc[i+3] & 0x1f
			step = 4
		} else if i+3 < len(acc) && acc[i] == 0 && acc[i+1] == 0 && acc[i+2] == 0 && acc[i+3] == 1 {
			nalType = acc[i+4] & 0x1f
			step = 5
		}
		end := i + step
		for end+2 < len(acc) && !(acc[end] == 0 && acc[end+1] == 0) {
			end++
		}
		payload := acc[i+step : end]
		switch nalType {
		case 7:
			sps = payload
		case 8:
			pps = payload
		}
		i = end
		if i >= len(acc) {
			break
		}
	}
	if len(sps) > 0 && len(pps) > 0 {
		e.mu.Lock()
		e.sps, e.pps = sps, pps
		e.mu.Unlock()
	}
}

// SPSPPS 返回缓存的 SPS/PPS NAL（不含起始码）。
func (e *asyncMFEncoder) SPSPPS() ([]byte, []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sps, e.pps
}

// drainRest 在关闭前尝试冲刷剩余输出（尽力而为，不阻塞）。
func (e *asyncMFEncoder) drainRest() {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		acc, err := e.sess.collectOutput(nil)
		if err != nil || len(acc) == 0 {
			break
		}
		select {
		case e.outCh <- acc:
		default:
		}
	}
}

// Close 停止泵 goroutine 并释放会话。
func (e *asyncMFEncoder) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.stop)
	e.mu.Unlock()
	e.wg.Wait()
	if e.sess != nil {
		e.sess.Close()
		e.sess = nil
	}
}
