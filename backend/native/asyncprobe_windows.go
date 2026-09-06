//go:build windows

package native

import (
	"fmt"
	"syscall"
	"unsafe"
)

// IID_IMFMediaEventGenerator = {2CD0BD52-BCD5-4B89-B62C-EADC0C031E7D}
var iidMediaEventGenerator = mkGUID(0x2CD0BD52, 0xBCD5, 0x4B89,
	[8]byte{0xB6, 0x2C, 0xEA, 0xDC, 0x0C, 0x03, 0x1E, 0x7D})

// probeAsyncEventGenerator 激活 NVIDIA MFT 并 QI IMFMediaEventGenerator，探测异步事件能力。
func probeAsyncEventGenerator() string {
	acts, names, err := enumH264Activates()
	if err != nil || len(acts) == 0 {
		return "无 MFT"
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
		return fmt.Sprintf("activate %s: %v", names[idx], err)
	}
	defer comRelease(tr)

	// QI for IMFMediaEventGenerator
	qi := comMethod(tr, 0)
	var gen unsafe.Pointer
	r, _, _ := syscall.SyscallN(qi, uintptr(tr), uintptr(unsafe.Pointer(&iidMediaEventGenerator)), uintptr(unsafe.Pointer(&gen)))
	if r != 0 {
		return fmt.Sprintf("[%s] QI IMFMediaEventGenerator HRESULT=0x%x（该 MFT 可能不直接暴露事件生成器）", names[idx], r)
	}
	defer comRelease(gen)
	// 尝试 GetEvent 非阻塞：flags=MF_EVENT_FLAG_NO_WAIT(0x1) 立即返回
	// 若无事件就绪返回 MF_E_NO_EVENTS_AVAILABLE(0xc00d3e80)——这证明事件生成器存在。
	getEvent := comMethod(gen, 3) // IMFMediaEventGenerator::GetEvent 槽位 3 (base=3 + rel0)
	var ev unsafe.Pointer
	r2, _, _ := syscall.SyscallN(getEvent, uintptr(gen), 1 /*NO_WAIT*/, uintptr(unsafe.Pointer(&ev)))
	if r2 == 0 && ev != nil {
		comRelease(ev)
		return fmt.Sprintf("[%s] 暴露事件生成器；当前有事件就绪", names[idx])
	}
	return fmt.Sprintf("[%s] 暴露事件生成器, GetEvent(NO_WAIT)=0x%x (0xc00d3e80=无事件就绪,属正常)", names[idx], r2)
}
