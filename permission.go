package main

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// ═══════════════════════════ 权限状态（仅内存） ═══════════════════════════

var (
	alwaysAllow     = make(map[string]bool)
	permanentlyDeny = make(map[string]bool)
	permMu          sync.RWMutex
)

// ═══════════════════════════ Win32 API 绑定 ═══════════════════════════

var (
	_u32 = syscall.NewLazyDLL("user32.dll")
	_k32 = syscall.NewLazyDLL("kernel32.dll")
	_g32 = syscall.NewLazyDLL("gdi32.dll")

	_cwEx          = _u32.NewProc("CreateWindowExW")
	_regCls        = _u32.NewProc("RegisterClassExW")
	_defWnd        = _u32.NewProc("DefWindowProcW")
	_dstWnd        = _u32.NewProc("DestroyWindow")
	_postQuit      = _u32.NewProc("PostQuitMessage")
	_getMsg        = _u32.NewProc("GetMessageW")
	_dispMsg       = _u32.NewProc("DispatchMessageW")
	_loadCur       = _u32.NewProc("LoadCursorW")
	_setFg         = _u32.NewProc("SetForegroundWindow")
	_sysInfo       = _u32.NewProc("SystemParametersInfoW")
	_sendMsg       = _u32.NewProc("SendMessageW")
	_loadIco       = _u32.NewProc("LoadIconW")
	_getClientRect = _u32.NewProc("GetClientRect")

	_creatFont = _g32.NewProc("CreateFontW")
	_createBrs = _g32.NewProc("CreateSolidBrush")
	_setBkMode = _g32.NewProc("SetBkMode")
	_setTxtCol = _g32.NewProc("SetTextColor")
	_fillRect  = _u32.NewProc("FillRect")
)

// ═══════════════════════════ 常量 ═══════════════════════════

const (
	WS_POPUP            = 0x80000000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_TABSTOP          = 0x00010000
	WS_EX_TOPMOST       = 0x00000008
	WS_EX_CONTROLPARENT = 0x00010000

	BS_PUSHBUTTON    = 0x00000000
	BS_DEFPUSHBUTTON = 0x00000001
	BS_AUTOCHECKBOX  = 3
	SS_LEFT          = 0x00000000
	SS_ICON          = 3

	WM_DESTROY        = 0x0002
	WM_COMMAND        = 0x0111
	WM_SETFONT        = 0x0030
	WM_CTLCOLORSTATIC = 0x0138
	WM_NCHITTEST      = 0x0084
	WM_ERASEBKGND     = 0x0014
	WM_CLOSE          = 0x0010
	HTCAPTION         = 2
	TRANSPARENT       = 1
	IDI_INFORMATION   = 32516
	STM_SETICON       = 0x0170
	BM_GETCHECK       = 0x00F0

	BTN_ALLOW      = 100
	BTN_DENY       = 102
	BTN_DISCONNECT = 200
	CHK_REMEMBER   = 300

	// 深色主题
	_bgR, _bgG, _bgB = 0x20, 0x20, 0x20
	_txR, _txG, _txB = 0xE0, 0xE0, 0xE0
)

// ═══════════════════════════ 对话框全局状态 ═══════════════════════════

var (
	_permAtom  uint16
	_permMu    sync.Mutex
	_permFBig  uintptr
	_permFMid  uintptr
	_permFSml  uintptr
	_permIco   uintptr
	_permBgBr  uintptr
	_permTxClr = uintptr(uint32(_txR) | uint32(_txG)<<8 | uint32(_txB)<<16)

	// 与 permWndProc 通信
	_permResultBtnID    int
	_permResultRemember bool
	_permResultReady    bool
	_permChkHwnd        uintptr
	_permShowCheckbox   bool // 是否显示复选框
)

// 跨线程关闭支持
var (
	activeDlgHwnd uintptr
	activeDlgUser string
	activeDlgMu   sync.Mutex
)

// ═══════════════════════════ 辅助函数 ═══════════════════════════

func _u16(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}
func _inst() uintptr {
	h, _, _ := _k32.NewProc("GetModuleHandleW").Call(0)
	return h
}
func _rgb(r, g, b uint8) uintptr {
	return uintptr(uint32(r) | uint32(g)<<8 | uint32(b)<<16)
}

// ═══════════════════════════ GDI 初始化 ═══════════════════════════

