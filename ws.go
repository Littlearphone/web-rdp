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

// wsMessage 封装一条待发送的 WebSocket 消息（类型 + 数据）
type wsMessage struct {
	msgType int    // websocket.TextMessage 或 websocket.BinaryMessage
	data    []byte // 消息负载
}

// ── 用户管理 ──
var (
	userCounter atomic.Int32              // 用户计数器，用于生成默认用户名
	userMap     = make(map[string]string) // IP → 用户名映射
	userMapMu   sync.Mutex                // 用户映射的互斥锁
)

// userNameFor 根据客户端 IP 获取或分配用户名。同一 IP 多次连接返回相同用户名
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

// handleWS 处理单个 WebSocket 连接的生命周期。
// 启动后持续读取控制消息并截屏推流，支持 MJPEG / H.264 双编码格式动态切换。
// 当连接断开或 ffmpeg 异常退出时自动清理资源。
func handleWS(conn *websocket.Conn, r *http.Request) {
	var ff *ffSession
	var useH264 atomic.Bool // H.264 优先，原子操作避免 data race
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
		// 断开时释放该用户的控制权
		releaseControl(userName)
		closeActiveDialog()
		_ = conn.Close()
	}()

	// ── 发送用户名 + 编码格式 ──
	useH264.Store(currentH264Encoder() != "") // 有可用 H.264 编码器则默认启用
	format := "jpeg"
	if useH264.Load() {
		format = "h264"
	}
	if b, _ := json.Marshal(map[string]string{"user": userName, "format": format}); b != nil {
		_ = conn.WriteMessage(websocket.TextMessage, b)
	}

	// ── 输出通道（提前创建，供 reader goroutine 发送状态消息）──
	outCh := make(chan wsMessage, 256)

	// 发送 goroutine（单写 WebSocket，单通道保证顺序）
	go func() {
		for msg := range outCh {
			_ = conn.WriteMessage(msg.msgType, msg.data)
		}
	}()

	// sendStatus 是一个便捷函数，向客户端发送 JSON 状态消息
	sendStatus := func(status, msg string) {
		b, _ := json.Marshal(map[string]string{"control_status": status, "control_msg": msg})
		if b != nil {
			outCh <- wsMessage{websocket.TextMessage, b}
		}
	}

	// ── 控制消息接收 ──
	var currentID, currentQuality, currentMaxW, currentFPS atomic.Int32
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
				if cm.Fps != nil && *cm.Fps >= 0 {
					currentFPS.Store(int32(*cm.Fps))
				}
				if cm.Text != nil && *cm.Text != "" && hasControl(userName) {
					doTypeText(*cm.Text)
				}
				if cm.Key != nil && cm.KeyDown != nil && hasControl(userName) {
					doKey(*cm.Key, *cm.KeyDown)
				}
				if cm.RX != nil && cm.RY != nil && hasControl(userName) {
					doRightClick(int32(*cm.RX), int32(*cm.RY))
				}
				if cm.MX != nil && cm.MY != nil && hasControl(userName) {
					_, _, _ = procSetCursorPos.Call(uintptr(*cm.MX), uintptr(*cm.MY))
				}
				if cm.MouseBtn != nil && cm.MouseDn != nil && hasControl(userName) {
					switch *cm.MouseBtn {
					case "left":
						if *cm.MouseDn {
							_, _, _ = procMouseWait.Call(0x0002, 0, 0, 0, 0) // LEFTDOWN
						} else {
							_, _, _ = procMouseWait.Call(0x0004, 0, 0, 0, 0) // LEFTUP
						}
					case "right":
						if *cm.MouseDn {
							_, _, _ = procMouseWait.Call(0x0008, 0, 0, 0, 0) // RIGHTDOWN
						} else {
							_, _, _ = procMouseWait.Call(0x0010, 0, 0, 0, 0) // RIGHTUP
						}
					}
				}
				if cm.Webcodecs != nil {
					useH264.Store(*cm.Webcodecs && currentH264Encoder() != "")
				}
				if cm.Control != nil {
					if *cm.Control {
						// ── 异步权限请求流程 ──
						status := requestControl(userName)
						switch status {
						case "granted":
							sendStatus("granted", "")
						case "denied":
							sendStatus("denied", "宿主已永久拒绝您的控制请求")
						case "busy":
							controlMu.Lock()
							owner := controlOwner
							controlMu.Unlock()
							sendStatus("busy", owner+" 正在控制此电脑，请稍后再试")
						case "pending":
							sendStatus("pending", "等待宿主确认...")
							go doRequestControlWithDialog(userName, func(granted bool) {
								if granted {
									sendStatus("granted", "")
								} else {
									sendStatus("denied", "宿主拒绝了您的控制请求")
								}
							})
						}
					} else {
						releaseControl(userName)
						closeActiveDialog()
					}
				}
				if cm.DX1 != nil && cm.DY1 != nil && cm.DX2 != nil && cm.DY2 != nil && hasControl(userName) {
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
		ffFPS        = -1
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
	)

	for {
		id := int(currentID.Load())
		if id >= screenshot.NumActiveDisplays() {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		q, mw, fps := int(currentQuality.Load()), int(currentMaxW.Load()), int(currentFPS.Load())

		if useFFmpeg {
			// ── ffmpeg 路径 ──
			h264 := useH264.Load()
			if ff == nil || ffScreen != id || ffQ != q || ffMW != mw || ffFPS != fps || ffH264 != h264 {
				if ff != nil {
					releaseFFmpeg(curScreen)
				}
				ff = acquireFFmpeg(id, q, mw, fps, h264)
				if ff == nil {
					log.Printf("ffmpeg 启动失败")
					if h264 && tryNextH264Encoder() {
						continue // 回退到下一个编码器重试
					}
					// 所有 H.264 编码器已耗尽，回退到 MJPEG
					if h264 {
						useH264.Store(false)
						log.Printf("所有 H.264 编码器已耗尽，回退到 MJPEG")
						continue
					}
					time.Sleep(time.Second)
					continue
				}
				ffScreen = id
				curScreen = id
				ffQ = q
				ffMW = mw
				ffFPS = fps
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
					releaseFFmpeg(curScreen) // 清理池中已死的 session，避免 acquireFFmpeg 复用
					ff = nil
					ffScreen = -1
					curScreen = -1
					if ffH264 && tryNextH264Encoder() {
						continue // ffmpeg异常退出，即时回退
					}
					// 所有 H.264 编码器已耗尽，回退到 MJPEG
					if ffH264 {
						useH264.Store(false)
						log.Printf("所有 H.264 编码器已耗尽，回退到 MJPEG")
					}
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
				if ffH264 && tryNextH264Encoder() {
					continue // GPU编码器运行时失败，回退
				}
				// 所有 H.264 编码器已耗尽，回退到 MJPEG
				if ffH264 {
					useH264.Store(false)
					log.Printf("所有 H.264 编码器已耗尽，回退到 MJPEG")
				}
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

			// 仅计数真实编码帧：MJPEG 每帧一个；H.264 跳过 AUD/SEI/SPS/PPS
			isFrame := true
			if useH264.Load() && len(data) > 4 {
				// data 以 Annex B 起始码开头（3B: 00 00 01 或 4B: 00 00 00 01）
				// NAL header 在起始码之后，低 5 位是 NAL unit type
				if data[2] == 1 {
					isFrame = (data[3]&0x1F) == 1 || (data[3]&0x1F) == 5 // 3 字节起始码
				} else if data[2] == 0 && data[3] == 1 {
					isFrame = (data[4]&0x1F) == 1 || (data[4]&0x1F) == 5 // 4 字节起始码
				}
			}
			if isFrame {
				frames++
			}
			if elapsed := time.Since(lastStats); elapsed >= time.Second {
				fps := float64(frames) / elapsed.Seconds()
				maxW := float64(maxWait.Microseconds()) / 1000
				maxRate := 60
				if hasDDAGrab {
					if r := getDisplayRefreshRate(id); r > 0 {
						maxRate = r
					}
					// 分辨率上限（与自动模式一致），选项里不写超出编码能力的值
					if px := cachedBounds.Dx() * cachedBounds.Dy(); px > 0 {
						if c := 700_000_000 / px; maxRate > c {
							maxRate = c
						}
					}
					if maxRate < 60 {
						maxRate = 60
					}
				}
				stat := statsMsg{
					FPS: math.Round(fps*10) / 10, EncMs: math.Round(maxW*10) / 10,
					KB: math.Round(float64(len(data))/102.4) / 10, Owner: controlOwner,
					Ox: cachedBounds.Min.X, Oy: cachedBounds.Min.Y, Zoom: cachedZoom,
					Q: q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
					Screens: screenshot.NumActiveDisplays(), MaxRate: maxRate,
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
		// 主动限速：MJPEG 模式下无 ffmpeg 帧率控制，限制采集速率
		// 避免全速运行导致带宽暴涨、前端解码积压
		targetInterval := time.Second / 60
		if !lastFrame.IsZero() {
			if d := targetInterval - time.Since(lastFrame); d > 0 {
				time.Sleep(d)
			}
		}
		loopStart := time.Now()

		img, err := screenshot.CaptureDisplay(id)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			lastFrame = time.Time{}
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
			frames++
		default:
		}

		// 统计本迭代耗时（含采集+缩放+编码）
		iterDuration := time.Since(loopStart)
		totalWait += iterDuration
		if iterDuration > maxWait {
			maxWait = iterDuration
		}
		lastFrame = loopStart

		if elapsed := time.Since(lastStats); elapsed >= time.Second {
			fps := float64(frames) / elapsed.Seconds()
			maxW := float64(maxWait.Microseconds()) / 1000
			stat := statsMsg{
				FPS: math.Round(fps*10) / 10, EncMs: math.Round(maxW*10) / 10,
				KB: math.Round(float64(len(msg))/102.4) / 10, Owner: controlOwner,
				Ox: cachedBounds.Min.X, Oy: cachedBounds.Min.Y, Zoom: cachedZoom,
				Q: q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
				Screens: screenshot.NumActiveDisplays(), MaxRate: 60,
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
