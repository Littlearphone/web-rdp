package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"image"
	_ "image/gif"  // 注册 GIF 解码器（浏览器剪贴板可能存 GIF）
	_ "image/jpeg" // 注册 JPEG 解码器（浏览器剪贴板常存 JPEG）
	"image/png"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

// tlsLogFilter 过滤 http.Server.ErrorLog 中的 TLS 握手错误。
// 自签名证书下浏览器在用户手动信任前会触发大量 "TLS handshake error"，
// 这些是预期行为，不需要输出到控制台。
type tlsLogFilter struct{ out io.Writer }

func (f *tlsLogFilter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return f.out.Write(p)
}

//go:embed static
var staticFS embed.FS

// httpClient 是全局复用的 HTTP 客户端，用于下载 ffmpeg 等网络请求
var httpClient *http.Client

// initHTTPClient 初始化全局 HTTP 客户端，支持可选的 HTTP 代理
func initHTTPClient(proxy string) {
	tr := &http.Transport{
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxy != "" {
		if u, err := url.Parse("http://" + proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
			fmt.Printf("使用代理: %s\n", proxy)
		}
	}
	httpClient = &http.Client{Timeout: 5 * time.Minute, Transport: tr}
}

// ── Windows API 绑定 ──
// 通过 syscall.NewLazyDLL 延迟加载 user32.dll，避免不必要的 DLL 加载
var (
	controlOwner            string     // 当前控制权的持有者用户名
	controlMu               sync.Mutex // 控制权的互斥锁
	user32                  = syscall.NewLazyDLL("user32.dll")
	gdi32                   = syscall.NewLazyDLL("gdi32.dll")
	procSetCursorPos        = user32.NewProc("SetCursorPos")        // 设置光标位置
	procMouseWait           = user32.NewProc("mouse_event")         // 鼠标事件（点击/移动）
	procKeybdWait           = user32.NewProc("keybd_event")         // 键盘事件（按下/释放）
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors") // 枚举显示器
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")  // 设置 DPI 感知
	procGetDC               = user32.NewProc("GetDC")               // 获取桌面 DC（用于读取刷新率）
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procGetDeviceCaps       = gdi32.NewProc("GetDeviceCaps")         // VREFRESH=116 获取刷新率
	procEnumDisplaySettings = user32.NewProc("EnumDisplaySettingsW") // 逐显示器读取 DEVMODE
	procEnumDisplayDevices  = user32.NewProc("EnumDisplayDevicesW")  // 枚举显示设备列表
	procGetMonitorInfo      = user32.NewProc("GetMonitorInfoW")      // 获取 HMONITOR 信息（含设备名）

	// 剪贴板 API
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procRtlMoveMemory              = kernel32.NewProc("RtlMoveMemory")

	// 认证
	authPassword string                    // 预设密码（空 = 自动生成随机密码）
	authNonces   = make(map[string]string) // challenge nonce 池，key 为 nonce ID
	authNoncesMu sync.Mutex
)

// keyCodeMap 将浏览器 KeyboardEvent.code 映射为 Windows 虚拟键码（VK）
var keyCodeMap = map[string]uintptr{
	"Backspace": 0x08, "Tab": 0x09, "Enter": 0x0D,
	"ShiftLeft": 0xA0, "ShiftRight": 0xA1,
	"ControlLeft": 0xA2, "ControlRight": 0xA3,
	"AltLeft": 0xA4, "AltRight": 0xA5,
	"CapsLock": 0x14, "Escape": 0x1B, "Space": 0x20,
	"PageUp": 0x21, "PageDown": 0x22, "End": 0x23, "Home": 0x24,
	"ArrowLeft": 0x25, "ArrowUp": 0x26, "ArrowRight": 0x27, "ArrowDown": 0x28,
	"Insert": 0x2D, "Delete": 0x2E,
	"MetaLeft": 0x5B, "MetaRight": 0x5C,
	"F1": 0x70, "F2": 0x71, "F3": 0x72, "F4": 0x73,
	"F5": 0x74, "F6": 0x75, "F7": 0x76, "F8": 0x77,
	"F9": 0x78, "F10": 0x79, "F11": 0x7A, "F12": 0x7B,
}

// doTypeText 模拟键盘输入文本字符串，逐字符发送 keydown + keyup 事件
func doTypeText(text string) {
	for _, r := range text {
		vk := uintptr(r)
		_, _, _ = procKeybdWait.Call(vk, 0, 0, 0)      // keydown
		_, _, _ = procKeybdWait.Call(vk, 0, 0x0002, 0) // keyup (flag=0x0002 = KEYEVENTF_KEYUP)
	}
}

// doKey 模拟单个按键的按下或释放。
// code 支持三种格式：标准键名（如 "Enter"）、Key+字符（如 "KeyA"）、Digit+数字（如 "Digit1"）、VK+十六进制码
func doKey(code string, down bool) {
	vk, ok := keyCodeMap[code]
	if !ok && len(code) >= 4 && code[:3] == "Key" {
		vk = uintptr(code[3])
	} else if !ok && len(code) >= 5 && code[:5] == "Digit" {
		vk = uintptr(code[5])
	} else if !ok && len(code) >= 2 && code[:2] == "VK" {
		if n, err := strconv.ParseUint(code[2:], 10, 64); err == nil {
			vk = uintptr(n)
		}
	}
	if vk == 0 {
		return
	}
	flag := uintptr(0)
	if !down {
		flag = 0x0002
	}
	_, _, _ = procKeybdWait.Call(vk, 0, flag, 0)
}

// doRightClick 在指定屏幕坐标执行鼠标右键单击（先移动光标再点击）
// doRightClick 在指定屏幕坐标执行鼠标右键单击（先移动光标再点击）。
// mouse_event 是队列化的，SetCursorPos 是同步的，无需长等待。
// 仅保留按键抬起前的短间隔（15ms），确保应用能识别为一次完整点击。
func doRightClick(x, y int32) {
	ix, iy := uintptr(x), uintptr(y)
	_, _, _ = procSetCursorPos.Call(ix, iy)
	_, _, _ = procMouseWait.Call(0x0008, ix, iy, 0, 0) // RIGHTDOWN
	time.Sleep(15 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0010, ix, iy, 0, 0) // RIGHTUP
}

// acquireControl 尝试获取远程控制权。同一用户可重复获取；其他用户需等待当前持有者释放。
// 返回 true 表示获取成功，false 表示被其他用户占用
func acquireControl(user string) bool { return acquireControlForce(user, false) }

// acquireControlForce 获取或顶替控制权。force=true 时无视当前持有者直接抢占。
func acquireControlForce(user string, force bool) bool {
	controlMu.Lock()
	defer controlMu.Unlock()
	if controlOwner == "" || controlOwner == user {
		controlOwner = user
		return true
	}
	if force {
		oldOwner := controlOwner
		controlOwner = user
		log.Printf("[%s] 控制权被 %s 顶替", oldOwner, user)
		return true
	}
	return false
}

// releaseControl 释放当前用户的控制权。仅当调用者是当前持有者时才生效
func releaseControl(user string) {
	controlMu.Lock()
	defer controlMu.Unlock()
	if controlOwner == user {
		controlOwner = ""
	}
}

// doDrag 模拟鼠标拖拽操作：从 (x1,y1) 按下左键并移动到 (x2,y2) 后释放。
// SetCursorPos 是同步的（调用后光标已在目标位置），但 mouse_event 是队列化的 ——
// 系统需要时间将 LEFTDOWN 派发到目标窗口后，后续的 WM_MOUSEMOVE（由 SetCursorPos
// 生成）才能携带按下状态，从而被窗口管理器识别为拖拽。
// 因此必须在 LEFTDOWN 之后、SetCursorPos 之前插入延迟。
func doDrag(x1, y1, x2, y2 int32) {
	ix1, iy1 := uintptr(x1), uintptr(y1)
	ix2, iy2 := uintptr(x2), uintptr(y2)
	_, _, _ = procSetCursorPos.Call(ix1, iy1)
	time.Sleep(15 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0002, 0, 0, 0, 0) // LEFTDOWN
	time.Sleep(15 * time.Millisecond)
	_, _, _ = procSetCursorPos.Call(ix2, iy2) // 移动光标到目标位置（产生 WM_MOUSEMOVE）
	time.Sleep(15 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0004, 0, 0, 0, 0) // LEFTUP
}

// ── 剪贴板操作（纯轮询，无 LockOSThread）──

// getClipboardSeq 获取剪贴板序列号（每次内容变化时自增）。
// 无需 OpenClipboard，任意线程可安全调用。
func getClipboardSeq() uint32 {
	s, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(s)
}

// clipLock 保护剪贴板 Open/Close 配对操作的互斥锁
var clipLock sync.Mutex

// getClipboardText 以 Unicode 文本格式读取剪贴板内容。
// 返回空字符串表示剪贴板中没有文本数据或读取失败。
func getClipboardText() string {
	clipLock.Lock()
	defer clipLock.Unlock()

	r, _, _ := procOpenClipboard.Call(0) // NULL hWnd = 当前任务窗口
	if r == 0 {
		return ""
	}
	defer procCloseClipboard.Call()

	// CF_UNICODETEXT = 13
	h, _, _ := procGetClipboardData.Call(13)
	if h == 0 {
		return ""
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(h)

	sz, _, _ := procGlobalSize.Call(h)
	if sz == 0 || sz > 10*1024*1024 { // 上限 10MB，防止异常数据
		return ""
	}
	// UTF-16LE → Go string，去除末尾 null 终止符
	buf := make([]uint16, sz/2)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), p, sz)
	return syscall.UTF16ToString(buf)
}

