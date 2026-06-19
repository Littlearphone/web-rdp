package main

import (
	"fmt"
	"log"
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
	_getDC         = _u32.NewProc("GetDC")
	_relDC         = _u32.NewProc("ReleaseDC")
	_createIco     = _u32.NewProc("CreateIconIndirect")

	_creatFont   = _g32.NewProc("CreateFontW")
	_createBrs   = _g32.NewProc("CreateSolidBrush")
	_setBkMode   = _g32.NewProc("SetBkMode")
	_setTxtCol   = _g32.NewProc("SetTextColor")
	_fillRect    = _u32.NewProc("FillRect")
	_createDC    = _g32.NewProc("CreateCompatibleDC")
	_createBmp   = _g32.NewProc("CreateCompatibleBitmap")
	_selectObj   = _g32.NewProc("SelectObject")
	_deleteDC    = _g32.NewProc("DeleteDC")
	_deleteObj   = _g32.NewProc("DeleteObject")
	_setPixel    = _g32.NewProc("SetPixel")
	_roundRect   = _g32.NewProc("RoundRect")
	_createPen   = _g32.NewProc("CreatePen")
	_createSldBr = _g32.NewProc("CreateSolidBrush")
	_rect        = _g32.NewProc("Rectangle")
	_moveTo      = _g32.NewProc("MoveToEx")
	_lineTo      = _g32.NewProc("LineTo")
)

// ═══════════════════════════ 常量 ═══════════════════════════

const (
	WS_POPUP            = 0x80000000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_TABSTOP          = 0x00010000
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
	STM_SETICON       = 0x0170
	BM_GETCHECK       = 0x00F0

	BTN_ALLOW      = 100
	BTN_DENY       = 102
	BTN_DISCONNECT = 200
	CHK_REMEMBER   = 300
	CHK_GRANT_CTRL = 301 // 访问审批对话框的"授予控制权"复选框

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
	_permResultBtnID     int
	_permResultRemember  bool
	_permResultGrantCtrl bool
	_permResultReady     bool
	_permChkHwnd         uintptr
	_permChkGrantHwnd    uintptr // "授予控制权"复选框句柄
	_permShowCheckbox    bool    // 是否显示复选框
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

// iconInfo 对应 Windows ICONINFO 结构体
type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

// drawMonitorIcon 用 GDI 绘制一个 48x48 的显示器图标，完全嵌入可执行文件。
// 背景色与弹窗背景一致（#202020），整体协调不突兀。
func drawMonitorIcon() uintptr {
	hdc, _, _ := _getDC.Call(0)
	if hdc == 0 {
		return 0
	}
	defer _relDC.Call(0, hdc)

	memDC, _, _ := _createDC.Call(hdc)
	if memDC == 0 {
		return 0
	}
	defer _deleteDC.Call(memDC)

	hBmp, _, _ := _createBmp.Call(hdc, 48, 48)
	if hBmp == 0 {
		return 0
	}
	defer _deleteObj.Call(hBmp)

	oldBmp, _, _ := _selectObj.Call(memDC, hBmp)
	defer _selectObj.Call(memDC, oldBmp)

	// 背景与弹窗一致 #202020
	bgBr, _, _ := _createSldBr.Call(_rgb(0x20, 0x20, 0x20))
	defer _deleteObj.Call(bgBr)
	var fullRC struct{ L, T, R, B int32 }
	fullRC.R, fullRC.B = 48, 48
	_fillRect.Call(memDC, uintptr(unsafe.Pointer(&fullRC)), bgBr)

	// 显示器外框
	bezelPen, _, _ := _createPen.Call(0, 3, _rgb(0x66, 0x66, 0x66))
	defer _deleteObj.Call(bezelPen)
	bezelBr, _, _ := _createSldBr.Call(_rgb(0x40, 0x40, 0x40))
	defer _deleteObj.Call(bezelBr)
	oldPen, _, _ := _selectObj.Call(memDC, bezelPen)
	oldBr, _, _ := _selectObj.Call(memDC, bezelBr)
	_roundRect.Call(memDC, 5, 2, 43, 33, 6, 6)
	_roundRect.Call(memDC, 8, 5, 40, 30, 4, 4)
	_selectObj.Call(memDC, oldPen)
	_selectObj.Call(memDC, oldBr)

	// 屏幕区域
	screenBr, _, _ := _createSldBr.Call(_rgb(0x1A, 0x3A, 0x50))
	defer _deleteObj.Call(screenBr)
	var scrRC struct{ L, T, R, B int32 }
	scrRC.L, scrRC.T, scrRC.R, scrRC.B = 10, 7, 38, 28
	_fillRect.Call(memDC, uintptr(unsafe.Pointer(&scrRC)), screenBr)

	// 屏幕高光
	hlPen, _, _ := _createPen.Call(0, 1, _rgb(0x60, 0x80, 0x99))
	defer _deleteObj.Call(hlPen)
	_, _, _ = _selectObj.Call(memDC, hlPen)
	for y := int32(9); y <= 13; y += 2 {
		_moveTo.Call(memDC, 12, uintptr(y))
		_lineTo.Call(memDC, 36, uintptr(y))
	}
	_selectObj.Call(memDC, oldPen)

	// 支架
	standPen, _, _ := _createPen.Call(0, 2, _rgb(0x66, 0x66, 0x66))
	defer _deleteObj.Call(standPen)
	_, _, _ = _selectObj.Call(memDC, standPen)
	_moveTo.Call(memDC, 24, 33)
	_lineTo.Call(memDC, 24, 41)
	_moveTo.Call(memDC, 16, 41)
	_lineTo.Call(memDC, 32, 41)
	_selectObj.Call(memDC, oldPen)

	// AND 掩码全零 = 完全不透明
	hMask, _, _ := _createBmp.Call(hdc, 48, 48)
	defer _deleteObj.Call(hMask)

	var ii iconInfo
	ii.fIcon = 1
	ii.hbmColor = hBmp
	ii.hbmMask = hMask
	hIcon, _, _ := _createIco.Call(uintptr(unsafe.Pointer(&ii)))
	return hIcon
}

// ═══════════════════════════ GDI 初始化 ═══════════════════════════

func _permGdiInit() {
	if _permAtom != 0 {
		return // 已初始化
	}
	_permFBig, _, _ = _creatFont.Call(24, 0, 0, 0, 700, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(_u16("Microsoft YaHei UI"))))
	_permFMid, _, _ = _creatFont.Call(20, 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(_u16("Microsoft YaHei UI"))))
	_permFSml, _, _ = _creatFont.Call(18, 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(_u16("Microsoft YaHei UI"))))
	_permIco = drawMonitorIcon()
	_permBgBr, _, _ = _createBrs.Call(_rgb(_bgR, _bgG, _bgB))
}

