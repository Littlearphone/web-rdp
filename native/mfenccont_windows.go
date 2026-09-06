//go:build windows

package native

import (
	"fmt"
	"strings"
	"unsafe"
)

// estimateBitrate 按分辨率和帧率估算合理目标码率（bps）。
// 屏幕内容需较高码率才清晰；参考经验：1080p@30 屏幕内容约 4-8 Mbps。
// quality 影响（0..100，默认 70）线性缩放系数。
func estimateBitrate(w, h, fps int) uint32 {
	pixels := w * h
	f := fps
	if f <= 0 {
		f = 30
	}
	// bits per pixel per frame（屏幕内容经验值 0.08-0.35）
	var bpp float64
	if pixels >= 1920*1080 {
		bpp = 0.10
	} else if pixels >= 1280*720 {
		bpp = 0.14
	} else {
		bpp = 0.20
	}
	br := float64(pixels) * bpp * float64(f)
	if br < 1_500_000 {
		br = 1_500_000
	}
	if br > 30_000_000 {
		br = 30_000_000
	}
	return uint32(br)
}

// NewMFH264EncoderQ 按显式码率/画质创建编码器。bitrate<=0 时按分辨率自动估算。
func NewMFH264EncoderQ(width, height, fps int, bitrate uint32) (*MFH264Encoder, error) {
	enc, err := newMFH264EncoderCore(width, height, fps, 0, bitrate)
	return enc, err
}

// EstimateBitrateForQuality 依分辨率与用户画质(0..100)估算码率(bps)。
// 画质越高、像素越多、帧率越高 → 码率越高；同时设下限/上限避免过低模糊或过高浪费。
func EstimateBitrateForQuality(w, h, quality int) uint32 {
	pixels := w * h
	if quality <= 0 {
		quality = 70
	}
	if quality > 100 {
		quality = 100
	}
	// 参考基数：1080p@30 屏幕内容在 q=70 时约 5 Mbps；像素线性缩放。
	base := float64(pixels) / float64(1920*1080) * 5_000_000
	// q=30 → x0.35; q=70 → x1.0; q=100 → x2.2
	qf := 0.35 + float64(quality-30)*1.0/70.0*(2.2-0.35)
	if quality < 30 {
		qf = 0.35
	}
	br := base * qf
	if br < 1_000_000 {
		br = 1_000_000
	}
	if br > 40_000_000 {
		br = 40_000_000
	}
	return uint32(br)
}



// ── 可复用的连续 MF H.264 编码器 ──
//
// MFH264Encoder 是一次配置、多帧喂入的有状态编码器（对齐 Encoder 接口语义），
// 是 ws.go native 会话（④b）真正会使用的原语。与单帧冒烟不同，它不在每帧重建会话：
//   Init 时实例化并配置一次 IMFTransform；Encode 逐帧喂 NV12 并取回本步已就绪的 Annex B。
//
// 同步 MF 编码器通常需少量 look-ahead：喂入第 N 帧后可能尚未就绪，需在喂入 N+1 或 Flush
// 时才产出第 N 帧。因此对外提供:
//   - Encode(nv12): 喂入一帧，返回"本轮已就绪"的 H.264 Annex B（可能为 nil=尚在缓冲）。
//   - Flush(): 排空，返回最后缓冲的输出（关键帧/剩余帧），编码完成后必须调用以取全。
type MFH264Encoder struct {
	tr      unsafe.Pointer // IMFTransform*（已配置）
	width   int
	height  int
	pts     int64 // 递增时间戳（100ns）
	started bool  // 是否已 BEGIN_STREAMING
	closed  bool
	profile uint32 // 0=由编码器决定; 66=baseline(无B帧)
	bitrate uint32 // 目标码率 bps（0=按分辨率/画质自动估算）
}

// NewMFH264Encoder 实例化并配置一个 H.264 编码器（码率按分辨率自动估算）。
func NewMFH264Encoder(width, height, fps int) (*MFH264Encoder, error) {
	return newMFH264EncoderCore(width, height, fps, 0, 0)
}

// newMFH264EncoderCore 共享核心：profile=0 默认；bitrate=0 时自动估算。
func newMFH264EncoderCore(width, height, fps int, profile uint32, bitrate uint32) (*MFH264Encoder, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("非法尺寸 %dx%d", width, height)
	}
	if !mfStartup() {
		return nil, fmt.Errorf("MFStartup 失败")
	}

	acts, _, err := enumH264Activates()
	if err != nil {
		mfShutdown()
		return nil, err
	}
	defer func() {
		for _, a := range acts {
			comRelease(a)
		}
	}()
	if len(acts) == 0 {
		mfShutdown()
		return nil, fmt.Errorf("无可用 H.264 编码器 MFT")
	}

	// 逐个尝试实例化 + 配置；命中第一个成功的。
	for i := range acts {
		e := &MFH264Encoder{tr: nil, width: width, height: height, profile: profile, bitrate: bitrate}
		if err := e.initFromActivate(acts[i], fps); err == nil {
			e.pts = 0
			return e, nil
		}
		// 该激活实例化失败，释放本尝试内的 transform（若已建）
		if e.tr != nil {
			comRelease(e.tr)
			e.tr = nil
		}
	}
	mfShutdown()
	return nil, fmt.Errorf("所有 H.264 编码器配置失败")
}

