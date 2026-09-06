//go:build windows

package native

import (
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ── 异步 MFT（硬件编码器）事件驱动编码 ──
//
// 权威参考：Chromium media/gpu/windows/media_foundation_video_encode_accelerator_win.cc
// （NVIDIA/Intel/AMD 硬件 H.264 MFT 的标准驱动方式）。本机 "NVIDIA H.264 Encoder MFT"
// 是异步 MFT：任何同步调用（ProcessMessage/ProcessInput）在未解锁前都返回
// MF_E_TRANSFORM_ASYNC_LOCKED(0xC00D6D77)。
//
// 关键：异步 MFT 必须先解锁（在编码器自身的 IMFAttributes 上设
// MF_TRANSFORM_ASYNC_UNLOCK=TRUE），然后事件驱动：
//   METransformNeedInput(601)  → 才能 ProcessInput 喂一帧 NV12
//   METransformHaveOutput(602) → 才能 ProcessOutput 取回一帧编码数据
//   METransformDrainComplete(603) → drain 结束
//
// vtable 槽位（依据 SDK 头文件）：
//   IMFTransform::GetAttributes = 9, GetStreamCount=4, GetStreamIDs=5
//   IMFMediaEventGenerator(extends IUnknown): GetEvent=3, BeginGetEvent=4, EndGetEvent=5
//   IMFMediaEvent(extends IMFAttributes, base=3, 30 methods→至 slot32): GetType=33
// 事件号：METransformNeedInput=601, HaveOutput=602, DrainComplete=603（METransformUnknown=600）

// 异步 MFT 属性 GUID
var (
	// MF_TRANSFORM_ASYNC = {f81a699a-649a-497d-8c73-29f8fed6ad7a}
	guidMFTAsync = mkGUID(0xf81a699a, 0x649a, 0x497d,
		[8]byte{0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a})
	// MF_TRANSFORM_ASYNC_UNLOCK = {e5666d6b-3422-4eb6-a421-da7db1f8e207}
	guidMFTAsyncUnlock = mkGUID(0xe5666d6b, 0x3422, 0x4eb6,
		[8]byte{0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07})
)

// 事件常量（MediaEventType）
const (
	evTransformNeedInput    = 601 // METransformNeedInput
	evTransformHaveOutput   = 602 // METransformHaveOutput
	evTransformDrainComplete = 603
	evError                 = 1 // MEError
	// GetEvent NO_WAIT 时无事件就绪
	mfENoEventsAvailable = 0xc00d3e80
	// MFT_MESSAGE_NOTIFY_START_OF_STREAM = 0x10000003
	mftMsgNotifyStartOfStream = 0x10000003
)

// mfTransformGetAttributes 调 IMFTransform::GetAttributes（槽位 8，rel5）。
func mfTransformGetAttributes(t unsafe.Pointer) (unsafe.Pointer, error) {
	fn := comMethod(t, 8)
	var attrs unsafe.Pointer
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(unsafe.Pointer(&attrs)))
	if r != 0 {
		return nil, fmt.Errorf("IMFTransform::GetAttributes HRESULT=0x%x", r)
	}
	if attrs == nil {
		return nil, fmt.Errorf("GetAttributes 返回空")
	}
	return attrs, nil
}

// mfTransformUnlockAsync 给异步 MFT 解锁（MF_TRANSFORM_ASYNC_UNLOCK=TRUE）。
// 必须在任何 ProcessMessage/流启动之前调用。
func mfTransformUnlockAsync(t unsafe.Pointer) error {
	attrs, err := mfTransformGetAttributes(t)
	if err != nil {
		return err
	}
	defer comRelease(attrs)
	// 确认确实是异步 MFT（可选诊断）
	var async uint32
	_ = attrGetUINT32(attrs, &guidMFTAsync, &async)
	if err := attrSetUINT32(attrs, &guidMFTAsyncUnlock, 1); err != nil {
		return fmt.Errorf("设 MF_TRANSFORM_ASYNC_UNLOCK 失败: %v", err)
	}
	return nil
}

