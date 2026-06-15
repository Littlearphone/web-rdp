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
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procMouseWait           = user32.NewProc("mouse_event")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
)

type RECT struct{ Left, Top, Right, Bottom int32 }

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type ctrlMsg struct {
	Screen  *int `json:"screen,omitempty"`
	Quality *int `json:"quality,omitempty"`
	MaxW    *int `json:"maxw,omitempty"`
}

type statsMsg struct {
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
		handleWS(conn)
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