// tryEncoderSingleActivate 尝试仅用单个 IMFActivate 构造一个 MFH264Encoder。
// 用于硬件候选探测：逐一对每个候选跑真实编码流程，判断该编码器是否能同步逐帧。
// 成功时返回可用的编码器；失败返回错误。调用方负责 comRelease 传入的 activate。
func tryEncoderSingleActivate(activate unsafe.Pointer, width, height int) (*MFH264Encoder, error) {
	if !mfStartup() {
		return nil, fmt.Errorf("MFStartup 失败")
	}
	e := &MFH264Encoder{tr: nil, width: width, height: height, profile: 0, bitrate: 0}
	if err := e.initFromActivate(activate, 30); err != nil {
		mfShutdown()
		return nil, err
	}
	e.pts = 0
	return e, nil
}


// initFromActivate 对单个 IMFActivate 完成 activate + 类型配置 + BEGIN_STREAMING。
func (e *MFH264Encoder) initFromActivate(activate unsafe.Pointer, fps int) error {
	tr, err := activateTransform(activate)
	if err != nil {
		return err
	}
	e.tr = tr
	e.started = true

	// 输出类型：软件编码器需 AVG_BITRATE + 渐进扫描。
	// 码率默认按分辨率+像素估算，避免固定 2Mbps 在清晰/大屏下模糊。
	br := e.bitrate
	if br <= 0 {
		br = estimateBitrate(e.width, e.height, fps)
	}
	outMT, err := buildVideoType(0x34363248, uint32(e.width), uint32(e.height), 60, 1, true, br)
	if err != nil {
		return err
	}
	// 若指定 baseline profile（无 B 帧），写入 MF_MT_MPEG2_PROFILE=66，
	// 期望消除重排缓冲 → 编码器能逐帧即时产出。
	if e.profile != 0 {
		_ = attrSetUINT32(outMT, &guidMTMPEG2Profile, e.profile)
	}
	err = tSetOutputType(tr, 0, outMT)
	comRelease(outMT)
	if err != nil {
		// 盲构造失败 → 用可用输出类型补全后重试
		outMT2, e2 := pickAvailableOutputType(tr, 0)
		if e2 == nil {
			if e.profile != 0 {
				_ = attrSetUINT32(outMT2, &guidMTMPEG2Profile, e.profile)
			}
			e2 = completeTypeSize(outMT2, uint32(e.width), uint32(e.height), 60, 1)
			if e2 == nil {
				e2 = tSetOutputType(tr, 0, outMT2)
			}
			comRelease(outMT2)
			err = e2
		}
		if err != nil {
			return err
		}
	}

	inMT, err := buildVideoType(0x3231564e, uint32(e.width), uint32(e.height), 60, 1, true, 0) // 'NV12'
	if err != nil {
		return err
	}
	err = tSetInputType(tr, 0, inMT)
	comRelease(inMT)
	if err != nil {
		return err
	}

	return tProcessMessage(tr, mftMsgNotifyBeginStreaming, 0)
}

// feedOne 仅喂入一帧 NV12（不拉取输出）。用于研究 MFT 的 look-ahead 行为/流式驱动。
func (e *MFH264Encoder) feedOne(nv12 []byte) error {
	if e.closed || e.tr == nil {
		return fmt.Errorf("编码器未就绪/已关闭")
	}
	expect := e.width * e.height * 3 / 2
	if len(nv12) < expect {
		return fmt.Errorf("NV12 数据不足: got=%d want=%d", len(nv12), expect)
	}
	sample, err := makeSample()
	if err != nil {
		return err
	}
	defer comRelease(sample)
	if err := putNV12InSample(sample, nv12); err != nil {
		return err
	}
	if err := sampleSetTime(sample, e.pts); err != nil {
		return err
	}
	e.pts += 10_000_000 / 60
	_ = sampleSetDuration(sample, 10_000_000/60)
	return tProcessInput(e.tr, 0, sample, 0)
}

