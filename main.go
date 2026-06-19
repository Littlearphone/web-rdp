package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

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
	procSetCursorPos        = user32.NewProc("SetCursorPos")        // 设置光标位置
	procMouseWait           = user32.NewProc("mouse_event")         // 鼠标事件（点击/移动）
	procKeybdWait           = user32.NewProc("keybd_event")         // 键盘事件（按下/释放）
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors") // 枚举显示器
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")  // 设置 DPI 感知
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
func doRightClick(x, y int32) {
	ix, iy := uintptr(x), uintptr(y)
	_, _, _ = procSetCursorPos.Call(ix, iy)
	time.Sleep(30 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0008, ix, iy, 0, 0) // RIGHTDOWN
	time.Sleep(50 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0010, ix, iy, 0, 0) // RIGHTUP
}

// acquireControl 尝试获取远程控制权。同一用户可重复获取；其他用户需等待当前持有者释放。
// 返回 true 表示获取成功，false 表示被其他用户占用
func acquireControl(user string) bool {
	controlMu.Lock()
	defer controlMu.Unlock()
	if controlOwner == "" || controlOwner == user {
		controlOwner = user
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

// doDrag 模拟鼠标拖拽操作：从 (x1,y1) 按下左键并移动到 (x2,y2) 后释放
func doDrag(x1, y1, x2, y2 int32) {
	ix1, iy1 := uintptr(x1), uintptr(y1)
	ix2, iy2 := uintptr(x2), uintptr(y2)
	_, _, _ = procSetCursorPos.Call(ix1, iy1)
	time.Sleep(30 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0002, ix1, iy1, 0, 0) // LEFTDOWN
	time.Sleep(30 * time.Millisecond)
	_, _, _ = procSetCursorPos.Call(ix2, iy2) // move
	time.Sleep(30 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0004, ix2, iy2, 0, 0) // LEFTUP
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
	Control   *bool   `json:"control,omitempty"`
	Screen    *int    `json:"screen,omitempty"`
	Quality   *int    `json:"quality,omitempty"`
	MaxW      *int    `json:"maxw,omitempty"`
	Key       *string `json:"key,omitempty"`
	KeyDown   *bool   `json:"down,omitempty"`
	Text      *string `json:"text,omitempty"`
	RX        *int    `json:"rx,omitempty"`
	RY        *int    `json:"ry,omitempty"`
	DX1       *int    `json:"dx1,omitempty"`
	DY1       *int    `json:"dy1,omitempty"`
	DX2       *int    `json:"dx2,omitempty"`
	DY2       *int    `json:"dy2,omitempty"`
	MX        *int    `json:"mx,omitempty"`
	MY        *int    `json:"my,omitempty"`
	Webcodecs *bool   `json:"webcodecs,omitempty"`
}

// statsMsg 定义性能统计消息，每秒由后端推送到前端用于状态栏展示
type statsMsg struct {
	Owner   string  `json:"owner"`
	FPS     float64 `json:"fps"`
	EncMs   float64 `json:"enc_ms"`
	KB      float64 `json:"kb"`
	Q       int     `json:"q"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Ox      int     `json:"ox"`
	Oy      int     `json:"oy"`
	Zoom    float64 `json:"zoom"`
	Screens int     `json:"screens"`
}

// main 是程序入口，负责解析命令行参数、初始化组件并启动 HTTP 服务器。
// 主要流程：解析参数 → 设置 DPI → 初始化 HTTP 客户端 → 检测 ffmpeg → 检测编码器 → 启动服务
func main() {
	var (
		proxy     string // HTTP 代理地址
		port      int    // 监听端口
		listen    string // 监听地址
		ffmpegArg string // 手动指定的 ffmpeg 路径
	)
	flag.StringVar(&proxy, "proxy", "", "HTTP 代理地址 (用于下载 ffmpeg)")
	flag.IntVar(&port, "port", 9000, "监听端口")
	flag.StringVar(&listen, "listen", "", "监听地址 (默认所有网卡)")
	flag.StringVar(&ffmpegArg, "ffmpeg", "", "手动指定 ffmpeg.exe 路径")
	flag.Usage = func() {
		o := flag.CommandLine.Output()
		fmt.Fprintf(o, "Web 远程控制 v1.0\n\n用法: %s [选项]\n\n选项:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprint(o, "\n示例:\n  web-rdp.exe                                    默认 :9000\n  web-rdp.exe -port 8080                          指定端口\n  web-rdp.exe -listen 127.0.0.1                   仅本机\n  web-rdp.exe -ffmpeg C:\\tools\\ffmpeg.exe         手动指定 ffmpeg\n  web-rdp.exe -proxy :7890                        走代理下载\n")
	}
	flag.Parse()

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
		x, e1 := strconv.Atoi(r.URL.Query().Get("x"))
		y, e2 := strconv.Atoi(r.URL.Query().Get("y"))
		if e1 != nil || e2 != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		ix, iy := int32(x), int32(y)
		_, _, _ = procSetCursorPos.Call(uintptr(ix), uintptr(iy))
		time.Sleep(30 * time.Millisecond)
		_, _, _ = procMouseWait.Call(uintptr(0x0002), uintptr(ix), uintptr(iy), 0, 0)
		time.Sleep(50 * time.Millisecond)
		_, _, _ = procMouseWait.Call(uintptr(0x0004), uintptr(ix), uintptr(iy), 0, 0)
	})

	addr := fmt.Sprintf("%s:%d", listen, port)
	fmt.Printf("远控已启动 → http://%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
