//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── 头less 单帧编码冒烟入口 ──

// EncodeSingleFrameSmoke 枚举硬件 H.264 编码器 MFT，逐个尝试把一帧合成 NV12
// 编码为 H.264，返回首个成功编码器产出的 Annex B。若全失败返回聚合 error。
// 不依赖 ffmpeg / cgo / 浏览器，是阶段一的头less 降级验证。
//
// 说明：部分硬件编码器（如 NVIDIA H.264 Encoder MFT）按异步 MFT 运行，
// 需要事件驱动模型或 D3D11，可能拒绝最简同步流程（MF_E_TRANSFORM_ASYNC_LOCKED）。
// 因此这里按"可用性"依次尝试每个枚举到的编码器，取第一个走通同步路径的，
// 兼顾"同步软件编码器"与"接受同步流程的硬件编码器"。
func EncodeSingleFrameSmoke(width, height int) (annexB []byte, info string, err error) {
	if width <= 0 || height <= 0 {
		width, height = 320, 240
	}
	if !mfStartup() {
		return nil, "", fmt.Errorf("MFStartup 失败")
	}
	defer mfShutdown()

	acts, names, err := enumH264Activates()
	if err != nil {
		return nil, "", err
	}
	defer func() {
		for _, a := range acts {
			comRelease(a)
		}
	}()
	if len(acts) == 0 {
		return nil, "", fmt.Errorf("无可用 H.264 编码器 MFT")
	}

	var lastErr error
	// 逐个尝试；不同编码器对"先设输入还是先设输出"要求不同，两者都试。
	for i := range acts {
		for _, inFirst := range []bool{true, false} {
			out, nfo, e := encodeOneFrame(acts[i], names[i], width, height, inFirst)
			if e == nil {
				return out, nfo, nil
			}
			if Verbose {
				order := "outFirst"
				if inFirst {
					order = "inFirst"
				}
				fmt.Printf("  [%s / %s] 失败: %v\n", names[i], order, e)
			}
			lastErr = e
		}
	}
	return nil, "", fmt.Errorf("全部 %d 个编码器均失败，最后错误: %v", len(acts), lastErr)
}

