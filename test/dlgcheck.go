//go:build ignore

// dlgcheck.go — 深色弹窗参考实现
//
// 已验证要点:
// 1. runtime.LockOSThread() 防止 Go 线程迁移导致消息循环卡死
// 2. 不调用 TranslateMessage，避免 WM_CHAR 导致 BS_AUTOCHECKBOX 反复 toggle
// 3. 在窗口销毁前读取复选框状态，避免 SendMessage 到无效句柄
// 4. PostQuitMessage 统一退出消息循环

package main

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	u32 = syscall.NewLazyDLL("user32.dll")
	g32 = syscall.NewLazyDLL("gdi32.dll")
	k32 = syscall.NewLazyDLL("kernel32.dll")

	cwEx     = u32.NewProc("CreateWindowExW")
	regCls   = u32.NewProc("RegisterClassExW")
	defWnd   = u32.NewProc("DefWindowProcW")
	dstWnd   = u32.NewProc("DestroyWindow")
	postQuit = u32.NewProc("PostQuitMessage")
	getMsg   = u32.NewProc("GetMessageW")
	dispMsg  = u32.NewProc("DispatchMessageW")
	loadCur  = u32.NewProc("LoadCursorW")
	setFg    = u32.NewProc("SetForegroundWindow")
	sysInfo  = u32.NewProc("SystemParametersInfoW")
	sendMsg  = u32.NewProc("SendMessageW")
	loadIco  = u32.NewProc("LoadIconW")

	creatFont = g32.NewProc("CreateFontW")
	createBrs = g32.NewProc("CreateSolidBrush")
	setBkMode = g32.NewProc("SetBkMode")
	setTxtCol = g32.NewProc("SetTextColor")
	fillRect  = u32.NewProc("FillRect")

	modHdl = k32.NewProc("GetModuleHandleW")
)

const (
	WS_POPUP            = 0x80000000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_TABSTOP          = 0x00010000
	WS_EX_TOPMOST       = 0x00000008
	WS_EX_CONTROLPARENT = 0x00010000
	BS_PUSHBUTTON       = 0
	BS_DEFPUSHBUTTON    = 1
	BS_AUTOCHECKBOX     = 3
	SS_LEFT             = 0
	SS_ICON             = 3

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

	BTN_ALLOW    = 100
	BTN_DENY     = 102
	CHK_REMEMBER = 300

	// 深色主题
	bgR, bgG, bgB = 0x20, 0x20, 0x20
	txR, txG, txB = 0xE0, 0xE0, 0xE0

	dlgW = 440
	dlgH = 210
)

var (
	atom uint16
	mu   sync.Mutex

	fBig, fMid, fSml uintptr
	ico  uintptr
	bgBr uintptr
	txClr = uintptr(uint32(txR) | uint32(txG)<<8 | uint32(txB)<<16)

	// wproc 与 show() 之间的通信
	resultBtnID    int
	resultRemember bool
	resultReady    bool
	chkHwnd        uintptr
)

func u16(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, _ := syscall.UTF16PtrFromString(s); return p
}
func inst() uintptr { h, _, _ := modHdl.Call(0); return h }
func rgb(r, g, b uint8) uintptr {
	return uintptr(uint32(r) | uint32(g)<<8 | uint32(b)<<16)
}

func gdiInit() {
	fBig, _, _ = creatFont.Call(24, 0, 0, 0, 700, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(u16("Microsoft YaHei UI"))))
	fMid, _, _ = creatFont.Call(20, 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(u16("Microsoft YaHei UI"))))
	fSml, _, _ = creatFont.Call(18, 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 2|4, uintptr(unsafe.Pointer(u16("Microsoft YaHei UI"))))
	ico, _, _ = loadIco.Call(0, uintptr(IDI_INFORMATION))
	bgBr, _, _ = createBrs.Call(rgb(bgR, bgG, bgB))
}

func readCheck() {
	ck, _, _ := sendMsg.Call(chkHwnd, BM_GETCHECK, 0, 0)
	resultRemember = ck == 1
}

func wproc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case WM_NCHITTEST:
		r, _, _ := defWnd.Call(hwnd, msg, wp, lp)
		if r == 1 {
			return HTCAPTION
		}
		return r

	case WM_ERASEBKGND:
		rc := struct{ L, T, R, B int32 }{0, 0, dlgW, dlgH}
		_, _, _ = fillRect.Call(wp, uintptr(unsafe.Pointer(&rc)), bgBr)
		return 1

	case WM_CTLCOLORSTATIC:
		_, _, _ = setBkMode.Call(wp, TRANSPARENT)
		_, _, _ = setTxtCol.Call(wp, txClr)
		return bgBr

	case WM_COMMAND:
		id := int(uint64(wp) & 0xFFFF)
		if id == CHK_REMEMBER {
			return 0 // BS_AUTOCHECKBOX 自行 toggle
		}
		// 按钮点击：读复选框状态后退出
		readCheck()
		resultBtnID = id
		resultReady = true
		_, _, _ = postQuit.Call(0)
		return 0

	case WM_CLOSE:
		readCheck()
		resultReady = true
		_, _, _ = dstWnd.Call(hwnd)
		return 0

	case WM_DESTROY:
		_, _, _ = postQuit.Call(0)
		return 0
	}
	r, _, _ := defWnd.Call(hwnd, msg, wp, lp)
	return r
}