// setClipboardText 将文本写入剪贴板（Unicode）。
func setClipboardText(text string) error {
	clipLock.Lock()
	defer clipLock.Unlock()

	r, _, _ := procOpenClipboard.Call(0)
	if r == 0 {
		return fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()

	_, _, _ = procEmptyClipboard.Call()

	u16, _ := syscall.UTF16FromString(text)
	byteLen := uintptr(len(u16) * 2)
	hMem, _, _ := procGlobalAlloc.Call(0x0002, byteLen) // GMEM_MOVEABLE
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc failed")
	}
	p, _, _ := procGlobalLock.Call(hMem)
	if p == 0 {
		procGlobalUnlock.Call(hMem)
		return fmt.Errorf("GlobalLock failed")
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u16[0])), byteLen)
	procGlobalUnlock.Call(hMem)

	// CF_UNICODETEXT = 13
	d, _, _ := procSetClipboardData.Call(13, hMem)
	if d == 0 {
		return fmt.Errorf("SetClipboardData failed")
	}
	return nil
}

// ═══════════════════ 剪贴板图像 (CF_DIB ↔ PNG) ═══════════════════

// getClipboardImage 以 PNG 格式读取剪贴板中的图像数据。
// 剪贴板图像格式为 CF_DIB（设备无关位图），转换为 PNG 便于网络传输。
// 返回 nil 表示剪贴板中没有图像或读取失败。
//
// 重要：必须在 DIB→PNG 转换之前释放剪贴板锁和 CloseClipboard，
// 否则 PNG 编码（耗时可达数百毫秒）期间整个系统的剪贴板都不可用。
func getClipboardImage() []byte {
	clipLock.Lock()

	r, _, _ := procOpenClipboard.Call(0)
	if r == 0 {
		clipLock.Unlock()
		return nil
	}

	// CF_DIB = 8
	h, _, _ := procGetClipboardData.Call(8)
	if h == 0 {
		procCloseClipboard.Call()
		clipLock.Unlock()
		return nil
	}

	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procCloseClipboard.Call()
		clipLock.Unlock()
		return nil
	}

	sz, _, _ := procGlobalSize.Call(h)
	if sz == 0 || sz > 50*1024*1024 {
		procGlobalUnlock.Call(h)
		procCloseClipboard.Call()
		clipLock.Unlock()
		return nil
	}

	// 复制 DIB 数据后立即释放所有剪贴板资源
	dib := make([]byte, sz)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&dib[0])), p, sz)
	procGlobalUnlock.Call(h)
	procCloseClipboard.Call()
	clipLock.Unlock()

	// 现在安全地进行昂贵的 PNG 编码，不持有任何锁
	pngBytes, err := dibToPNG(dib)
	if err != nil {
		return nil
	}
	return pngBytes
}