// encodeOneFrame 使用单个 IMFActivate 完成一次完整编码（不释放 activate，由调用方负责）。
// inFirst 为 true 时先 SetInputType(NV12) 再 SetOutputType(H264)；否则相反。
func encodeOneFrame(activate unsafe.Pointer, name string, width, height int, inFirst bool) ([]byte, string, error) {
	tr, err := activateTransform(activate)
	if err != nil {
		return nil, "", err
	}
	defer comRelease(tr)

	buildOut := func() (unsafe.Pointer, error) {
		// 软件编码器 SetOutputType 需要 AVG_BITRATE + 渐进扫描。
		return buildVideoType(0x34363248, uint32(width), uint32(height), 60, 1, true, 2_000_000) // 'H264' 2 Mbps
	}
	buildIn := func() (unsafe.Pointer, error) {
		return buildVideoType(0x3231564e, uint32(width), uint32(height), 60, 1, true, 0) // 'NV12'
	}
	dumpAvail := func(label string) {
		if !Verbose {
			return
		}
		for i := uint32(0); i < 6; i++ {
			mt, err := tGetOutputAvailableType(tr, 0, i)
			if err != nil {
				break
			}
			if sub, serr := subtypeOf(mt); serr == nil {
				cc := uint32(sub[0]) | uint32(sub[1])<<8 | uint32(sub[2])<<16 | uint32(sub[3])<<24
				fmt.Printf("    [%s] avail out type[%d] subtype=%08x\n", label, i, cc)
			}
			comRelease(mt)
		}
	}
	setOut := func() error {
		// 先试盲构造完整输出类型（文档范式：MajorType+Subtype+FrameSize+FrameRate+Bitrate+Interlace）。
		outMT, err := buildOut()
		if err == nil {
			e := tSetOutputType(tr, 0, outMT)
			comRelease(outMT)
			if e == nil {
				return nil
			}
			if Verbose {
				fmt.Printf("    [%s] 盲构造 SetOutputType: %v（试可用输出类型）\n", name, e)
			}
		}
		// 盲构造失败 → 用编码器可用输出类型补全后重试。
		outMT, err = pickAvailableOutputType(tr, 0)
		if err != nil {
			return err
		}
		if e2 := completeTypeSize(outMT, uint32(width), uint32(height), 60, 1); e2 != nil {
			comRelease(outMT)
			return e2
		}
		defer comRelease(outMT)
		return tSetOutputType(tr, 0, outMT)
	}
	setIn := func() error {
		inMT, err := buildIn()
		if err != nil {
			return err
		}
		defer comRelease(inMT)
		return tSetInputType(tr, 0, inMT)
	}

	// 诊断：打印编码器默认可用输出类型（了解它到底接受什么）。
	dumpAvail(name)

	if inFirst {
		if err := setIn(); err != nil {
			return nil, "", err
		}
		if err := setOut(); err != nil {
			return nil, "", err
		}
	} else {
		if err := setOut(); err != nil {
			return nil, "", err
		}
		if err := setIn(); err != nil {
			return nil, "", err
		}
	}

	if err := tProcessMessage(tr, mftMsgNotifyBeginStreaming, 0); err != nil {
		return nil, "", err
	}

	sample, err := makeSample()
	if err != nil {
		return nil, "", err
	}
	defer comRelease(sample)
	if err := putNV12InSample(sample, makeNV12(width, height)); err != nil {
		return nil, "", err
	}
	// 设置时间戳（单位 100ns）：60fps → 时长 ~166667；编码器拒绝无时间戳的样本。
	_ = sampleSetTime(sample, 0)
	_ = sampleSetDuration(sample, int64(1e7)/60)

	if err := tProcessInput(tr, 0, sample, 0); err != nil {
		return nil, "", err
	}
	if err := tProcessMessage(tr, mftMsgCommandDrain, 0); err != nil {
		return nil, "", err
	}

	out, err := drainOutput(tr)
	if err != nil {
		return nil, "", err
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("编码器未产出任何数据")
	}
	check, detail := validateAnnexB(out)
	info := fmt.Sprintf("encoder=%s frame=%dx%d bytes=%d %s", name, width, height, len(out), detail)
	if !check {
		return nil, "", fmt.Errorf("输出非合法 H.264: %s", detail)
	}
	return out, info, nil
}

// pickAvailableOutputType 遍历编码器可用输出类型，返回首个 subtype 为 H264 的类型。
func pickAvailableOutputType(t unsafe.Pointer, streamID uint32) (unsafe.Pointer, error) {
	h264 := videoSubtype(0x34363248) // 'H264'
	var lastErr error
	for idx := uint32(0); idx < 16; idx++ {
		mt, err := tGetOutputAvailableType(t, streamID, idx)
		if err != nil {
			lastErr = err
			break // 越界（通常 MF_E_NO_MORE_TYPES）
		}
		sub, serr := subtypeOf(mt)
		if serr == nil && sub == h264 {
			return mt, nil // 交由调用方 SetOutputType + 释放
		}
		comRelease(mt)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无可用输出类型")
	}
	return nil, fmt.Errorf("未找到 H.264 可用输出类型: %v", lastErr)
}

// completeTypeSize 往已有媒体类型补写 MF_MT_FRAME_SIZE / MF_MT_FRAME_RATE（可用输出类型常缺）。
func completeTypeSize(mt unsafe.Pointer, width, height uint32, num, den uint32) error {
	if err := attrSetUINT64(mt, &guidMTFrameSize, uint64(width)<<32|uint64(height)); err != nil {
		return err
	}
	return attrSetUINT64(mt, &guidMTFrameRate, uint64(num)<<32|uint64(den))
}

