package main

import (
	"bufio"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
)

//go:embed views/index.html
var indexHTML string

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procMouseWait           = user32.NewProc("mouse_event")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
)

type RECT struct{ Left, Top, Right, Bottom int32 }

func init() { _, _, _ = procSetProcessDPIAware.Call() }

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

// ── ffmpeg gdigrab 单屏捕获 ──

type ffSession struct {
	cmd    *exec.Cmd
	stdout *bufio.Reader
	mu     sync.Mutex
}

func (f *ffSession) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cmd != nil {
		f.cmd.Process.Kill()
		f.cmd.Wait()
		f.cmd = nil
		f.stdout = nil
	}
}

func startFFmpeg(id, quality, maxW int) (*ffSession, int, int) {
	bounds := screenshot.GetDisplayBounds(id)
	primaryZoom := getScreenZoom(0) // 虚拟桌面坐标系始终按主屏 DPI 缩放

	physW := bounds.Dx()
	physH := bounds.Dy()
	capW := int(float64(physW) * primaryZoom)
	capH := int(float64(physH) * primaryZoom)
	capX := int(float64(bounds.Min.X) * primaryZoom)
	capY := int(float64(bounds.Min.Y) * primaryZoom)

	// 输出尺寸（初始 = 截图区域，可能被缩放缩小）
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
		"-f", "gdigrab", "-framerate", "60",
		"-offset_x", strconv.Itoa(capX),
		"-offset_y", strconv.Itoa(capY),
		"-video_size", fmt.Sprintf("%dx%d", capW, capH),
		"-i", "desktop",
		"-vf", vf,
		"-c:v", "mjpeg", "-q:v", strconv.Itoa(ffQ),
		"-f", "image2pipe", "pipe:1",
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = log.Writer()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, 0
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg 启动失败: %v", err)
		return nil, 0, 0
	}

	log.Printf("ffmpeg: screen=%d phys=%dx%d@(%d,%d) zoom=%.2f cap=%dx%d@(%d,%d) out=%dx%d",
		id, physW, physH, bounds.Min.X, bounds.Min.Y,
		primaryZoom, capW, capH, capX, capY, outW, outH)

	return &ffSession{
		cmd:    cmd,
		stdout: bufio.NewReaderSize(stdout, 256*1024),
	}, outW, outH
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
			ff        *ffSession
			ffScreen  = -1
			ffQ       = -1
			ffMW      = -1
			jpgBuf    = make([]byte, 0, 512*1024)
			frames    int
			totalEnc  time.Duration
			lastStats time.Time
		)

		for {
			id := int(currentID.Load())
			if id >= screenshot.NumActiveDisplays() {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			q := int(currentQuality.Load())
			mw := int(currentMaxW.Load())

			// 参数变了 → 重启 ffmpeg
			if ff == nil || ffScreen != id || ffQ != q || ffMW != mw {
				if ff != nil {
					ff.stop()
				}
				var outW, outH int
				ff, outW, outH = startFFmpeg(id, q, mw)
				if ff == nil {
					log.Printf("ffmpeg 启动失败，1秒后重试")
					time.Sleep(time.Second)
					continue
				}
				ffScreen = id
				ffQ = q
				ffMW = mw
				_ = outW
				_ = outH
			}

			// 从 ffmpeg stdout 读一帧 JPEG
			t0 := time.Now()
			jpg, err := readJPEG(ff.stdout, jpgBuf)
			if err != nil {
				log.Printf("ffmpeg 读取失败: %v", err)
				ff.stop()
				ff = nil
				ffScreen = -1
				continue
			}
			jpgBuf = jpg
			totalEnc += time.Since(t0)

			// 取元数据（用于前端点击坐标）
			bounds := screenshot.GetDisplayBounds(id)
			zoom := getScreenZoom(id)

			msg := encodeFrame(
				int32(bounds.Min.X), int32(bounds.Min.Y),
				int32(bounds.Dx()), int32(bounds.Dy()),
				zoom, jpg,
			)
			if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				ff.stop()
				return
			}

			frames++
			elapsed := time.Since(lastStats)
			if elapsed >= time.Second {
				fps := float64(frames) / elapsed.Seconds()
				avgMs := float64(totalEnc.Microseconds()) / float64(frames) / 1000
				stat := statsMsg{
					FPS:   math.Round(fps*10) / 10,
					EncMs: math.Round(avgMs*10) / 10,
					KB:    math.Round(float64(len(msg))/102.4) / 10,
					Q:     q, W: bounds.Dx(), H: bounds.Dy(),
				}
				if b, _ := json.Marshal(stat); b != nil {
					conn.WriteMessage(websocket.TextMessage, b)
				}
				frames = 0
				totalEnc = 0
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

	fmt.Println("远控已启动 → http://localhost:9000")
	log.Fatal(http.ListenAndServe(":9000", nil))
}
