//go:build windows

package native

import (
	"syscall"
	"testing"
	"unsafe"
)

// TestCOMVTableInvoke 隔离验证纯 Go COM vtable 回调对象：
// 通过 vtable[4]=Invoke 槽位直接调用（如同 MF 调用我们的 IMFAsyncCallback），
// 确认能正确路由到注册的 Go 函数且返回 S_OK。这是异步 MFT 回调的基石。
func TestCOMVTableInvoke(t *testing.T) {
	called := false
	cb := makeAsyncCallback(func(resultPtr unsafe.Pointer) uintptr {
		called = true
		return 0 // S_OK
	})
	if cb == nil {
		t.Fatal("callback 为空")
	}
	// 读 vtable
	vtbl := *(*uintptr)(cb)
	fns := unsafe.Slice((*uintptr)(unsafe.Pointer(vtbl)), 5)
	// 模拟调用 Invoke = vtable[4](this, pAsyncResult)
	hr, _, _ := syscall.SyscallN(fns[4], uintptr(cb), 0)
	if hr != 0 {
		t.Fatalf("Invoke 返回 HRESULT=0x%x", hr)
	}
	if !called {
		t.Fatal("Go 回调未被调用")
	}
	t.Logf("✓ 纯 Go COM vtable 回调 Invoke 可用")
}