// enumH264Activates 枚举硬件 H.264 视频编码器的 IMFActivate 数组。
func enumH264Activates() ([]unsafe.Pointer, []string, error) {
	if !mfStartup() {
		return nil, nil, fmt.Errorf("MFStartup 失败")
	}
	defer mfShutdown()

	var pArr unsafe.Pointer
	var count uint32
	outType := mftRegisterTypeInfo{majorType: guidMediaTypeVideo, subtype: guidVideoH264}
	r, _, _ := procMFTEnumEx.Call(
		uintptr(unsafe.Pointer(&guidVideoEncoder)),
		uintptr(mftEnumSyncMFT|mftEnumHardware|mftEnumLocalMFT),
		0,
		uintptr(unsafe.Pointer(&outType)),
		uintptr(unsafe.Pointer(&pArr)),
		uintptr(unsafe.Pointer(&count)))
	if r != 0 {
		return nil, nil, fmt.Errorf("MFTEnumEx HRESULT=0x%x", r)
	}
	if count == 0 || pArr == nil {
		return nil, nil, nil
	}
	acts := unsafe.Slice((*unsafe.Pointer)(pArr), count)
	outA := make([]unsafe.Pointer, 0, count)
	outN := make([]string, 0, count)
	for _, a := range acts {
		if a == nil {
			continue
		}
		name := "encoder"
		if s, ok := comGetString(a, &guidFriendlyName); ok {
			name = s
		}
		outA = append(outA, a)
		outN = append(outN, name)
	}
	return outA, outN, nil
}

// putNV12InSample 把 NV12 帧数据写入 sample（新建 buffer → copy → SetCurrentLength → AddBuffer）。
func putNV12InSample(s unsafe.Pointer, data []byte) error {
	b, err := makeBuffer(len(data))
	if err != nil {
		return err
	}
	defer comRelease(b)

	// Lock 槽位 3
	fnLock := comMethod(b, 3)
	var ppb unsafe.Pointer
	var maxLen, got uint32
	r, _, _ := syscall.SyscallN(fnLock, uintptr(b), uintptr(unsafe.Pointer(&ppb)),
		uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&got)))
	if r != 0 {
		return fmt.Errorf("buffer Lock HRESULT=0x%x", r)
	}
	if int(maxLen) < len(data) {
		return fmt.Errorf("buffer 过小: max=%d need=%d", maxLen, len(data))
	}
	dst := unsafe.Slice((*byte)(ppb), len(data))
	copy(dst, data)
	fnUnlock := comMethod(b, 4)
	_, _, _ = syscall.SyscallN(fnUnlock, uintptr(b))

	if err := bufferSetCurrentLength(b, len(data)); err != nil {
		return err
	}
	return sampleAddBuffer(s, b)
}

// drainOutput 在 drain 后循环 ProcessOutput，把所有样本数据拼为 Annex B。
func drainOutput(t unsafe.Pointer) ([]byte, error) {
	var acc []byte
	for {
		// 每轮新建输出 sample + 空 buffer（由编码器写入）
		outSample, err := makeSample()
		if err != nil {
			return nil, err
		}
		buf, err := makeBuffer(1 << 20)
		if err != nil {
			comRelease(outSample)
			return nil, err
		}
		if err := sampleAddBuffer(outSample, buf); err != nil {
			comRelease(outSample)
			return nil, err
		}
		ob := mftOutputDataBuffer{streamID: 0, pSample: outSample}
		s, err := tProcessOutput(t, &ob)
		if err != nil {
			comRelease(outSample)
			return nil, err
		}
		if s == nil {
			comRelease(outSample)
			break
		}
		// 取回该样本全部 buffer
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
		comRelease(outSample)
	}
	return acc, nil
}

// validateAnnexB 检查输出是否为合法 H.264 Annex B（含起始码且含 IDR 关键帧）。
func validateAnnexB(data []byte) (bool, string) {
	if len(data) == 0 {
		return false, "空输出"
	}
	idr, sps, pps := false, false, false
	for i := 0; i+3 < len(data); {
		nalType := byte(255)
		step := 1
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				nalType = data[i+3] & 0x1f
				step = 4
			} else if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				nalType = data[i+4] & 0x1f
				step = 5
			}
		}
		switch nalType {
		case 5:
			idr = true
		case 7:
			sps = true
		case 8:
			pps = true
		}
		if idr && sps && pps {
			return true, "合法: 含 SPS/PPS/IDR"
		}
		i += step
	}
	return idr && sps, fmt.Sprintf("IDR=%v SPS=%v PPS=%v", idr, sps, pps)
}

// makeNV12 生成宽*高*3/2 的 NV12 帧。Y=128 灰, 色度交错。
func makeNV12(w, h int) []byte {
	buf := make([]byte, w*h*3/2)
	for i := 0; i < w*h; i++ {
		buf[i] = 128
	}
	return buf
}
