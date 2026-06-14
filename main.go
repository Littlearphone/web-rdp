package main

import (
	_ "embed"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
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
		proxyURL, err := url.Parse("http://" + proxy)
		if err == nil {
			tr.Proxy = http.ProxyURL(proxyURL)
			fmt.Printf("使用代理: %s\n", proxy)
		}
	}
	httpClient = &http.Client{Timeout: 5 * time.Minute, Transport: tr}
}

// ── Windows API ──

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procMouseWait           = user32.NewProc("mouse_event")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
)

type RECT struct{ Left, Top, Right, Bottom int32 }

// ── 缩放比缓存 ──

var (
	zoomCache   = make(map[int]float64)
	zoomCacheMu sync.RWMutex
)

func getScreenZoom(id int) float64 {
	zoomCacheMu.RLock()
	if z, ok := zoomCache[id]; ok {
		zoomCacheMu.RUnlock()
		return z
	}
	zoomCacheMu.RUnlock()
	var lw int32
	idx := 0
	cb := syscall.NewCallback(func(_ uintptr, _ uintptr, rc *RECT, _ uintptr) uintptr {
		if idx == id {
			lw = rc.Right - rc.Left
		}
		idx++
		return 1
	})
	_, _, _ = procEnumDisplayMonitors.Call(0, 0, cb, 0)
	b := screenshot.GetDisplayBounds(id)
	z := 1.0
	if pw := b.Dx(); pw > 0 && lw > 0 {
		z = float64(lw) / float64(pw)
	}
	zoomCacheMu.Lock()
	zoomCache[id] = z
	zoomCacheMu.Unlock()
	return z
}

func encodeFrame(ox, oy, pw, ph int32, zoom float64, jpg []byte) []byte {
	buf := make([]byte, 24+len(jpg))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(ox))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(oy))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(pw))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(ph))
	binary.LittleEndian.PutUint64(buf[16:24], math.Float64bits(zoom))
	copy(buf[24:], jpg)
	return buf
}

func downscale(img *image.RGBA, maxW int) *image.RGBA {
	if maxW <= 0 || img.Bounds().Dx() <= maxW {
		return img
	}
	newW := maxW
	newH := img.Bounds().Dy() * maxW / img.Bounds().Dx()
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type ctrlMsg struct {
	Screen  *int `json:"screen,omitempty"`
	Quality *int `json:"quality,omitempty"`
	MaxW    *int `json:"maxw,omitempty"`
}

type statsMsg struct {
	FPS   float64 `json:"fps"`
	EncMs float64 `json:"enc_ms"`
	KB    float64 `json:"kb"`
	Q     int     `json:"q"`
	W     int     `json:"w"`
	H     int     `json:"h"`
}

func main() {
	var (
		proxy  string
		port   int
		listen string
	)
	flag.StringVar(&proxy, "proxy", "", "HTTP 代理地址 (用于下载 ffmpeg)")
	flag.IntVar(&port, "port", 9000, "监听端口")
	flag.StringVar(&listen, "listen", "", "监听地址 (默认所有地址，可指定 127.0.0.1)")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(out, `Web 远程控制 v1.0 — 通过浏览器远程控制 Windows 桌面

用法:
  %s [-listen <IP>] [-port <端口>] [-proxy <代理>]

参数说明:
  -listen string
        监听的 IP 地址
        · 不填（默认）：监听所有网络接口（局域网可访问）
        · 0.0.0.0      ：显式监听所有接口（同上）
        · 127.0.0.1    ：仅本机可访问
        · 192.168.1.x  ：仅监听指定网卡

  -port int
        监听的端口号 (默认: 9000)
        · 1-1023 需要管理员权限
        · 建议使用 1024-65535 之间的端口

  -proxy string
        HTTP 代理地址，用于下载 ffmpeg
        · 格式: IP:端口 或 :端口（默认 localhost）
        · 示例: -proxy 127.0.0.1:7890  或  -proxy :7890

示例:
  %-40s  默认配置
  %-40s  指定端口
  %-40s  仅本机访问
  %-40s  局域网分享
  %-40s  走代理下载 ffmpeg
`, os.Args[0],
			"web-rdp.exe",
			"web-rdp.exe -port 8080",
			"web-rdp.exe -listen 127.0.0.1",
			"web-rdp.exe -listen 0.0.0.0 -port 9000",
			"web-rdp.exe -proxy :7890")
	}
	flag.Parse()

	_, _, _ = procSetProcessDPIAware.Call()
	initHTTPClient(proxy)
	detectFFmpeg()

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
