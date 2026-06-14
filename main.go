package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// ── ffmpeg 发现 ──

const ffmpegLocalDir = "ffmpeg_local"
const ffmpegReleaseAPI = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/latest"

var (
	ffmpegPath string // ffmpeg 可执行文件路径，空 = 不可用
	hasDXGI    bool   // 是否支持 dxgigrab
	useFFmpeg  bool   // 用户是否选择使用 ffmpeg
)

func detectFFmpeg() {
	// 1. 优先检查本地目录
	local := filepath.Join(ffmpegLocalDir, "bin", "ffmpeg.exe")
	if _, err := os.Stat(local); err == nil {
		ffmpegPath = local
		hasDXGI = checkDXGI(local)
		useFFmpeg = true
		return
	}

	// 2. 检查系统 PATH
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpegPath = p
		hasDXGI = checkDXGI(p)
		useFFmpeg = true

		if !hasDXGI {
			if askYN("当前 ffmpeg 不支持 dxgigrab，下载优化版？") {
				downloadAndExtract()
				if _, err := os.Stat(local); err == nil {
					ffmpegPath = local
					hasDXGI = checkDXGI(local)
				}
			}
		}
		return
	}

	// 3. 完全没找到 ffmpeg
	if askYN("未找到 ffmpeg，自动下载？") {
		downloadAndExtract()
		if _, err := os.Stat(local); err == nil {
			ffmpegPath = local
			hasDXGI = checkDXGI(local)
			useFFmpeg = true
			return
		}
	}
	useFFmpeg = false
	fmt.Println("→ 使用纯 Go 截图方案（无 ffmpeg）")
}