func doReg() error {
	if atom != 0 {
		return nil
	}
	cur, _, _ := loadCur.Call(0, uintptr(32512))
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
		lpfnWndProc:   syscall.NewCallback(wproc),
		hInstance:     inst(),
		hCursor:       cur,
		hbrBackground: bgBr,
		lpszClassName: u16("DlgV10"),
		hIcon:         ico,
		hIconSm:       ico,
	}
	a, _, _ := regCls.Call(uintptr(unsafe.Pointer(&wc)))
	if a == 0 {
		return fmt.Errorf("reg")
	}
	atom = uint16(a)
	return nil
}

func ctl(par uintptr, cls, txt string, st uintptr, id, x, y, w, h int) uintptr {
	t := uintptr(0)
	if txt != "" {
		t = uintptr(unsafe.Pointer(u16(txt)))
	}
	hw, _, _ := cwEx.Call(0, uintptr(unsafe.Pointer(u16(cls))), t,
		st|WS_CHILD|WS_VISIBLE, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		par, uintptr(id), inst(), 0)
	return hw
}

func show(header, subtext string) (btnID int, remember bool) {
	runtime.LockOSThread()

	mu.Lock()
	defer mu.Unlock()
	if err := doReg(); err != nil {
		fmt.Println(err)
		return -1, false
	}

	resultBtnID = 0
	resultRemember = false
	resultReady = false

	var wa struct{ L, T, R, B int32 }
	_, _, _ = sysInfo.Call(0x0030, 0, uintptr(unsafe.Pointer(&wa)), 0)
	x := int((wa.R - wa.L - dlgW) / 2)
	y := int((wa.B - wa.T - dlgH) / 2)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	hwnd, _, _ := cwEx.Call(WS_EX_TOPMOST|WS_EX_CONTROLPARENT,
		uintptr(unsafe.Pointer(u16("DlgV10"))), 0, WS_POPUP|WS_VISIBLE,
		uintptr(x), uintptr(y), uintptr(dlgW), uintptr(dlgH), 0, 0, inst(), 0)
	if hwnd == 0 {
		return -1, false
	}

	// 图标
	ic := ctl(hwnd, "STATIC", "", SS_ICON, 0, 18, 22, 28, 28)
	_, _, _ = sendMsg.Call(ic, STM_SETICON, ico, 0)

	// 标题
	h1 := ctl(hwnd, "STATIC", header, SS_LEFT, 0, 108, 26, dlgW-118, 36)
	_, _, _ = sendMsg.Call(h1, WM_SETFONT, fBig, 1)

	// 副标题
	h2 := ctl(hwnd, "STATIC", subtext, SS_LEFT, 0, 108, 68, dlgW-118, 20)
	_, _, _ = sendMsg.Call(h2, WM_SETFONT, fMid, 1)

	// 复选框
	chkHwnd = ctl(hwnd, "BUTTON", "记住我的选择", WS_TABSTOP|BS_AUTOCHECKBOX, CHK_REMEMBER, 108, 108, 200, 26)
	_, _, _ = sendMsg.Call(chkHwnd, WM_SETFONT, fSml, 1)

	// 按钮
	bw, bh, gap := 120, 38, 16
	bx := (dlgW - bw*2 - gap) / 2
	by := dlgH - 58
	ba := ctl(hwnd, "BUTTON", "允许", WS_TABSTOP|BS_DEFPUSHBUTTON, BTN_ALLOW, bx, by, bw, bh)
	_, _, _ = sendMsg.Call(ba, WM_SETFONT, fSml, 1)
	bd := ctl(hwnd, "BUTTON", "拒绝", WS_TABSTOP|BS_PUSHBUTTON, BTN_DENY, bx+bw+gap, by, bw, bh)
	_, _, _ = sendMsg.Call(bd, WM_SETFONT, fSml, 1)

	_, _, _ = setFg.Call(hwnd)

	// 消息循环（不调用 TranslateMessage）
	var msg [7]uintptr
	for {
		has, _, _ := getMsg.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if has == 0 {
			break
		}
		_, _, _ = dispMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
	}

	if resultReady {
		return resultBtnID, resultRemember
	}
	return -1, false
}

func main() {
	fmt.Println("=== 深色弹窗 ===\n")
	gdiInit()
	btn, rem := show("用户「测试用户」请求远程控制权限", "请选择允许或拒绝此请求")
	act := "关闭"
	if btn == BTN_ALLOW {
		act = "允许"
	}
	if btn == BTN_DENY {
		act = "拒绝"
	}
	fmt.Printf("结果: %s  记住: %v\n", act, rem)
}