// ═══════════════════════════ 窗口过程 ═══════════════════════════

func _permReadCheck() {
	ck, _, _ := _sendMsg.Call(_permChkHwnd, BM_GETCHECK, 0, 0)
	_permResultRemember = ck == 1
}

func _permReadGrantCheck() {
	if _permChkGrantHwnd == 0 {
		_permResultGrantCtrl = false
		return
	}
	ck, _, _ := _sendMsg.Call(_permChkGrantHwnd, BM_GETCHECK, 0, 0)
	_permResultGrantCtrl = ck == 1
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
		if id == CHK_REMEMBER || id == CHK_GRANT_CTRL {
			return 0 // 复选框切换，不关闭窗口
		}
		if _permShowCheckbox {
			_permReadCheck()
		}
		_permReadGrantCheck()
		_permResultBtnID = id
		_permResultReady = true
		_, _, _ = _dstWnd.Call(hwnd)
		return 0

	case WM_CLOSE:
		if _permShowCheckbox {
			_permReadCheck()
		}
		_permReadGrantCheck()
		_permResultReady = true
		_permResultBtnID = -1
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
// showGrantCheckbox=true 时额外渲染"同时授予控制权"复选框（位置在记住选择上方）。
func runDarkDialog(header, body string, buttons []permBtn,
	showCheckbox bool, showGrantCheckbox bool, onCreated func(hwnd uintptr), dlgW, dlgH int) (btnID int, remember bool, grantCtrl bool) {

	runtime.LockOSThread()

	_permMu.Lock()
	defer _permMu.Unlock()

	if err := _permRegClass(); err != nil {
		return -1, false, false
	}

	_permResultBtnID = 0
	_permResultRemember = false
	_permResultGrantCtrl = false
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

	hwnd, _, _ := _cwEx.Call(
		uintptr(WS_EX_CONTROLPARENT),
		uintptr(unsafe.Pointer(_u16("RDPPermDarkV1"))),
		uintptr(unsafe.Pointer(_u16(header))),
		uintptr(WS_POPUP|WS_VISIBLE),
		uintptr(x), uintptr(y), uintptr(dlgW), uintptr(dlgH),
		0, 0, _inst(), 0,
	)
	if hwnd == 0 {
		return -1, false, false
	}

	if onCreated != nil {
		onCreated(hwnd)
	}

	// 图标
	ic := _permCtl(hwnd, "STATIC", "", SS_ICON, 0, 40, 28, 48, 48)
	_, _, _ = _sendMsg.Call(ic, STM_SETICON, _permIco, 0)

	// 标题
	h1 := _permCtl(hwnd, "STATIC", header, SS_LEFT, 0, 108, 26, dlgW-118, 36)
	_, _, _ = _sendMsg.Call(h1, WM_SETFONT, _permFBig, 1)

	// 正文
	h2 := _permCtl(hwnd, "STATIC", body, SS_LEFT, 0, 108, 68, dlgW-118, 20)
	_, _, _ = _sendMsg.Call(h2, WM_SETFONT, _permFMid, 1)

	// "同时授予控制权"复选框（在记住选择上方）
	if showGrantCheckbox {
		_permChkGrantHwnd = _permCtl(hwnd, "BUTTON", "同时授予控制权", WS_TABSTOP|BS_AUTOCHECKBOX, CHK_GRANT_CTRL, 108, 100, 220, 26)
		_, _, _ = _sendMsg.Call(_permChkGrantHwnd, WM_SETFONT, _permFSml, 1)
		// 默认勾选
		_, _, _ = _sendMsg.Call(_permChkGrantHwnd, BM_GETCHECK+1, 1, 0) // BM_SETCHECK=0x00F1, BST_CHECKED=1
	} else {
		_permChkGrantHwnd = 0
	}

	// "记住我的选择"复选框
	if showCheckbox {
		ckY := 108
		if showGrantCheckbox {
			ckY = 130
		}
		_permChkHwnd = _permCtl(hwnd, "BUTTON", "记住我的选择", WS_TABSTOP|BS_AUTOCHECKBOX, CHK_REMEMBER, 108, ckY, 220, 26)
		_, _, _ = _sendMsg.Call(_permChkHwnd, WM_SETFONT, _permFSml, 1)
	}

	// 按钮
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

	// 消息循环
	var msg [7]uintptr
	for {
		has, _, _ := _getMsg.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if has == 0 {
			break
		}
		_, _, _ = _dispMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
	}

	runtime.UnlockOSThread()

	if _permResultReady {
		return _permResultBtnID, _permResultRemember, _permResultGrantCtrl
	}
	return -1, false, false
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
		_, _, _ = _sendMsg.Call(hwnd, WM_CLOSE, 0, 0)
	}
}