func _permGdiInit() {
	if _permAtom != 0 {
		return // 已初始化
	}
	_permFBig, _, _ = _creatFont.Call(24, 0, 0, 0, 700, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(_u16("Microsoft YaHei UI"))))
	_permFMid, _, _ = _creatFont.Call(20, 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(_u16("Microsoft YaHei UI"))))
	_permFSml, _, _ = _creatFont.Call(18, 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(_u16("Microsoft YaHei UI"))))
	_permIco, _, _ = _loadIco.Call(0, uintptr(IDI_INFORMATION))
	_permBgBr, _, _ = _createBrs.Call(_rgb(_bgR, _bgG, _bgB))
}

// ═══════════════════════════ 窗口过程 ═══════════════════════════

func _permReadCheck() {
	ck, _, _ := _sendMsg.Call(_permChkHwnd, BM_GETCHECK, 0, 0)
	_permResultRemember = ck == 1
}

func permWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case WM_NCHITTEST:
		r, _, _ := _defWnd.Call(hwnd, msg, wp, lp)
		if r == 1 {
			return HTCAPTION
		}
		return r

	case WM_ERASEBKGND:
		// 用深色画刷填充整个客户区
		var rc struct{ L, T, R, B int32 }
		_, _, _ = _getClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		_, _, _ = _fillRect.Call(wp, uintptr(unsafe.Pointer(&rc)), _permBgBr)
		return 1

	case WM_CTLCOLORSTATIC:
		_, _, _ = _setBkMode.Call(wp, TRANSPARENT)
		_, _, _ = _setTxtCol.Call(wp, _permTxClr)
		return _permBgBr

	case WM_COMMAND:
		id := int(uint64(wp) & 0xFFFF)
		if id == CHK_REMEMBER {
			return 0
		}
		if _permShowCheckbox {
			_permReadCheck()
		}
		_permResultBtnID = id
		_permResultReady = true
		// ★ 先销毁窗口再退出消息循环，与 WM_CLOSE 路径一致。
		// 不能只调 PostQuitMessage —— 那样窗口不会被销毁，会残留在屏幕上。
		_, _, _ = _dstWnd.Call(hwnd) // DestroyWindow → WM_DESTROY → PostQuitMessage
		return 0

	case WM_CLOSE:
		if _permShowCheckbox {
			_permReadCheck()
		}
		_permResultReady = true
		_, _, _ = _dstWnd.Call(hwnd)
		return 0

	case WM_DESTROY:
		_, _, _ = _postQuit.Call(0)
		return 0
	}
	r, _, _ := _defWnd.Call(hwnd, msg, wp, lp)
	return r
}

// ═══════════════════════════ 窗口类注册 ═══════════════════════════

func _permRegClass() error {
	if _permAtom != 0 {
		return nil
	}
	_permGdiInit()

	cur, _, _ := _loadCur.Call(0, uintptr(32512)) // IDC_ARROW
	type wcex struct {
		cbSize        uint32
		style         uint32
		lpfnWndProc   uintptr
		cbClsExtra    int32
		cbWndExtra    int32
		hInstance     uintptr
		hIcon         uintptr
		hCursor       uintptr
		hbrBackground uintptr
		lpszMenuName  *uint16
		lpszClassName *uint16
		hIconSm       uintptr
	}
	wc := wcex{
		cbSize:        uint32(unsafe.Sizeof(wcex{})),
		lpfnWndProc:   syscall.NewCallback(permWndProc),
		hInstance:     _inst(),
		hCursor:       cur,
		hbrBackground: _permBgBr,
		lpszClassName: _u16("RDPPermDarkV1"),
		hIcon:         _permIco,
		hIconSm:       _permIco,
	}
	a, _, _ := _regCls.Call(uintptr(unsafe.Pointer(&wc)))
	if a == 0 {
		return fmt.Errorf("RegisterClassExW failed")
	}
	_permAtom = uint16(a)
	return nil
}

// ═══════════════════════════ 控件创建 ═══════════════════════════

func _permCtl(par uintptr, cls, txt string, st uintptr, id, x, y, w, h int) uintptr {
	t := uintptr(0)
	if txt != "" {
		t = uintptr(unsafe.Pointer(_u16(txt)))
	}
	hw, _, _ := _cwEx.Call(0, uintptr(unsafe.Pointer(_u16(cls))), t,
		st|WS_CHILD|WS_VISIBLE, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		par, uintptr(id), _inst(), 0)
	return hw
}