// Encode 喂入一帧 NV12 并返回对应编码帧的 H.264 Annex B。
//
// 实测：微软软件 H264 Encoder MFT 在纯"ProcessInput→ProcessOutput"同步模型下不会逐帧产出，
// 而是把所有输入缓冲到 DRAIN 才集中输出。可行的实时流式范式是"喂一帧 → 发 DRAIN → 收集输出"，
// 编码器在 DRAIN 后仍接受后续输入并可每 tick 产出一帧（首帧含 SPS/PPS/IDR）。
func (e *MFH264Encoder) Encode(nv12 []byte) ([]byte, error) {
	if e.closed || e.tr == nil {
		return nil, fmt.Errorf("编码器未就绪/已关闭")
	}
	expect := e.width * e.height * 3 / 2
	if len(nv12) < expect {
		return nil, fmt.Errorf("NV12 数据不足: got=%d want=%d", len(nv12), expect)
	}
	sample, err := makeSample()
	if err != nil {
		return nil, err
	}
	defer comRelease(sample)
	if err := putNV12InSample(sample, nv12); err != nil {
		return nil, err
	}
	if err := sampleSetTime(sample, e.pts); err != nil {
		return nil, err
	}
	e.pts += 10_000_000 / 60
	_ = sampleSetDuration(sample, 10_000_000/60)
	if err := tProcessInput(e.tr, 0, sample, 0); err != nil {
		return nil, err
	}
	// 发 DRAIN 强制编码器落盘当前帧，再收集全部就绪输出。
	if err := tProcessMessage(e.tr, mftMsgCommandDrain, 0); err != nil {
		return nil, err
	}
	var acc []byte
	for {
		d, err := e.pollOnce()
		if err != nil {
			return acc, err
		}
		if len(d) == 0 {
			break
		}
		acc = append(acc, d...)
	}
	// DRAIN 后编码器视流为结束；为支持连续喂帧，需重新进入 streaming 状态。
	if err := tProcessMessage(e.tr, mftMsgNotifyBeginStreaming, 0); err != nil {
		return acc, err
	}
	return acc, nil
}

// outputBufferSize 估算一次输出样本所需缓冲（含裕量），随分辨率增长。
// 1080p 关键帧在 11Mbps 下可能接近数 MB；太大可能整帧未压缩 >2MB，故动态放大。
func outputBufferSize(w, h int) int {
	base := 2 << 20 // 2MB
	px := w * h
	if px > 1280*720 {
		// 每像素预留更充裕空间：接近按码率/8bpp 估算 + 裕量
		// 关键帧峰值可达 ~0.1-0.3 bytes/pixel 甚至更多，取 0.5B/px 保底
		est := px / 2
		if est > base {
			return est
		}
	}
	return base
}

// pollOnce 尝试取一帧已就绪输出，无则返回 nil。
// 若输出缓冲不足(BUFFERTOOSMALL)则自动放大重试。
func (e *MFH264Encoder) pollOnce() ([]byte, error) {
	capacity := outputBufferSize(e.width, e.height)
	for attempt := 0; attempt < 4; attempt++ {
		outSample, err := makeSample()
		if err != nil {
			return nil, err
		}
		buf, err := makeBuffer(capacity)
		if err != nil {
			comRelease(outSample)
			return nil, err
		}
		if err := sampleAddBuffer(outSample, buf); err != nil {
			comRelease(outSample)
			return nil, err
		}
		ob := mftOutputDataBuffer{streamID: 0, pSample: outSample}
		s, err := tProcessOutput(e.tr, &ob)
		if err != nil {
			comRelease(outSample)
			// BUFFERTOOSMALL：缓冲不足，翻倍重试
			if isBufferTooSmall(err) {
				capacity *= 2
				continue
			}
			return nil, err
		}
		if s == nil {
			comRelease(outSample)
			return nil, nil // 尚无就绪帧
		}
		// 取样本全部 buffer
		var data []byte
		var i uint32
		for {
			bb, berr := sampleGetBufferByIndex(outSample, i)
			if berr != nil || bb == nil {
				break
			}
			d, derr := bufferCopyOut(bb)
			if derr == nil && len(d) > 0 {
				data = append(data, d...)
			}
			comRelease(bb)
			i++
		}
		comRelease(outSample)
		return data, nil
	}
	return nil, fmt.Errorf("输出缓冲持续不足")
}

// isBufferTooSmall 判断错误是否为 MF_E_BUFFERTOOSMALL。
func isBufferTooSmall(err error) bool {
	s := fmt.Sprintf("%v", err)
	return strings.Contains(s, "0xc00d36b1") || strings.Contains(s, "BUFFERTOOSMALL")
}

// Flush 排空编码器，返回剩余缓冲的输出（通常为最后一帧）。调用后应 Close。
func (e *MFH264Encoder) Flush() ([]byte, error) {
	if e.tr == nil {
		return nil, fmt.Errorf("编码器未就绪")
	}
	if err := tProcessMessage(e.tr, mftMsgCommandDrain, 0); err != nil {
		return nil, err
	}
	var acc []byte
	for {
		d, err := e.pollOnce()
		if err != nil {
			return acc, err
		}
		if len(d) == 0 {
			break
		}
		acc = append(acc, d...)
	}
	return acc, nil
}

// Close 释放编码器与 Media Foundation。
func (e *MFH264Encoder) Close() {
	if e.closed {
		return
	}
	e.closed = true
	if e.tr != nil {
		_ = tProcessMessage(e.tr, mftMsgNotifyEndStreaming, 0)
		comRelease(e.tr)
		e.tr = nil
	}
	mfShutdown()
}