// setClipboardImage 将图像数据（PNG/JPEG/GIF 等任意格式）写入 Windows 剪贴板（CF_DIB 格式）。
// 使用 image.Decode 自动检测格式，浏览器传来的可能是 JPEG/PNG/WebP 等。
func setClipboardImage(imgData []byte) error {
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return fmt.Errorf("图片解码失败: %w", err)
	}

	dib, err := rgbaToDIB(img)
	if err != nil {
		return fmt.Errorf("DIB 转换失败: %w", err)
	}

	clipLock.Lock()
	defer clipLock.Unlock()

	r, _, _ := procOpenClipboard.Call(0)
	if r == 0 {
		return fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()

	_, _, _ = procEmptyClipboard.Call()

	hMem, _, _ := procGlobalAlloc.Call(0x0002, uintptr(len(dib)))
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc failed")
	}
	p, _, _ := procGlobalLock.Call(hMem)
	if p == 0 {
		return fmt.Errorf("GlobalLock failed")
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&dib[0])), uintptr(len(dib)))
	procGlobalUnlock.Call(hMem)

	// CF_DIB = 8
	d, _, _ := procSetClipboardData.Call(8, hMem)
	if d == 0 {
		return fmt.Errorf("SetClipboardData failed")
	}
	return nil
}