// ═══════════════════════════ 深色弹窗（通用） ═══════════════════════════

type permBtn struct {
	id        int
	text      string
	isDefault bool
}

// runDarkDialog 创建深色主题弹窗并运行消息循环。
// 风格与 dlgcheck.go 一致：无系统标题栏，深色背景，WM_NCHITTEST 拖拽。
// onCreated 在窗口创建后、消息循环开始前调用。
func runDarkDialog(header, body string, buttons []permBtn,
	showCheckbox bool, onCreated func(hwnd uintptr), dlgW, dlgH int) (btnID int, remember bool) {

	// ★ 锁定 OS 线程
	runtime.LockOSThread()

	_permMu.Lock()
	defer _permMu.Unlock()

	if err := _permRegClass(); err != nil {
		return -1, false
	}

	_permResultBtnID = 0
	_permResultRemember = false
	_permResultReady = false
	_permShowCheckbox = showCheckbox

	var wa struct{ L, T, R, B int32 }
	_, _, _ = _sysInfo.Call(0x0030, 0, uintptr(unsafe.Pointer(&wa)), 0)
	x := int((wa.R - wa.L - int32(dlgW)) / 2)
	y := int((wa.B - wa.T - int32(dlgH)) / 2)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	// 无 WS_CAPTION —— 和 dlgcheck.go 一样，完全自绘深色背景
	hwnd, _, _ := _cwEx.Call(
		uintptr(WS_EX_TOPMOST|WS_EX_CONTROLPARENT),
		uintptr(unsafe.Pointer(_u16("RDPPermDarkV1"))),
		0, // 无窗口标题
		uintptr(WS_POPUP|WS_VISIBLE),
		uintptr(x), uintptr(y), uintptr(dlgW), uintptr(dlgH),
		0, 0, _inst(), 0,
	)
	if hwnd == 0 {
		return -1, false
	}

	if onCreated != nil {
		onCreated(hwnd)
	}

	// 图标 (同 dlgcheck.go)
	ic := _permCtl(hwnd, "STATIC", "", SS_ICON, 0, 48, 32, 28, 28)
	_, _, _ = _sendMsg.Call(ic, STM_SETICON, _permIco, 0)

	// 标题 — 大字体
	h1 := _permCtl(hwnd, "STATIC", header, SS_LEFT, 0, 108, 26, dlgW-118, 36)
	_, _, _ = _sendMsg.Call(h1, WM_SETFONT, _permFBig, 1)

	// 正文 — 中等字体
	h2 := _permCtl(hwnd, "STATIC", body, SS_LEFT, 0, 108, 68, dlgW-118, 20)
	_, _, _ = _sendMsg.Call(h2, WM_SETFONT, _permFMid, 1)

	// 复选框（可选）
	if showCheckbox {
		_permChkHwnd = _permCtl(hwnd, "BUTTON", "记住我的选择", WS_TABSTOP|BS_AUTOCHECKBOX, CHK_REMEMBER, 108, 108, 220, 26)
		_, _, _ = _sendMsg.Call(_permChkHwnd, WM_SETFONT, _permFSml, 1)
	}

	// 按钮 — 水平居中
	btnW, btnH, gap := 120, 38, 16
	n := len(buttons)
	totalW := (btnW+gap)*n - gap
	startX := (dlgW - totalW) / 2
	btnY := dlgH - 58
	for i, b := range buttons {
		st := uintptr(WS_TABSTOP)
		if b.isDefault {
			st |= BS_DEFPUSHBUTTON
		} else {
			st |= BS_PUSHBUTTON
		}
		bx := startX + i*(btnW+gap)
		bh := _permCtl(hwnd, "BUTTON", b.text, st, b.id, bx, btnY, btnW, btnH)
		_, _, _ = _sendMsg.Call(bh, WM_SETFONT, _permFSml, 1)
	}

	_, _, _ = _setFg.Call(hwnd)

	// ── 消息循环（不调用 TranslateMessage）──
	var msg [7]uintptr
	for {
		has, _, _ := _getMsg.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if has == 0 {
			break
		}
		_, _, _ = _dispMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
	}

	// ★ 解锁 OS 线程：消息循环已结束，不再需要固定线程。
	// 必须在 return 前调用，否则 goroutine 退出时会直接终止 OS 线程，
	// 导致线程池频繁创建/销毁，可能触发下一个弹窗的消息循环卡死。
	runtime.UnlockOSThread()

	if _permResultReady {
		return _permResultBtnID, _permResultRemember
	}
	return -1, false
}

