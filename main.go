package main

import (
	_ "embed"
	"flag"
	"fmt"
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

//go:embed views/index.html
var indexHTML string

var httpClient *http.Client

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

var (
	controlOwner            string
	controlMu               sync.Mutex
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procMouseWait           = user32.NewProc("mouse_event")
	procKeybdWait           = user32.NewProc("keybd_event")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
)

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

func doTypeText(text string) {
	for _, r := range text {
		vk := uintptr(r)
		_, _, _ = procKeybdWait.Call(vk, 0, 0, 0)      // down
		_, _, _ = procKeybdWait.Call(vk, 0, 0x0002, 0) // up
	}
}

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

func doRightClick(x, y int32) {
	ix, iy := uintptr(x), uintptr(y)
	_, _, _ = procSetCursorPos.Call(ix, iy)
	time.Sleep(30 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0008, ix, iy, 0, 0) // RIGHTDOWN
	time.Sleep(50 * time.Millisecond)
	_, _, _ = procMouseWait.Call(0x0010, ix, iy, 0, 0) // RIGHTUP
}

func acquireControl(user string) bool {
	controlMu.Lock()
	defer controlMu.Unlock()
	if controlOwner == "" || controlOwner == user {
		controlOwner = user
		return true
	}
	return false
}
func releaseControl(user string) {
	controlMu.Lock()
	defer controlMu.Unlock()
	if controlOwner == user {
		controlOwner = ""
	}
}
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

type RECT struct{ Left, Top, Right, Bottom int32 }

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type ctrlMsg struct {
	Control *bool   `json:"control,omitempty"`
	Screen  *int    `json:"screen,omitempty"`
	Quality *int    `json:"quality,omitempty"`
	MaxW    *int    `json:"maxw,omitempty"`
	Key     *string `json:"key,omitempty"`
	KeyDown *bool   `json:"down,omitempty"`
	Text    *string `json:"text,omitempty"`
	RX      *int    `json:"rx,omitempty"`
	RY      *int    `json:"ry,omitempty"`
	DX1     *int    `json:"dx1,omitempty"`
	DY1     *int    `json:"dy1,omitempty"`
	DX2     *int    `json:"dx2,omitempty"`
	DY2     *int    `json:"dy2,omitempty"`
}

type statsMsg struct {
	Owner   string  `json:"owner"`
	FPS     float64 `json:"fps"`
	EncMs   float64 `json:"enc_ms"`
	KB      float64 `json:"kb"`
	Q       int     `json:"q"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Screens int     `json:"screens"`
}

func main() {
	var (
		proxy     string
		port      int
		listen    string
		ffmpegArg string
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
		hasDXGI = checkDXGI(ffmpegArg)
		useFFmpeg = true
		fmt.Printf("使用指定 ffmpeg: %s\n", ffmpegArg)
	}

	_, _, _ = procSetProcessDPIAware.Call()
	initHTTPClient(proxy)
	if ffmpegArg == "" {
		detectFFmpeg()
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(indexHTML))
	})

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handleWS(conn, r)
	})

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