// ═══════════════════════════ 对话框实现 ═══════════════════════════

func showControlRequestDialog(userName string) (int, bool) {
	buttons := []permBtn{
		{BTN_ALLOW, "允许", true},
		{BTN_DENY, "拒绝", false},
	}

	header := fmt.Sprintf("「%s」请求远程控制权限", userName)
	body := "请选择允许或拒绝此请求。\n勾选【记住我的选择】可将本次选择设为永久规则。"

	btn, remember, _ := runDarkDialog(header, body, buttons, true, false, func(hwnd uintptr) {
		activeDlgMu.Lock()
		activeDlgUser = userName
		activeDlgHwnd = hwnd
		activeDlgMu.Unlock()
	}, 480, 220)

	activeDlgMu.Lock()
	if activeDlgUser == userName {
		activeDlgUser = ""
		activeDlgHwnd = 0
	}
	activeDlgMu.Unlock()

	return btn, remember
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

	header := fmt.Sprintf("「%s」正在控制此电脑", userName)
	body := "点击「断开控制」可立即终止此用户的控制权限。"

	btn, _, _ := runDarkDialog(header, body, buttons, false, false,
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

// showAccessRequestDialog 显示访问审批弹窗（匿名用户请求连接时）。
// 比 showControlRequestDialog 多一个"同时授予控制权"复选框。
// 返回: btnID, remember, grantCtrl
func showAccessRequestDialog(userName string) (int, bool, bool) {
	buttons := []permBtn{
		{BTN_ALLOW, "允许访问", true},
		{BTN_DENY, "拒绝", false},
	}

	header := fmt.Sprintf("「%s」请求访问此电脑", userName)
	body := "该用户未提供密码，需要您的审批。\n勾选【同时授予控制权】可直接让其操控桌面。"

	btn, remember, grantCtrl := runDarkDialog(header, body, buttons, true, true, func(hwnd uintptr) {
		activeDlgMu.Lock()
		activeDlgUser = userName
		activeDlgHwnd = hwnd
		activeDlgMu.Unlock()
	}, 480, 250)

	activeDlgMu.Lock()
	if activeDlgUser == userName {
		activeDlgUser = ""
		activeDlgHwnd = 0
	}
	activeDlgMu.Unlock()

	return btn, remember, grantCtrl
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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("权限弹窗 panic: %v", r)
			onResult(false)
		}
	}()

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
		onResult(false)
	}
}

// doRequestAccessWithDialog 显示访问审批弹窗并处理结果。
// grantCtrl: 宿主在弹窗中是否勾选了"同时授予控制权"。
func doRequestAccessWithDialog(userName string, onResult func(allowed bool, grantCtrl bool)) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("访问审批弹窗 panic: %v", r)
			onResult(false, false)
		}
	}()

	btn, remember, grantCtrl := showAccessRequestDialog(userName)

	switch btn {
	case BTN_ALLOW:
		if remember {
			rememberChoice(userName, true, false)
		}
		// 如果勾选了"同时授予控制权"，用 force=true 顶替当前控制者
		if grantCtrl {
			if acquireControlForce(userName, true) {
				go showActiveControlDialog(userName)
			}
		}
		onResult(true, grantCtrl)

	case BTN_DENY:
		if remember {
			rememberChoice(userName, false, true)
		}
		onResult(false, false)

	default:
		// 关闭窗口 = 拒绝
		onResult(false, false)
	}
}