// getStreamIDs 取输入/输出流 ID。GetStreamCount(槽4)/GetStreamIDs(槽5)。
func getStreamIDs(t unsafe.Pointer) (uint32, uint32, error) {
	var inN, outN uint32
	fn := comMethod(t, 4)
	r, _, _ := syscall.SyscallN(fn, uintptr(t), uintptr(unsafe.Pointer(&inN)), uintptr(unsafe.Pointer(&outN)))
	if r != 0 {
		return 0, 0, fmt.Errorf("GetStreamCount HRESULT=0x%x", r)
	}
	if inN < 1 || outN < 1 {
		return 0, 0, fmt.Errorf("流数量不足 in=%d out=%d", inN, outN)
	}
	inIDs := make([]uint32, inN)
	outIDs := make([]uint32, outN)
	fn2 := comMethod(t, 5)
	r2, _, _ := syscall.SyscallN(fn2, uintptr(t),
		uintptr(inN), uintptr(unsafe.Pointer(&inIDs[0])),
		uintptr(outN), uintptr(unsafe.Pointer(&outIDs[0])))
	if r2 == 0 {
		return inIDs[0], outIDs[0], nil
	}
	// E_NOTIMPL = 0x80004001 → 流 ID 为 0/1 推断
	return 0, 0, nil
}

// asyncHWEncoder 一次异步硬件 MFT 编码会话。
type asyncHWEncoder struct {
	tr     unsafe.Pointer // IMFTransform*
	gen    unsafe.Pointer // IMFMediaEventGenerator*（GetEvent=槽3）
	inID   uint32
	outID  uint32
	width  int
	height int
	dev    *D3D11Device  // D3D11 设备（硬件 MFT 输入所需）
	mgr    *mfD3DManager // MF DXGI 设备管理器
}

// attachD3D 创建 D3D11 设备 + MF DXGI manager 并设到 MFT（解锁之后、流启动之前）。
func (e *asyncHWEncoder) attachD3D() error {
	dev, err := NewD3D11Device()
	if err != nil {
		return fmt.Errorf("D3D11 设备: %v", err)
	}
	mgr, err := NewMFDXGIDeviceManager(dev)
	if err != nil {
		dev.Close()
		return fmt.Errorf("MF DXGI manager: %v", err)
	}
	if err := mgr.SetOnTransform(e.tr); err != nil {
		mgr.Close()
		dev.Close()
		return fmt.Errorf("SetD3DManager: %v", err)
	}
	e.dev, e.mgr = dev, mgr
	return nil
}

// activateAsyncEncoder 激活首选（NVIDIA）异步 MFT 并解锁。
func activateAsyncEncoder(width, height int) (*asyncHWEncoder, error) {
	acts, names, err := enumH264Activates()
	if err != nil || len(acts) == 0 {
		return nil, fmt.Errorf("无 H.264 编码器 MFT")
	}
	defer func() {
		for _, a := range acts {
			comRelease(a)
		}
	}()
	idx := 0
	for i, n := range names {
		if containsFoldStr(n, "nvidia") {
			idx = i
			break
		}
	}
	tr, err := activateTransform(acts[idx])
	if err != nil {
		return nil, fmt.Errorf("activate %s: %v", names[idx], err)
	}
	// 解锁异步（关键：否则 ProcessMessage 全返回 ASYNC_LOCKED）
	if err := mfTransformUnlockAsync(tr); err != nil {
		comRelease(tr)
		return nil, fmt.Errorf("[%s] 解锁异步失败: %v", names[idx], err)
	}
	inID, outID, err := getStreamIDs(tr)
	if err != nil {
		comRelease(tr)
		return nil, err
	}
	return &asyncHWEncoder{tr: tr, inID: inID, outID: outID, width: width, height: height}, nil
}

// configureTypes 配置 H.264 输出 + NV12 输入类型。
func (e *asyncHWEncoder) configureTypes() error {
	outMT, err := buildVideoType(0x34363248, uint32(e.width), uint32(e.height), 30, 1, true, 4_000_000)
	if err != nil {
		return err
	}
	err = tSetOutputType(e.tr, e.outID, outMT)
	comRelease(outMT)
	if err != nil {
		// 盲构造失败 → 用编码器可用输出类型补全
		mt2, e2 := pickAvailableOutputType(e.tr, e.outID)
		if e2 != nil {
			return err
		}
		e2 = completeTypeSize(mt2, uint32(e.width), uint32(e.height), 30, 1)
		if e2 != nil {
			comRelease(mt2)
			return e2
		}
		e2 = tSetOutputType(e.tr, e.outID, mt2)
		comRelease(mt2)
		if e2 != nil {
			return e2
		}
	}
	inMT, err := buildVideoType(0x3231564e, uint32(e.width), uint32(e.height), 30, 1, true, 0) // NV12
	if err != nil {
		return err
	}
	err = tSetInputType(e.tr, e.inID, inMT)
	comRelease(inMT)
	if err != nil {
		return err
	}
	return nil
}