// dibToPNG 将 Windows DIB（设备无关位图）字节转换为 PNG 编码。
// DIB = BITMAPINFOHEADER + color table (可选) + pixel data.
// 支持 32-bit BGRA 和 24-bit BGR，BI_RGB 和 BI_BITFIELDS 压缩.
func dibToPNG(dib []byte) ([]byte, error) {
	if len(dib) < 40 {
		return nil, fmt.Errorf("DIB too small: %d bytes", len(dib))
	}

	// 解析 BITMAPINFOHEADER
	biWidth := int(int32(binary.LittleEndian.Uint32(dib[4:8])))
	biHeight := int(int32(binary.LittleEndian.Uint32(dib[8:12])))
	biBitCount := binary.LittleEndian.Uint16(dib[14:16])
	biCompression := binary.LittleEndian.Uint32(dib[16:20])

	if biWidth <= 0 || biWidth > 16384 || biHeight == 0 || (biHeight > 0 && biHeight > 16384) || (biHeight < 0 && -biHeight > 16384) {
		return nil, fmt.Errorf("DIB dimensions out of range: %dx%d", biWidth, biHeight)
	}

	topDown := biHeight < 0
	absHeight := biHeight
	if absHeight < 0 {
		absHeight = -absHeight
	}

	// 计算像素数据偏移（跳过 color table 和 bitfield masks）
	offset := 40            // BITMAPINFOHEADER size
	if biCompression == 3 { // BI_BITFIELDS
		offset += 12 // 3 DWORD masks
	}
	if biBitCount <= 8 {
		clrUsed := int(binary.LittleEndian.Uint32(dib[32:36]))
		if clrUsed == 0 && biBitCount <= 8 {
			clrUsed = 1 << biBitCount
		}
		offset += clrUsed * 4
	}

	if offset > len(dib) {
		return nil, fmt.Errorf("DIB header overflow")
	}

	// 行对齐到 4 字节边界
	rowSize := ((biWidth*int(biBitCount) + 31) / 32) * 4
	expectedSize := offset + rowSize*absHeight
	if len(dib) < expectedSize {
		if len(dib) < offset+rowSize {
			return nil, fmt.Errorf("DIB pixel data too small")
		}
		absHeight = (len(dib) - offset) / rowSize
	}

	img := image.NewRGBA(image.Rect(0, 0, biWidth, absHeight))

	for row := 0; row < absHeight; row++ {
		var srcRow int
		if topDown {
			srcRow = row
		} else {
			srcRow = absHeight - 1 - row // bottom-up DIB
		}

		srcOff := offset + srcRow*rowSize
		if srcOff+rowSize > len(dib) {
			break
		}

		dstOff := row * img.Stride
		switch biBitCount {
		case 32:
			for x := 0; x < biWidth; x++ {
				off := srcOff + x*4
				if off+4 > len(dib) {
					break
				}
				img.Pix[dstOff+x*4] = dib[off+2]   // R (BGRA → RGBA)
				img.Pix[dstOff+x*4+1] = dib[off+1] // G
				img.Pix[dstOff+x*4+2] = dib[off]   // B
				img.Pix[dstOff+x*4+3] = 255        // A (ignore alpha from DIB)
			}
		case 24:
			for x := 0; x < biWidth; x++ {
				off := srcOff + x*3
				if off+3 > len(dib) {
					break
				}
				img.Pix[dstOff+x*4] = dib[off+2]   // R (BGR → RGBA)
				img.Pix[dstOff+x*4+1] = dib[off+1] // G
				img.Pix[dstOff+x*4+2] = dib[off]   // B
				img.Pix[dstOff+x*4+3] = 255
			}
		default:
			return nil, fmt.Errorf("unsupported DIB bit count: %d", biBitCount)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rgbaToDIB 将 Go image.Image 转换为 Windows DIB（32-bit BGRA, bottom-up）。
func rgbaToDIB(img image.Image) ([]byte, error) {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return nil, fmt.Errorf("image dimensions out of range: %dx%d", w, h)
	}

	rowSize := ((w*32 + 31) / 32) * 4
	dibSize := 40 + rowSize*h

	dib := make([]byte, dibSize)

	// BITMAPINFOHEADER (40 bytes)
	binary.LittleEndian.PutUint32(dib[0:4], 40)                  // biSize
	binary.LittleEndian.PutUint32(dib[4:8], uint32(w))           // biWidth
	binary.LittleEndian.PutUint32(dib[8:12], uint32(h))          // biHeight (positive = bottom-up)
	binary.LittleEndian.PutUint16(dib[12:14], 1)                 // biPlanes
	binary.LittleEndian.PutUint16(dib[14:16], 32)                // biBitCount
	binary.LittleEndian.PutUint32(dib[16:20], 0)                 // biCompression = BI_RGB
	binary.LittleEndian.PutUint32(dib[20:24], uint32(rowSize*h)) // biSizeImage

	rgba, ok := img.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(bounds)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				rgba.Set(x, y, img.At(x, y))
			}
		}
	}

	for row := 0; row < h; row++ {
		srcRow := h - 1 - row // bottom-up
		srcOff := srcRow * rgba.Stride
		dstOff := 40 + row*rowSize
		for x := 0; x < w; x++ {
			dib[dstOff+x*4] = rgba.Pix[srcOff+x*4+2]   // B
			dib[dstOff+x*4+1] = rgba.Pix[srcOff+x*4+1] // G
			dib[dstOff+x*4+2] = rgba.Pix[srcOff+x*4]   // R
			dib[dstOff+x*4+3] = 0                      // A (ignored)
		}
	}

	return dib, nil
}

