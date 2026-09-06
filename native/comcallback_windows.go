//go:build windows

package native

import (
	"syscall"
	"unsafe"
)

// ── 极简纯 Go COM 对象（供异步 MFT 的 IMFAsyncCallback）──
//
// Windows COM 对象 = 内存结构：首字段是指向 vtable 的指针。
// vtable 是函数指针数组；前 3 项固定为 IUnknown 的 QueryInterface/AddRef/Release，
// 之后是接口自身方法。每个方法第一个参数是对象自身指针(this)。
//
// 我们用一个 goCallback 结构，其 objPtr 字段指向一个由本结构 + vtable 构成的
// "外部对象内存"。vtable 通过 syscall.NewCallback 生成的函数指针构成。
//
// 注意：Go 的 syscall.NewCallback 生成 stdcall 函数指针；在 x64 上 stdcall/cdecl
// 调用约定相同，因此可用作 COM vtable 条目。这是纯 Go 实现 COM 回调的途径。

// comVtableEntryCount 当前只需 IMFAsyncCallback：IUnknown(3) + GetParameters + Invoke = 5
const callbackVtblSlots = 5

// goCallback 是对外暴露的 IMFAsyncCallback 对象（以 COM 指针形式返回给 MF）。
type goCallback struct {
	// object 是一片连续内存，布局为 [vtablePtr][ (this指向本 goCallback 的字段区) ]。
	// 我们把 goCallback 自身作为 this；object 指向的 vtable 我们单独存。
	cbPtr  unsafe.Pointer // 指向 goCallback 自身，作为 COM this 传给方法
	invoke func(p unsafe.Pointer) uintptr // 收到 IMFAsyncResult* 时的处理函数
}

// callbackCOMObj 实际的内存布局：我们用一个数组承载 vtable 指针 + 保证存活。
// vtable 需要长期存活，不能是栈上临时量。

var (
	// callbackVTables 持有所有已创建 goCallback 的 vtable 内存（防 GC/栈回收）。
	// 每个 vtable = callbackVtblSlots 个 uintptr。
	callbackVTables [][]uintptr
	// newCallbackLimit: syscall.NewCallback 数量有限(2000)，我们用少量复用。
)

// goCallbackQueryInterface / AddRef / Release / GetParameters / Invoke 的 C 函数。
// 这些通过 syscall.NewCallback 注册。签名需符合 COM：返回 HRESULT/ULONG，收 this。

// 为了把 this 映射回 goCallback，我们约定：this 就是 goCallback 指针本身。
// 因此新建对象时把 cbPtr 设为一个指向 goCallback 的指针，并从 vtable 方法里经
// 固定全局注册表反查。

// 每个 COM 方法函数（在 C 层）都会收到 (this, ...args)。我们把 this 在全局 map 登记。

type cbRegistryEntry struct {
	invokeFn func(resultPtr unsafe.Pointer) uintptr
	refs     int
}

var (
	cbRegistry     = map[unsafe.Pointer]*cbRegistryEntry{}
	cbRegistryLock = make(chan struct{}, 1)
)

func cbLock()   { cbRegistryLock <- struct{}{} }
func cbUnlock() { <-cbRegistryLock }

// ---- C 层导出函数（经 NewCallback）----

// goCallback_AddRef(this) ULONG
func goCallback_AddRef(this uintptr) uintptr {
	cbLock()
	if e, ok := cbRegistry[unsafe.Pointer(this)]; ok {
		e.refs++
		cbUnlock()
		return uintptr(e.refs)
	}
	cbUnlock()
	return 1
}

// goCallback_Release(this) ULONG
func goCallback_Release(this uintptr) uintptr {
	cbLock()
	e, ok := cbRegistry[unsafe.Pointer(this)]
	if !ok {
		cbUnlock()
		return 0
	}
	e.refs--
	r := e.refs
	cbUnlock()
	return uintptr(r)
}

// goCallback_GetParameters(this, &flags, &queue) HRESULT
func goCallback_GetParameters(this, pdwFlags, pdwQueue uintptr) uintptr {
	return 0 // S_OK; 不指定 flags/queue
}

// goCallback_Invoke(this, pAsyncResult) HRESULT
func goCallback_Invoke(this, pAsyncResult uintptr) uintptr {
	cbLock()
	e, ok := cbRegistry[unsafe.Pointer(this)]
	cbUnlock()
	if !ok || e.invokeFn == nil {
		return 0x80070057 // E_INVALIDARG
	}
	return e.invokeFn(unsafe.Pointer(pAsyncResult))
}

// QueryInterface：为简化，仅支持请求 IID_IMFAsyncCallback（等同 IUnknown 返回自身）。
func goCallback_QueryInterface(this, riid, ppv uintptr) uintptr {
	*(*uintptr)(unsafe.Pointer(ppv)) = this
	return 0
}

// goCallbackCOMObj 在内存中构造一个 IMFAsyncCallback COM 对象，返回其接口指针。
// 仅支持一个回调。invokeFn 在 Invoke 被调用时执行。
func newGoCallbackCOM(invokeFn func(resultPtr unsafe.Pointer) uintptr) unsafe.Pointer {
	cbLock()
	defer cbUnlock()

	// 生成 5 个函数指针
	qif := syscall.NewCallback(goCallback_QueryInterface)
	add := syscall.NewCallback(goCallback_AddRef)
	rel := syscall.NewCallback(goCallback_Release)
	gp := syscall.NewCallback(goCallback_GetParameters)
	inv := syscall.NewCallback(goCallback_Invoke)

	vtbl := make([]uintptr, callbackVtblSlots)
	vtbl[0], vtbl[1], vtbl[2] = qif, add, rel
	vtbl[3], vtbl[4] = gp, inv
	callbackVTables = append(callbackVTables, vtbl)

	// 对象内存 = [vtablePtr uintptr] 放在一个分配的结构里
	obj := struct {
		vtblPtr uintptr
	}{vtblPtr: uintptr(unsafe.Pointer(&vtbl[0]))}

	// 由于 obj 在栈上，不能返回其指针——需要堆分配。
	buf := make([]uintptr, callbackVtblSlots+1)
	buf[0] = uintptr(unsafe.Pointer(&vtbl[0])) // vtablePtr
	objPtr := unsafe.Pointer(&buf[1])          // 实际对象起始(自定)，此处我们让 this=buf[0]处
	// this 应为 &buf[0]（对象首地址=vtablePtr 字段）
	thisPtr := unsafe.Pointer(&buf[0])
	_ = objPtr
	_ = obj

	cbRegistry[thisPtr] = &cbRegistryEntry{invokeFn: invokeFn, refs: 1}
	// 防止 buf 被 GC：登记到全局
	callbackObjects = append(callbackObjects, buf)
	return thisPtr
}

var callbackObjects [][]uintptr

// makeAsyncCallback 便捷入口：返回一个 IMFAsyncCallback*，回调触发时调用 fn(resultPtr)。
func makeAsyncCallback(fn func(resultPtr unsafe.Pointer) uintptr) unsafe.Pointer {
	return newGoCallbackCOM(fn)
}