// startStreaming 启动异步处理模型：FLUSH → BEGIN_STREAMING → START_OF_STREAM。
func (e *asyncHWEncoder) startStreaming() error {
	for _, msg := range []struct {
		m uint32
		s string
	}{
		{mftMsgCommandFlush, "COMMAND_FLUSH"},
		{mftMsgNotifyBeginStreaming, "NOTIFY_BEGIN_STREAMING"},
		{mftMsgNotifyStartOfStream, "NOTIFY_START_OF_STREAM"},
	} {
		if err := tProcessMessage(e.tr, msg.m, 0); err != nil {
			return fmt.Errorf("%s: %v", msg.s, err)
		}
	}
	return nil
}

// getEventNoWait 尝试取一个事件（NO_WAIT）。返回 (type, 是否有事件, hr)。
func (e *asyncHWEncoder) getEventNoWait() (int, bool, uint32) {
	fn := comMethod(e.gen, 3) // GetEvent
	var ev unsafe.Pointer
	r, _, _ := syscall.SyscallN(fn, uintptr(e.gen), 1 /*MF_EVENT_FLAG_NO_WAIT*/, uintptr(unsafe.Pointer(&ev)))
	if r != 0 {
		if uint32(r) == mfENoEventsAvailable {
			return 0, false, uint32(r)
		}
		return 0, false, uint32(r)
	}
	if ev == nil {
		return 0, false, 0
	}
	defer comRelease(ev)
	// IMFMediaEvent::GetType 槽位 33
	getType := comMethod(ev, 33)
	var typ int32
	rr, _, _ := syscall.SyscallN(getType, uintptr(ev), uintptr(unsafe.Pointer(&typ)))
	if rr != 0 {
		return 0, false, uint32(rr)
	}
	return int(typ), true, 0
}

// collectOutput 取回一帧编码输出，追加到 acc。硬件 MFT 传 pSample=NULL 让其自分配输出样本。
func (e *asyncHWEncoder) collectOutput(acc []byte) ([]byte, error) {
	ob := mftOutputDataBuffer{streamID: e.outID, pSample: nil}
	var status uint32
	fn := comMethod(e.tr, 25) // ProcessOutput
	r, _, _ := syscall.SyscallN(fn, uintptr(e.tr), 0, 1,
		uintptr(unsafe.Pointer(&ob)), uintptr(unsafe.Pointer(&status)))
	if uint32(r) == 0xc00d6d72 { // MF_E_TRANSFORM_NEED_MORE_INPUT
		return acc, nil
	}
	if ob.pEvents != nil {
		comRelease(ob.pEvents)
	}
	if r != 0 {
		return acc, fmt.Errorf("ProcessOutput HRESULT=0x%x", r)
	}
	outSample := ob.pSample
	if outSample == nil {
		return acc, nil
	}
	defer comRelease(outSample)
	var i uint32
	for {
		bb, berr := sampleGetBufferByIndex(outSample, i)
		if berr != nil || bb == nil {
			break
		}
		d, derr := bufferCopyOut(bb)
		if derr == nil && len(d) > 0 {
			acc = append(acc, d...)
		}
		comRelease(bb)
		i++
	}
	return acc, nil
}

// qiEventGenerator 从 transform QI IMFMediaEventGenerator。
func (e *asyncHWEncoder) acquireEventGenerator() error {
	qi := comMethod(e.tr, 0)
	var gen unsafe.Pointer
	r, _, _ := syscall.SyscallN(qi, uintptr(e.tr), uintptr(unsafe.Pointer(&iidMediaEventGenerator)), uintptr(unsafe.Pointer(&gen)))
	if r != 0 || gen == nil {
		return fmt.Errorf("QI IMFMediaEventGenerator HRESULT=0x%x", r)
	}
	e.gen = gen
	return nil
}

func (e *asyncHWEncoder) Close() {
	if e.gen != nil {
		comRelease(e.gen)
		e.gen = nil
	}
	if e.tr != nil {
		comRelease(e.tr)
		e.tr = nil
	}
	if e.mgr != nil {
		e.mgr.Close()
		e.mgr = nil
	}
	if e.dev != nil {
		e.dev.Close()
		e.dev = nil
	}
}