// ── 认证 ──

// genNonce 生成随机挑战字符串，存入池中并返回给客户端。
// 同一字符串同时作为 map key 和 challenge 值，客户端用它计算 SHA-256(challenge+password)。
func genNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	challenge := hex.EncodeToString(b)
	authNoncesMu.Lock()
	authNonces[challenge] = challenge // key==value，发下去的就是 challenge 本身
	authNoncesMu.Unlock()
	return challenge
}

// verifyAuth 验证客户端提交的 auth token。
func verifyAuth(challenge, response, password string) bool {
	authNoncesMu.Lock()
	_, ok := authNonces[challenge]
	if ok {
		delete(authNonces, challenge) // 一次性使用，防重放
	}
	authNoncesMu.Unlock()
	if !ok {
		return false
	}
	expected := sha256Hex(challenge + password)
	return expected == response
}

// sha256Hex 计算 SHA-256 并以十六进制字符串返回
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// generateRandomPassword 生成 12 位可读随机密码（含大小写字母和数字）
func generateRandomPassword() string {
	const chars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 12)
	for i := range b {
		r := make([]byte, 1)
		_, _ = rand.Read(r)
		b[i] = chars[int(r[0])%len(chars)]
	}
	return string(b)
}

// RECT 定义 Windows RECT 结构体，用于 EnumDisplayMonitors 回调
type RECT struct{ Left, Top, Right, Bottom int32 }