// ═══════════════════════════ 权限管理 ═══════════════════════════

func checkPermission(user string) (allowed, denied bool) {
	permMu.RLock()
	defer permMu.RUnlock()
	if permanentlyDeny[user] {
		return false, true
	}
	if alwaysAllow[user] {
		return true, false
	}
	return false, false
}

func rememberChoice(user string, always, permanent bool) {
	permMu.Lock()
	defer permMu.Unlock()
	if always {
		alwaysAllow[user] = true
	}
	if permanent {
		permanentlyDeny[user] = true
	}
}

func hasControl(user string) bool {
	controlMu.Lock()
	defer controlMu.Unlock()
	return controlOwner != "" && controlOwner == user
}

// ═══════════════════════════ 跨线程关闭活动对话框 ═══════════════════════════

func closeActiveDialog() {
	activeDlgMu.Lock()
	hwnd := activeDlgHwnd
	activeDlgHwnd = 0
	activeDlgUser = ""
	activeDlgMu.Unlock()

	if hwnd != 0 {
		// ★ 发送 WM_CLOSE 而非 PostThreadMessage(WM_QUIT)。
		// WM_QUIT 只退出消息循环但不销毁窗口 —— 窗口会变成孤儿窗口卡在屏幕上。
		// WM_CLOSE 会触发 DestroyWindow → WM_DESTROY → PostQuitMessage，
		// 既销毁窗口又退出消息循环。
		_, _, _ = _sendMsg.Call(hwnd, WM_CLOSE, 0, 0)
	}
}

// ═══════════════════════════ 对话框实现 ═══════════════════════════

func showControlRequestDialog(userName string) (int, bool) {
	buttons := []permBtn{
		{BTN_ALLOW, "允许", true},
		{BTN_DENY, "拒绝", false},
	}

	header := fmt.Sprintf("用户「%s」请求远程控制权限", userName)
	body := "请选择允许或拒绝此请求。\n勾选【记住我的选择】可将本次选择设为永久规则。"

	return runDarkDialog(header, body, buttons, true, nil, 480, 220)
}

func showActiveControlDialog(userName string) {
	activeDlgMu.Lock()
	activeDlgUser = userName
	activeDlgMu.Unlock()

	defer func() {
		activeDlgMu.Lock()
		if activeDlgUser == userName {
			activeDlgUser = ""
			activeDlgHwnd = 0
		}
		activeDlgMu.Unlock()
	}()

	buttons := []permBtn{
		{BTN_DISCONNECT, "断开控制", true},
	}

	header := fmt.Sprintf("用户「%s」正在控制此电脑", userName)
	body := "点击「断开控制」可立即终止此用户的控制权限。"

	btn, _ := runDarkDialog(header, body, buttons, false,
		func(hwnd uintptr) {
			activeDlgMu.Lock()
			activeDlgHwnd = hwnd
			activeDlgMu.Unlock()
		}, 440, 190)

	if btn == BTN_DISCONNECT || btn == -1 {
		activeDlgMu.Lock()
		user := activeDlgUser
		activeDlgMu.Unlock()
		if user != "" {
			releaseControl(user)
		}
	}
}

// ═══════════════════════════ 业务逻辑 ═══════════════════════════

func requestControl(userName string) string {
	allowed, denied := checkPermission(userName)
	if denied {
		return "denied"
	}
	if allowed {
		if acquireControl(userName) {
			go showActiveControlDialog(userName)
			return "granted"
		}
		return "busy"
	}

	controlMu.Lock()
	owner := controlOwner
	controlMu.Unlock()

	if owner != "" && owner != userName {
		return "busy"
	}

	return "pending"
}

func doRequestControlWithDialog(userName string, onResult func(granted bool)) {
	btn, remember := showControlRequestDialog(userName)

	switch btn {
	case BTN_ALLOW:
		if remember {
			rememberChoice(userName, true, false)
		}
		if acquireControl(userName) {
			go showActiveControlDialog(userName)
			onResult(true)
		} else {
			onResult(false)
		}

	case BTN_DENY:
		if remember {
			rememberChoice(userName, false, true)
		}
		onResult(false)

	default:
		// 关闭窗口
		onResult(false)
	}
}