// AsyncHWEncodeSmoke 驱动首选异步硬件 MFT 编码 frames 帧 NV12 为 H.264 Annex B。
// 头less 自验证：不依赖 ffmpeg/cgo/浏览器。frames=0 时默认 3。
func AsyncHWEncodeSmoke(width, height, frames int) (annexB []byte, info string, err error) {
	if width <= 0 || height <= 0 {
		width, height = 320, 240
	}
	if frames <= 0 {
		frames = 3
	}
	enc, err := activateAsyncEncoder(width, height)
	if err != nil {
		return nil, "", err
	}
	defer enc.Close()
	if err := enc.acquireEventGenerator(); err != nil {
		return nil, "", err
	}
	// 设 D3D manager（解锁之后）。失败则继续（部分 MFT 无 D3D 也能收系统内存）。
	if derr := enc.attachD3D(); derr != nil && Verbose {
		fmt.Printf("  [async] D3D 附加跳过: %v\n", derr)
	}
	if err := enc.configureTypes(); err != nil {
		return nil, "", err
	}
	if err := enc.startStreaming(); err != nil {
		return nil, "", err
	}

	// 帧数据：生成 frames 帧 NV12（梯度，避免全灰导致退化的极端编码）
	framesData := make([][]byte, frames)
	for i := range framesData {
		framesData[i] = makeNV12Gradient(width, height, i)
	}

	var acc []byte
	feedIdx := 0  // 下一个待喂帧（NOTACCEPTING 时保持，等下一次 NeedInput 重试）
	draining := false
	deadline := time.Now().Add(20 * time.Second)
	for {
		typ, ok, hr := enc.getEventNoWait()
		if !ok {
			if hr == mfENoEventsAvailable {
				if time.Now().After(deadline) {
					return acc, fmt.Sprintf("超时无事件 fed=%d", feedIdx), fmt.Errorf("事件超时")
				}
				time.Sleep(2 * time.Millisecond)
				continue
			}
			return acc, fmt.Sprintf("GetEvent hr=0x%x", hr), fmt.Errorf("GetEvent 失败 0x%x", hr)
		}
		switch typ {
		case evTransformNeedInput:
			if Verbose {
				fmt.Printf("  [async] ev NeedInput feedIdx=%d\n", feedIdx)
			}
			if !draining && feedIdx < len(framesData) {
				sample, e2 := makeSample()
				if e2 != nil {
					return acc, "", e2
				}
				e2 = putNV12InSample(sample, framesData[feedIdx])
				if e2 != nil {
					comRelease(sample)
					return acc, "", e2
				}
				_ = sampleSetTime(sample, int64(feedIdx)*int64(1e7)/30)
				_ = sampleSetDuration(sample, int64(1e7)/30)
				e2 = tProcessInput(enc.tr, enc.inID, sample, 0)
				comRelease(sample)
				if e2 != nil {
					// NOTACCEPTING 属异步 MFT 正常瞬时态：保持 feedIdx，等下一次 NeedInput 重试
					if !strings.Contains(e2.Error(), "0xc00d6d76") {
						return acc, fmt.Sprintf("喂第%d帧失败", feedIdx), e2
					}
					continue
				}
				feedIdx++
				continue
			}
			// 帧已喂完 → 触发 drain 冲刷
			if !draining {
				draining = true
				if e2 := tProcessMessage(enc.tr, mftMsgCommandDrain, 0); e2 != nil {
					return acc, "drain", e2
				}
			}
		case evTransformHaveOutput:
			before := len(acc)
			acc, err = enc.collectOutput(acc)
			if err != nil {
				return acc, "", err
			}
			if Verbose {
				fmt.Printf("  [async] ev HaveOutput +%d bytes (total %d)\n", len(acc)-before, len(acc))
			}
		case evTransformDrainComplete:
			if Verbose {
				fmt.Printf("  [async] ev DrainComplete total=%d\n", len(acc))
			}
			// 冲刷结束；为稳妥再试取一次输出
			acc, _ = enc.collectOutput(acc)
			goto done
		case evError:
			return acc, "MEError", fmt.Errorf("编码器 MEError")
		default:
			if Verbose {
				fmt.Printf("  [async] ev other type=%d\n", typ)
			}
		}
	}
done:
	if len(acc) == 0 {
		return acc, fmt.Sprintf("encoder=async fed=%d 无输出", feedIdx), fmt.Errorf("无编码输出")
	}
	check, detail := validateAnnexB(acc)
	info = fmt.Sprintf("encoder=async-HW fed=%d bytes=%d %s", feedIdx, len(acc), detail)
	if !check {
		return acc, info, fmt.Errorf("输出非合法 H.264: %s", detail)
	}
	return acc, info, nil
}