// upgrader 将 HTTP 连接升级为 WebSocket 连接，允许所有来源
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ctrlMsg 定义 WebSocket 控制消息的 JSON 结构。
// 所有字段均为可选指针，仅发送变更的字段以减少带宽
type ctrlMsg struct {
	Control        *bool   `json:"control,omitempty"`
	Screen         *int    `json:"screen,omitempty"`
	Quality        *int    `json:"quality,omitempty"`
	MaxW           *int    `json:"maxw,omitempty"`
	Key            *string `json:"key,omitempty"`
	KeyDown        *bool   `json:"down,omitempty"`
	Text           *string `json:"text,omitempty"`
	RX             *int    `json:"rx,omitempty"`
	RY             *int    `json:"ry,omitempty"`
	DX1            *int    `json:"dx1,omitempty"`
	DY1            *int    `json:"dy1,omitempty"`
	DX2            *int    `json:"dx2,omitempty"`
	DY2            *int    `json:"dy2,omitempty"`
	MX             *int    `json:"mx,omitempty"`
	MY             *int    `json:"my,omitempty"`
	Webcodecs      *bool   `json:"webcodecs,omitempty"`
	Fps            *int    `json:"fps,omitempty"`
	MouseBtn       *string `json:"mb,omitempty"`              // 鼠标按钮: "left" / "right"
	MouseDn        *bool   `json:"md,omitempty"`              // true=按下 / false=释放
	User           *string `json:"user,omitempty"`            // 修改用户名
	Clipboard      *string `json:"clipboard,omitempty"`       // 剪贴板文本（双向同步）
	ClipboardImage *string `json:"clipboard_image,omitempty"` // 剪贴板图像（base64 PNG，双向同步）
	Auth           *string `json:"auth,omitempty"`            // 认证响应: sha256(challenge+password) 或 "anonymous"
	// WebRTC 信令（内网直连模式）
	RTCWebRTC *bool   `json:"rtc_webrtc,omitempty"` // 前端告知支持 WebRTC
	RTCSDP    *string `json:"rtc_sdp,omitempty"`    // SDP Offer/Answer
	RTCIce    *string `json:"rtc_ice,omitempty"`    // ICE Candidate (JSON)
	// 自适应码率（前端网络反馈）
	AdaptMode *string  `json:"adapt_mode,omitempty"` // "quality" | "smooth"
	NetFPS    *float64 `json:"net_fps,omitempty"`    // 前端实际接收帧率
	NetQueue  *int     `json:"net_queue,omitempty"`  // 前端解码队列深度
}

// statsMsg 定义性能统计消息，每秒由后端推送到前端用于状态栏展示
type statsMsg struct {
	Owner       string  `json:"owner"`
	FPS         float64 `json:"fps"`
	EncMs       float64 `json:"enc_ms"`
	KB          float64 `json:"kb"`
	Q           int     `json:"q"`
	W           int     `json:"w"`
	H           int     `json:"h"`
	Ox          int     `json:"ox"`
	Oy          int     `json:"oy"`
	Zoom        float64 `json:"zoom"`
	Screens     int     `json:"screens"`
	MaxRate     int     `json:"maxrate"`      // 显示器刷新率上限（仅 ddagrab）
	Users       int     `json:"users"`        // 当前在线连接数
	AdaptActive bool    `json:"adapt_active"` // 自适应是否正在降级
	AdaptQ      int     `json:"adapt_q"`      // 自适应目标画质
	AdaptFPS    int     `json:"adapt_fps"`    // 自适应目标帧率
}

