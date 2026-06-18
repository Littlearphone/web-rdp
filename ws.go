package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
)

type wsMessage struct {
	msgType int
	data    []byte
}

var (
	userCounter atomic.Int32
	userMap     = make(map[string]string)
	userMapMu   sync.Mutex
)

func userNameFor(ip string) string {
	userMapMu.Lock()
	defer userMapMu.Unlock()
	if name, ok := userMap[ip]; ok {
		return name
	}
	name := fmt.Sprintf("用户%d", userCounter.Add(1))
	userMap[ip] = name
	return name
}

func handleWS(conn *websocket.Conn, r *http.Request) {
	var ff *ffSession
	var useH264 atomic.Bool // 默认 MJPEG，原子操作避免 data race
	var curScreen int = -1
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	userName := userNameFor(ip)

	defer func() {
		if ff != nil {
			releaseFFmpeg(curScreen)
		}
		_ = conn.Close()
	}()

	// ── 发送用户名 + 编码格式 ──
	format := "jpeg"
	if useH264.Load() {
		format = "h264"
	}
	if b, _ := json.Marshal(map[string]string{"user": userName, "format": format}); b != nil {
		_ = conn.WriteMessage(websocket.TextMessage, b)
	}

	// ── 控制消息接收 ──
	var currentID, currentQuality, currentMaxW atomic.Int32
	currentQuality.Store(75)

	readErr := make(chan struct{})
	go func() {
		defer close(readErr)
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
				if cm.Text != nil && *cm.Text != "" {
					doTypeText(*cm.Text)
				}
				if cm.Key != nil && cm.KeyDown != nil {
					doKey(*cm.Key, *cm.KeyDown)
				}
				if cm.RX != nil && cm.RY != nil {
					doRightClick(int32(*cm.RX), int32(*cm.RY))
				}
				if cm.MX != nil && cm.MY != nil {
					_, _, _ = procSetCursorPos.Call(uintptr(*cm.MX), uintptr(*cm.MY))
				}
				if cm.Webcodecs != nil {
					useH264.Store(*cm.Webcodecs && h264Encoder != "")
				}
				if cm.Control != nil {
					if *cm.Control {
						acquireControl(userName)
					} else {
						releaseControl(userName)
					}
				}
				if cm.DX1 != nil {
					doDrag(int32(*cm.DX1), int32(*cm.DY1), int32(*cm.DX2), int32(*cm.DY2))
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

	// ── 帧处理 ──
	var (
		ffScreen     = -1
		ffQ          = -1
		ffMW         = -1
		ffH264       = false
		goJpgBuf     bytes.Buffer
		frames       int
		totalWait    time.Duration
		lastStats    time.Time
		lastFrame    time.Time
		maxWait      time.Duration
		cachedBounds image.Rectangle
		cachedZoom   float64
		cacheFrame   int
		outCh        = make(chan wsMessage, 256) // 大缓冲 + H.264 阻塞发送避免丢帧
	)

	// 发送 goroutine（单写 WebSocket，单通道保证顺序）
	go func() {
		for msg := range outCh {
			_ = conn.WriteMessage(msg.msgType, msg.data)
		}
	}()

	for {
		id := int(currentID.Load())
		if id >= screenshot.NumActiveDisplays() {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		q, mw := int(currentQuality.Load()), int(currentMaxW.Load())

		if useFFmpeg {
			// ── ffmpeg 路径 ──
			h264 := useH264.Load()
			if ff == nil || ffScreen != id || ffQ != q || ffMW != mw || ffH264 != h264 {
				if ff != nil {
					releaseFFmpeg(curScreen)
				}
				ff = acquireFFmpeg(id, q, mw, h264)
				if ff == nil {
					log.Printf("ffmpeg 启动失败")
					time.Sleep(time.Second)
					continue
				}
				ffScreen = id
				curScreen = id
				ffQ = q
				ffMW = mw
				ffH264 = h264
				cacheFrame = 0
				f := "jpeg"
				if ffH264 {
					f = "h264"
				}
				log.Printf("编码格式切换 → %s", f)
				if b, _ := json.Marshal(map[string]string{"format": f}); b != nil {
					outCh <- wsMessage{websocket.TextMessage, b}
				}
			}

			var data []byte
			select {
			case data = <-ff.frameCh:
				if data == nil {
					ff = nil
					ffScreen = -1
					curScreen = -1
					continue
				}
			case <-readErr:
				return
			case <-time.After(5 * time.Second):
				log.Printf("ffmpeg 无帧")
				releaseFFmpeg(curScreen)
				ff = nil
				ffScreen = -1
				curScreen = -1
				continue
			}

			now := time.Now()
			if !lastFrame.IsZero() {
				w := now.Sub(lastFrame)
				totalWait += w
				if w > maxWait {
					maxWait = w
				}
			}
			lastFrame = now

			if cacheFrame <= 0 || ffScreen != id {
				cachedBounds = screenshot.GetDisplayBounds(id)
				cachedZoom = getScreenZoom(id)
				cacheFrame = 30
			}
			cacheFrame--

			// MJPEG 需要 24B 头，H.264 裸发
			if !ffH264 {
				data = encodeFrame(int32(cachedBounds.Min.X), int32(cachedBounds.Min.Y),
					int32(cachedBounds.Dx()), int32(cachedBounds.Dy()), cachedZoom, data)
			}
			if ffH264 {
				outCh <- wsMessage{websocket.BinaryMessage, data} // H.264 阻塞：参考帧不丢
			} else {
				select {
				case outCh <- wsMessage{websocket.BinaryMessage, data}:
				default:
				}
			}

			frames++
			if elapsed := time.Since(lastStats); elapsed >= time.Second {
				fps := float64(frames) / elapsed.Seconds()
				maxW := float64(maxWait.Microseconds()) / 1000
				stat := statsMsg{
					FPS: math.Round(fps*10) / 10, EncMs: math.Round(maxW*10) / 10,
					KB: math.Round(float64(len(data))/102.4) / 10, Owner: controlOwner,
					Ox: cachedBounds.Min.X, Oy: cachedBounds.Min.Y, Zoom: cachedZoom,
					Q: q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
					Screens: screenshot.NumActiveDisplays(),
				}
				if b, _ := json.Marshal(stat); b != nil {
					select {
					case outCh <- wsMessage{websocket.TextMessage, b}:
					default:
						{
						}
					}
				}
				frames, totalWait, maxWait, lastStats = 0, 0, 0, time.Now()
			}
			continue
		}

		// ── 纯 Go 回退 ──
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
		_ = jpeg.Encode(&goJpgBuf, img, &jpeg.Options{Quality: q})
		msg := encodeFrame(int32(cachedBounds.Min.X), int32(cachedBounds.Min.Y),
			int32(cachedBounds.Dx()), int32(cachedBounds.Dy()), cachedZoom, goJpgBuf.Bytes())
		select {
		case outCh <- wsMessage{websocket.BinaryMessage, msg}:
		default:
			{
			}
		}

		now := time.Now()
		if !lastFrame.IsZero() {
			w := now.Sub(lastFrame)
			totalWait += w
			if w > maxWait {
				maxWait = w
			}
		}
		lastFrame = now

		frames++
		if elapsed := time.Since(lastStats); elapsed >= time.Second {
			fps := float64(frames) / elapsed.Seconds()
			maxW := float64(maxWait.Microseconds()) / 1000
			stat := statsMsg{
				FPS: math.Round(fps*10) / 10, EncMs: math.Round(maxW*10) / 10,
				KB: math.Round(float64(len(msg))/102.4) / 10, Owner: controlOwner,
				Ox: cachedBounds.Min.X, Oy: cachedBounds.Min.Y, Zoom: cachedZoom,
				Q: q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
				Screens: screenshot.NumActiveDisplays(),
			}
			if b, _ := json.Marshal(stat); b != nil {
				select {
				case outCh <- wsMessage{websocket.TextMessage, b}:
				default:
					{
					}
				}
			}
			frames, totalWait, maxWait, lastStats = 0, 0, 0, time.Now()
		}
	}
}