// makeNV12Gradient 生成带竖向亮度的 NV12 帧（内容不同帧便于 IDR 校验/解码）。
func makeNV12Gradient(w, h, frame int) []byte {
	buf := make([]byte, w*h*3/2)
	lum := byte(40 + frame*20)
	for y := 0; y < h; y++ {
		base := y * w
		for x := 0; x < w; x++ {
			buf[base+x] = byte((x*255)/w)*2/3 + lum/3
		}
	}
	// U/V 平面设为 128
	for i := w * h; i < w*h*3/2; i++ {
		buf[i] = 128
	}
	return buf
}

// AsyncHWEncodePerf 在同一会话内连续编码 frames 帧（异步流式，无每帧 drain），
// 计时返回平均 fps 与总字节。用于评估硬件编码路径的稳态吞吐。
func AsyncHWEncodePerf(width, height, frames int) (fps float64, bytesOut int, info string, err error) {
	if frames < 2 {
		frames = 60
	}
	enc, err := activateAsyncEncoder(width, height)
	if err != nil {
		return 0, 0, "", err
	}
	defer enc.Close()
	if err := enc.acquireEventGenerator(); err != nil {
		return 0, 0, "", err
	}
	if derr := enc.attachD3D(); derr != nil {
		return 0, 0, "", derr
	}
	if err := enc.configureTypes(); err != nil {
		return 0, 0, "", err
	}
	if err := enc.startStreaming(); err != nil {
		return 0, 0, "", err
	}

	// 预生成帧
	framesData := make([][]byte, frames)
	for i := range framesData {
		framesData[i] = makeNV12Gradient(width, height, i)
	}

	var acc []byte
	feedIdx := 0
	draining := false
	start := time.Now()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		typ, ok, hr := enc.getEventNoWait()
		if !ok {
			if hr == mfENoEventsAvailable {
				if time.Now().After(deadline) {
					return 0, len(acc), fmt.Sprintf("fed=%d 超时", feedIdx), fmt.Errorf("事件超时")
				}
				time.Sleep(500 * time.Microsecond)
				continue
			}
			return 0, len(acc), "", fmt.Errorf("GetEvent hr=0x%x", hr)
		}
		switch typ {
		case evTransformNeedInput:
			if !draining && feedIdx < frames {
				sample, e2 := makeSample()
				if e2 != nil {
					return 0, len(acc), "", e2
				}
				e2 = putNV12InSample(sample, framesData[feedIdx])
				if e2 != nil {
					comRelease(sample)
					return 0, len(acc), "", e2
				}
				_ = sampleSetTime(sample, int64(feedIdx)*int64(1e7)/60)
				_ = sampleSetDuration(sample, int64(1e7)/60)
				e2 = tProcessInput(enc.tr, enc.inID, sample, 0)
				comRelease(sample)
				if e2 != nil {
					if strings.Contains(e2.Error(), "0xc00d6d76") {
						continue // 异步瞬时态，重试
					}
					return 0, len(acc), "", e2
				}
				feedIdx++
				continue
			}
			if !draining {
				draining = true
				if e2 := tProcessMessage(enc.tr, mftMsgCommandDrain, 0); e2 != nil {
					return 0, len(acc), "", e2
				}
			}
		case evTransformHaveOutput:
			acc, err = enc.collectOutput(acc)
			if err != nil {
				return 0, len(acc), "", err
			}
		case evTransformDrainComplete:
			acc, _ = enc.collectOutput(acc)
			goto done
		case evError:
			return 0, len(acc), "", fmt.Errorf("MEError")
		}
	}
done:
	el := time.Since(start).Seconds()
	fps = float64(feedIdx) / el
	info = fmt.Sprintf("%dx%d fed=%d fps=%.1f bytes=%d", width, height, feedIdx, fps, len(acc))
	return fps, len(acc), info, nil
}