// main 是程序入口，负责解析命令行参数、初始化组件并启动 HTTP 服务器。
// 主要流程：解析参数 → 设置 DPI → 初始化 HTTP 客户端 → 检测 ffmpeg → 检测编码器 → 启动服务
// ── 证书持久化 ──

// loadCertFromDisk 从磁盘加载 PEM 证书和私钥。
// 如果任一文件不存在、解析失败或证书距过期 ≤30 天，返回 false。
func loadCertFromDisk(certFile, keyFile string) (tls.Certificate, bool) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, false
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, false
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("⚠ 证书解析失败: %v，重新生成", err)
		return tls.Certificate{}, false
	}
	// 解析证书检查过期时间
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	if time.Until(x509Cert.NotAfter) <= 30*24*time.Hour {
		fmt.Printf("→ 证书即将过期 (%s)，重新生成\n", x509Cert.NotAfter.Format("2006-01-02"))
		return tls.Certificate{}, false
	}
	fmt.Printf("→ 使用已有证书 (过期: %s)\n", x509Cert.NotAfter.Format("2006-01-02"))
	return cert, true
}

// generateSelfSignedCert 生成自签名 ECDSA P-256 证书，返回 cert 和 PEM 编码数据。
// 证书有效期 365 天，SAN 包含主机名和本机非回环 IPv4 地址。
func generateSelfSignedCert() (tls.Certificate, []byte, []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("生成 ECDSA 密钥失败: %v", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(365 * 24 * time.Hour)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		log.Fatalf("生成证书序列号失败: %v", err)
	}

	hostname, _ := os.Hostname()

	var ips []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() {
			ips = append(ips, ipNet.IP)
		}
	}

	dnsNames := []string{hostname, "localhost"}
	ips = append(ips, net.ParseIP("127.0.0.1"))

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname, Organization: []string{"web-rdp"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatalf("生成自签名证书失败: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("序列化私钥失败: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Fatalf("加载证书失败: %v", err)
	}
	return cert, certPEM, keyPEM
}