func askYN(prompt string) bool {
	fmt.Printf("\n⚠ %s [Y/n]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "" || line == "y" || line == "yes"
}

func resolveDownloadURL() string {
	resp, err := httpClient.Get(ffmpegReleaseAPI)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&release) != nil {
		return ""
	}
	for _, a := range release.Assets {
		if strings.Contains(a.Name, "win64") && strings.Contains(a.Name, "gpl") && strings.HasSuffix(a.Name, ".zip") {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func checkDXGI(path string) bool {
	out, err := exec.Command(path, "-hide_banner", "-devices").Output()
	return err == nil && strings.Contains(string(out), "dxgigrab")
}

func downloadAndExtract() {
	tmpFile := filepath.Join(os.TempDir(), "ffmpeg_download.zip")
	defer os.Remove(tmpFile)

	for attempt := 1; ; attempt++ {
		// ── 解析下载地址 ──
		dlURL := resolveDownloadURL()
		if dlURL == "" {
			fmt.Println("  无法获取下载地址，请检查网络或手动安装 ffmpeg")
			return
		}

		// ── 下载 ──
		fmt.Printf("下载 ffmpeg... (约 120MB)\n")
		resp, err := httpClient.Get(dlURL)
		if err != nil {
			fmt.Printf("  下载失败: %v\n", err)
			if askYN("重试下载？") {
				continue
			}
			return
		}

		f, _ := os.Create(tmpFile)
		totalSize := resp.ContentLength
		downloaded := int64(0)
		startTime := time.Now()
		buf := make([]byte, 64*1024)
		lastReport := time.Now()

		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				f.Write(buf[:n])
				downloaded += int64(n)
				// 每 200ms 刷新进度
				if totalSize > 0 && time.Since(lastReport) > 200*time.Millisecond {
					elapsed := time.Since(startTime).Seconds()
					speed := float64(downloaded) / elapsed / 1024 / 1024 // MB/s
					pct := downloaded * 100 / totalSize
					eta := ""
					if speed > 0 {
						remaining := float64(totalSize-downloaded) / (speed * 1024 * 1024)
						eta = fmt.Sprintf(" 剩余 %ds", int(remaining))
					}
					fmt.Printf("\r  %d%%  %.1f MB/s%s    ", pct, speed, eta)
					lastReport = time.Now()
				} else if totalSize <= 0 {
					mb := float64(downloaded) / 1024 / 1024
					fmt.Printf("\r  已下载 %.1f MB    ", mb)
				}
			}
			if readErr != nil {
				break
			}
		}
		f.Close()
		resp.Body.Close()

		if totalSize > 0 && downloaded < totalSize {
			fmt.Printf("\n  下载不完整 (%d/%d 字节)\n", downloaded, totalSize)
			if askYN("重试下载？") {
				continue
			}
			return
		}
		fmt.Printf("\n  下载完成 (%.1f MB)\n", float64(downloaded)/1024/1024)

		// ── 解压 ──
		fmt.Println("解压...")
		os.RemoveAll(ffmpegLocalDir)

		zr, err := zip.OpenReader(tmpFile)
		if err != nil {
			fmt.Printf("  解压失败: %v\n", err)
			if askYN("重试下载？") {
				continue
			}
			return
		}

		extractOK := true
		for _, zf := range zr.File {
			parts := strings.SplitN(zf.Name, "/", 2)
			if len(parts) < 2 || parts[1] == "" {
				continue
			}
			target := filepath.Join(ffmpegLocalDir, parts[1])
			if zf.FileInfo().IsDir() {
				os.MkdirAll(target, 0755)
				continue
			}
			os.MkdirAll(filepath.Dir(target), 0755)
			rc, err := zf.Open()
			if err != nil {
				extractOK = false
				break
			}
			out, err := os.Create(target)
			if err != nil {
				rc.Close()
				extractOK = false
				break
			}
			io.Copy(out, rc)
			rc.Close()
			out.Close()
		}
		zr.Close()

		if !extractOK {
			fmt.Println("  解压失败")
			os.RemoveAll(ffmpegLocalDir)
			if askYN("重试下载？") {
				continue
			}
			return
		}

		fmt.Printf("→ ffmpeg 就绪 (ffmpeg_local/)  重试 %d 次\n", attempt-1)
		return
	}
}

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

// ── ffmpeg 管线 ──

type ffSession struct {
	cmd     *exec.Cmd
	stdout  *bufio.Reader
	frameCh chan []byte
	stopCh  chan struct{}
}

func (f *ffSession) stop() {
	if f.cmd != nil {
		close(f.stopCh)
		f.cmd.Process.Kill()
		f.cmd.Wait()
	}
}

func startFFmpeg(id, quality, maxW int) (*ffSession, int, int) {
	bounds := screenshot.GetDisplayBounds(id)
	physW := bounds.Dx()
	physH := bounds.Dy()

	var capX, capY, capW, capH int
	var device string

	if hasDXGI {
		device = "dxgigrab"
		capX = bounds.Min.X
		capY = bounds.Min.Y
		capW = physW
		capH = physH
	} else {
		z := getScreenZoom(0)
		device = "gdigrab"
		capX = int(float64(bounds.Min.X) * z)
		capY = int(float64(bounds.Min.Y) * z)
		capW = int(float64(physW) * z)
		capH = int(float64(physH) * z)
	}

	outW := capW
	outH := capH

	ffQ := 32 - (quality-1)*31/99
	if ffQ < 1 {
		ffQ = 1
	}
	if ffQ > 31 {
		ffQ = 31
	}

	vf := "null"
	if maxW > 0 && capW > maxW {
		outH = capH * maxW / capW
		outW = maxW
		vf = fmt.Sprintf("scale=%d:%d:flags=fast_bilinear", outW, outH)
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-sws_flags", "fast_bilinear",
		"-f", device, "-framerate", "60", // 最大 60fps，超过会导致 CPU 100% 卡死
		"-offset_x", strconv.Itoa(capX),
		"-offset_y", strconv.Itoa(capY),
		"-video_size", fmt.Sprintf("%dx%d", capW, capH),
		"-i", "desktop",
		"-vf", vf,
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(ffQ),
		"-f", "image2pipe", "pipe:1",
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stderr = log.Writer()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, 0
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg 启动失败: %v", err)
		return nil, 0, 0
	}

	log.Printf("ffmpeg: %s screen=%d phys=%dx%d@(%d,%d) cap=%dx%d@(%d,%d) out=%dx%d",
		device, id, physW, physH, bounds.Min.X, bounds.Min.Y,
		capW, capH, capX, capY, outW, outH)

	ff := &ffSession{
		cmd:     cmd,
		stdout:  bufio.NewReaderSize(stdout, 256*1024),
		frameCh: make(chan []byte, 1),
		stopCh:  make(chan struct{}),
	}

	go func() {
		buf := make([]byte, 0, 512*1024)
		for {
			select {
			case <-ff.stopCh:
				return
			default:
			}
			jpg, err := readJPEG(ff.stdout, buf)
			if err != nil {
				return
			}
			buf = jpg
			frame := make([]byte, len(jpg))
			copy(frame, jpg)
			select {
			case ff.frameCh <- frame:
			default:
				<-ff.frameCh
				ff.frameCh <- frame
			}
		}
	}()

	return ff, outW, outH
}

func readJPEG(br *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	prev := byte(0)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if prev == 0xFF && b == 0xD8 {
			buf = append(buf, 0xFF, 0xD8)
			break
		}
		prev = b
	}
	prev = 0
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		buf = append(buf, b)
		if prev == 0xFF && b == 0xD9 {
			break
		}
		prev = b
	}
	return buf, nil
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
		fmt.Fprintf(out, `Web 远程控制 v1.0 — 通过浏览器远程控制 Windows 桌面

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
		w.Write([]byte(indexHTML))
	})

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var currentID atomic.Int32
		var currentQuality atomic.Int32
		var currentMaxW atomic.Int32
		currentQuality.Store(75)
		currentMaxW.Store(0)

		go func() {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var cm ctrlMsg
				if json.Unmarshal(msg, &cm) == nil {
					if cm.Screen != nil && *cm.Screen >= 0 && *cm.Screen < screenshot.NumActiveDisplays() {
						currentID.Store(int32(*cm.Screen))
					}
					if cm.Quality != nil && *cm.Quality >= 10 && *cm.Quality <= 100 {
						currentQuality.Store(int32(*cm.Quality))
					}
					if cm.MaxW != nil {
						currentMaxW.Store(int32(*cm.MaxW))
					}
					continue
				}
				id, err := strconv.Atoi(string(msg))
				if err != nil || id < 0 || id >= screenshot.NumActiveDisplays() {
					continue
				}
				currentID.Store(int32(id))
			}
		}()

		var (
			ff           *ffSession
			ffScreen     = -1
			ffQ          = -1
			ffMW         = -1
			goJpgBuf     bytes.Buffer
			frames       int
			totalWait    time.Duration
			lastStats    time.Time
			lastFrame    time.Time
			maxWait      time.Duration
			cachedBounds image.Rectangle
			cachedZoom   float64
			cacheFrame   int
			sendCh       = make(chan []byte, 3) // 帧
			statCh       = make(chan []byte, 1) // stats
		)

		// 唯一写 goroutine：一个协程写 WebSocket，无需锁
		go func() {
			for {
				select {
				case msg := <-sendCh:
					conn.WriteMessage(websocket.BinaryMessage, msg)
				case s := <-statCh:
					conn.WriteMessage(websocket.TextMessage, s)
				}
			}
		}()

		for {
			id := int(currentID.Load())
			if id >= screenshot.NumActiveDisplays() {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			q := int(currentQuality.Load())
			mw := int(currentMaxW.Load())

			// ffmpeg 路径
			if useFFmpeg {
				if ff == nil || ffScreen != id || ffQ != q || ffMW != mw {
					if ff != nil {
						ff.stop()
					}
					var outW, outH int
					ff, outW, outH = startFFmpeg(id, q, mw)
					if ff == nil {
						log.Printf("ffmpeg 启动失败")
						time.Sleep(time.Second)
						continue
					}
					ffScreen = id
					ffQ = q
					ffMW = mw
					_ = outW
					_ = outH
				}

				var jpg []byte
				select {
				case jpg = <-ff.frameCh:
				case <-time.After(5 * time.Second):
					log.Printf("ffmpeg 5 秒无帧，重启")
					ff.stop()
					ff = nil
					ffScreen = -1
					continue
				}

				now := time.Now()
				if !lastFrame.IsZero() {
					wait := now.Sub(lastFrame)
					totalWait += wait
					if wait > maxWait {
						maxWait = wait
					}
				}
				lastFrame = now

				// 缓存 bounds/zoom，每 30 帧刷新一次（避免 Win32 API 偶尔卡）
				if cacheFrame <= 0 || ffScreen != id {
					cachedBounds = screenshot.GetDisplayBounds(id)
					cachedZoom = getScreenZoom(id)
					cacheFrame = 30
				}
				cacheFrame--

				msg := encodeFrame(
					int32(cachedBounds.Min.X), int32(cachedBounds.Min.Y),
					int32(cachedBounds.Dx()), int32(cachedBounds.Dy()),
					cachedZoom, jpg,
				)

				select {
				case sendCh <- msg:
				default:
				}

				frames++
				elapsed := time.Since(lastStats)
				if elapsed >= time.Second {
					fps := float64(frames) / elapsed.Seconds()
					maxW := float64(maxWait.Microseconds()) / 1000
					stat := statsMsg{
						FPS:   math.Round(fps*10) / 10,
						EncMs: math.Round(maxW*10) / 10,
						KB:    math.Round(float64(len(msg))/102.4) / 10,
						Q:     q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
					}
					if b, _ := json.Marshal(stat); b != nil {
						select {
						case statCh <- b:
						default:
						}
					}
					frames = 0
					totalWait = 0
					maxWait = 0
					lastStats = time.Now()
				}
				continue
			}

			// ── 纯 Go 回退路径 ──
			img, err := screenshot.CaptureDisplay(id)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if cacheFrame <= 0 {
				cachedBounds = screenshot.GetDisplayBounds(id)
				cachedZoom = getScreenZoom(id)
				cacheFrame = 30
			}
			cacheFrame--
			img = downscale(img, mw)

			goJpgBuf.Reset()
			jpeg.Encode(&goJpgBuf, img, &jpeg.Options{Quality: q})

			msg := encodeFrame(
				int32(cachedBounds.Min.X), int32(cachedBounds.Min.Y),
				int32(cachedBounds.Dx()), int32(cachedBounds.Dy()),
				cachedZoom, goJpgBuf.Bytes(),
			)
			select {
			case sendCh <- msg:
			default:
			}

			now := time.Now()
			if !lastFrame.IsZero() {
				wait := now.Sub(lastFrame)
				totalWait += wait
				if wait > maxWait {
					maxWait = wait
				}
			}
			lastFrame = now

			frames++
			elapsed := time.Since(lastStats)
			if elapsed >= time.Second {
				fps := float64(frames) / elapsed.Seconds()
				maxW := float64(maxWait.Microseconds()) / 1000
				stat := statsMsg{
					FPS:   math.Round(fps*10) / 10,
					EncMs: math.Round(maxW*10) / 10,
					KB:    math.Round(float64(len(msg))/102.4) / 10,
					Q:     q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
				}
				if b, _ := json.Marshal(stat); b != nil {
					select {
					case statCh <- b:
					default:
					}
				}
				frames = 0
				totalWait = 0
				maxWait = 0
				lastStats = time.Now()
			}
		}
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