func main() {
	var (
		proxy     string // HTTP 代理地址
		port      int    // 监听端口
		listen    string // 监听地址
		ffmpegArg string // 手动指定的 ffmpeg 路径
		useTLS    bool   // 是否启用 HTTPS（自动生成自签名证书）
		password  string // 访问密码
	)
	flag.StringVar(&proxy, "proxy", "", "HTTP 代理地址 (用于下载 ffmpeg)")
	flag.IntVar(&port, "port", 9000, "监听端口")
	flag.StringVar(&listen, "listen", "", "监听地址 (默认所有网卡)")
	flag.StringVar(&ffmpegArg, "ffmpeg", "", "手动指定 ffmpeg.exe 路径")
	flag.BoolVar(&useTLS, "tls", true, "启用 HTTPS，-tls=false 禁用（自签名证书，局域网 H.264 需要）")
	flag.StringVar(&password, "password", "", "访问密码（空=随机生成，0=无需密码）")
	flag.Usage = func() {
		o := flag.CommandLine.Output()
		fmt.Fprintf(o, "Web 远程控制 v1.0\n\n用法: %s [选项]\n\n选项:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprint(o, "\n示例:\n  web-rdp.exe                                    默认 HTTPS :9000\n  web-rdp.exe -port 8080                          指定端口\n  web-rdp.exe -listen 127.0.0.1                   仅本机\n  web-rdp.exe -ffmpeg C:\\tools\\ffmpeg.exe         手动指定 ffmpeg\n  web-rdp.exe -proxy :7890                        走代理下载\n  web-rdp.exe -tls=false                          禁用 HTTPS，回退 HTTP\n")
	}
	flag.Parse()

	// ── 密码初始化 ──
	if password == "0" {
		authPassword = "" // 无需密码
		fmt.Println("⚠ 无需密码模式：任何人可直接连接（仅建议内网使用）")
	} else if password != "" {
		authPassword = password
		fmt.Printf("→ 使用预设密码进行访问控制\n")
	} else {
		authPassword = generateRandomPassword()
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("  随机访问密码: %s\n", authPassword)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	}
	fmt.Println()

	// ── 证书加载 / 生成 ──
	// 证书存放在 %APPDATA%/web-rdp/ 下，与系统规范一致
	appDataDir, err := os.UserConfigDir()
	if err != nil {
		log.Fatalf("无法获取用户配置目录: %v", err)
	}
	appDir := filepath.Join(appDataDir, "web-rdp")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		log.Fatalf("无法创建应用数据目录 %s: %v", appDir, err)
	}
	certFile := filepath.Join(appDir, "cert.pem")
	keyFile := filepath.Join(appDir, "key.pem")

	cert, ok := loadCertFromDisk(certFile, keyFile)
	if !ok {
		var certPEM, keyPEM []byte
		cert, certPEM, keyPEM = generateSelfSignedCert()
		if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
			log.Printf("⚠ 无法保存证书: %v", err)
		}
		if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
			log.Printf("⚠ 无法保存私钥: %v", err)
		}
		fmt.Printf("→ 自签名证书已保存 (%s)\n", appDir)
	}

	if ffmpegArg != "" {
		ffmpegPath = ffmpegArg
		hasDDAGrab = checkDDAGrab(ffmpegArg)
		useFFmpeg = true
		fmt.Printf("使用指定 ffmpeg: %s\n", ffmpegArg)
	}

	_, _, _ = procSetProcessDPIAware.Call() // 设置进程 DPI 感知，避免高 DPI 下坐标偏移
	initHTTPClient(proxy)
	if ffmpegArg == "" {
		detectFFmpeg() // 自动检测或下载 ffmpeg
	}
	detectH264Encoder() // 按 GPU 品牌选择最优 H.264 编码器
	initWebRTC()        // 初始化 WebRTC（全局视频轨 + 信令管理）

	// ── 静态文件服务（嵌入的 HTML/JS/CSS）──
	sub, _ := fs.Sub(staticFS, "static")
	http.Handle("/", http.FileServer(http.FS(sub)))

	// ── WebSocket 端点（主通信通道）──
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handleWS(conn, r)
	})

	// ── HTTP 鼠标点击端点（低延迟点击，不走 WebSocket）──
	http.HandleFunc("/click", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// 权限检查：仅控制权持有者可以执行点击
		ip := r.RemoteAddr
		if idx := strings.LastIndex(ip, ":"); idx != -1 {
			ip = ip[:idx]
		}
		userName := userNameFor(ip)
		controlMu.Lock()
		owner := controlOwner
		controlMu.Unlock()
		if owner != "" && owner != userName {
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
		x, e1 := strconv.Atoi(r.URL.Query().Get("x"))
		y, e2 := strconv.Atoi(r.URL.Query().Get("y"))
		if e1 != nil || e2 != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		ix, iy := int32(x), int32(y)
		_, _, _ = procSetCursorPos.Call(uintptr(ix), uintptr(iy))
		_, _, _ = procMouseWait.Call(uintptr(0x0002), uintptr(ix), uintptr(iy), 0, 0) // LEFTDOWN
		time.Sleep(15 * time.Millisecond)
		_, _, _ = procMouseWait.Call(uintptr(0x0004), uintptr(ix), uintptr(iy), 0, 0) // LEFTUP
	})

	// 自定义 Server，过滤自签名证书导致的 TLS 握手噪音日志
	srv := &http.Server{
		ErrorLog: log.New(&tlsLogFilter{os.Stderr}, "", 0),
	}

	addr := fmt.Sprintf("%s:%d", listen, port)
	if useTLS {
		tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("监听失败: %v", err)
		}
		tlsLn := tls.NewListener(ln, tlsCfg)
		fmt.Printf("远控已启动 → https://%s （自签名证书，浏览器需手动信任）\n", addr)
		log.Fatal(srv.Serve(tlsLn))
	} else {
		fmt.Printf("远控已启动 → http://%s\n", addr)
		srv.Addr = addr
		log.Fatal(srv.ListenAndServe())
	}
}
