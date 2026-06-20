package main

import (
	"bytes"
	"encoding/base64"
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
	connCount   atomic.Int32              // 当前活跃 WebSocket 连接数

	activeConns   = make(map[string]*websocket.Conn) // IP → 当前活跃连接（同 IP 新连接顶替旧连接）
	activeConnsMu sync.Mutex
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
	var subID int
	var subCh <-chan []byte
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	// 同 IP 新连接顶替旧连接，避免同一人开多窗口被视为多用户
	activeConnsMu.Lock()
	if old, ok := activeConns[ip]; ok {
		// 发送 4001 关闭码告知前端"被顶替，不要重连"，防止两个窗口无限互踢
		_ = old.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(4001, "同 IP 新连接替代"))
		_ = old.Close()
		log.Printf("[%s] 旧连接已关闭（同 IP 新连接顶替）", ip)
	}
	activeConns[ip] = conn
	activeConnsMu.Unlock()
	// 优先使用客户端指定的用户名（?user=XXX），否则按 IP 自动分配
	userName := strings.TrimSpace(r.URL.Query().Get("user"))
	if userName != "" {
		userMapMu.Lock()
		userMap[ip] = userName
		userMapMu.Unlock()
	} else {
		userName = userNameFor(ip)
	}
	connCount.Add(1)
	log.Printf("[%s] 已连接 (%s)  在线 %d 人", userName, ip, connCount.Load())

	defer func() {
		// 清理本连接的 IP 注册（仅当 map 中仍指向此连接时）
		activeConnsMu.Lock()
		if activeConns[ip] == conn {
			delete(activeConns, ip)
		}
		activeConnsMu.Unlock()
		connCount.Add(-1)
		log.Printf("[%s] 已断开  在线 %d 人", userName, connCount.Load())
		if ff != nil {
			ff.unsubscribe(subID)
			releaseFFmpeg(curScreen)
		}
		releaseControl(userName)
		removeRTCSession(userName) // 清理 WebRTC PeerConnection
		closeActiveDialog()
		_ = conn.Close()
	}()

	// ── 认证握手 ──
	if authPassword != "" {
		challenge := genNonce()

		if b, _ := json.Marshal(map[string]string{"challenge": challenge}); b != nil {
			_ = conn.WriteMessage(websocket.TextMessage, b)
		}

		// 设置 30 秒认证超时
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, msgBytes, err := conn.ReadMessage()
		_ = conn.SetReadDeadline(time.Time{}) // 清除超时
		if err != nil {
			log.Printf("[%s] 认证超时或读取失败: %v", userName, err)
			return
		}

		var authMsg struct {
			Auth *string `json:"auth"`
		}
		if json.Unmarshal(msgBytes, &authMsg) != nil || authMsg.Auth == nil {
			log.Printf("[%s] 无效的认证消息", userName)
			return
		}

		authToken := *authMsg.Auth
		if authToken == "anonymous" {
			// 匿名访问 → 弹出宿主审批弹窗
			log.Printf("[%s] 请求匿名访问，等待宿主审批...", userName)
			done := make(chan struct {
				allowed   bool
				grantCtrl bool
			}, 1)
			go doRequestAccessWithDialog(userName, func(allowed bool, grantCtrl bool) {
				done <- struct {
					allowed   bool
					grantCtrl bool
				}{allowed, grantCtrl}
			})
			result := <-done
			if !result.allowed {
				log.Printf("[%s] 匿名访问被宿主拒绝", userName)
				return
			}
			log.Printf("[%s] 匿名访问已批准 (grantCtrl=%v)", userName, result.grantCtrl)
		} else {
			// 密码认证
			if !verifyAuth(challenge, authToken, authPassword) {
				log.Printf("[%s] 密码验证失败", userName)
				return
			}
			log.Printf("[%s] 密码验证通过", userName)
		}
	} else {
		// 无需密码模式：直接通过
		log.Printf("[%s] 无需密码模式，直接通过", userName)
	}

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
	outCh := make(chan wsMessage, 7)

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
	// WebRTC 模式：前端通过 ?h264=1 URL 参数提前告知，在进入主循环前拉满参数。
	// 非 WebRTC（WS JPEG / WS H.264 手动模式）使用默认值，用户可手动调节。
	if r.URL.Query().Get("h264") == "1" && currentH264Encoder() != "" {
		currentQuality.Store(100) // 最高画质
		currentMaxW.Store(0)      // 原始分辨率
		// fps 保持零值（自动跟随显示器刷新率）
	} else {
		currentQuality.Store(60) // 默认画质
	}

	// 剪贴板防回环：记录最后一次同步的文本和图像
	var clipMu sync.Mutex
	var lastClipSeq uint32
	var lastClipText string
	var lastClipImage []byte

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
				if cm.AdaptMode != nil && hasControl(userName) {
					setAdaptConfig(int(currentQuality.Load()), int(currentFPS.Load()), int(currentMaxW.Load()), *cm.AdaptMode)
				}
				if cm.NetFPS != nil || cm.NetQueue != nil {
					f := 0.0
					q := 0
					if cm.NetFPS != nil {
						f = *cm.NetFPS
					}
					if cm.NetQueue != nil {
						q = *cm.NetQueue
					}
					if hasControl(userName) {
						feedNetworkStats(f, q)
					}
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
				if cm.User != nil {
					newName := strings.TrimSpace(*cm.User)
					if newName != "" && newName != userName {
						oldName := userName
						userMapMu.Lock()
						userMap[ip] = newName
						userMapMu.Unlock()
						userName = newName
						// 如果改名者是当前控制者，同步更新 controlOwner
						controlMu.Lock()
						if controlOwner == oldName {
							controlOwner = newName
						}
						controlMu.Unlock()
						log.Printf("[%s] 改名为 %s", oldName, newName)
						// 通知客户端改名成功
						if b, _ := json.Marshal(map[string]string{"user": newName}); b != nil {
							outCh <- wsMessage{websocket.TextMessage, b}
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
				// 剪贴板同步（前端推送 → 写入本地）
				if cm.Clipboard != nil && hasControl(userName) {
					clipMu.Lock()
					lastClipText = *cm.Clipboard
					clipMu.Unlock()
					if err := setClipboardText(*cm.Clipboard); err != nil {
						log.Printf("剪贴板写入失败: %v", err)
					}
				}
				// 剪贴板图像同步（前端推送 → 写入本地）
				if cm.ClipboardImage != nil && hasControl(userName) {
					pngData, err := base64.StdEncoding.DecodeString(*cm.ClipboardImage)
					if err != nil {
						log.Printf("剪贴板图像 base64 解码失败: %v", err)
					} else {
						clipMu.Lock()
						lastClipImage = pngData
						clipMu.Unlock()
						if err := setClipboardImage(pngData); err != nil {
							log.Printf("剪贴板图像写入失败: %v", err)
						}
					}
				}
				// ── WebRTC 信令处理 ──
				if cm.RTCWebRTC != nil && *cm.RTCWebRTC && webRTCEnabled() {
					// 前端告知支持 WebRTC → 创建 PeerConnection + 生成 Offer
					sendFn := func(data []byte) {
						select {
						case outCh <- wsMessage{websocket.TextMessage, data}:
						default:
						}
					}
					offer, err := createRTCSession(userName, sendFn)
					if err != nil {
						log.Printf("[%s] WebRTC 会话创建失败: %v", userName, err)
					} else {
						b, _ := json.Marshal(map[string]string{"rtc_sdp": offer})
						if b != nil {
							outCh <- wsMessage{websocket.TextMessage, b}
						}
						log.Printf("[%s] WebRTC Offer 已发送", userName)
					}
				}
				if cm.RTCSDP != nil && *cm.RTCSDP != "" {
					// 前端返回 SDP Answer
					if err := handleRTCAnswer(userName, *cm.RTCSDP); err != nil {
						log.Printf("[%s] WebRTC Answer 处理失败: %v", userName, err)
					} else {
						log.Printf("[%s] WebRTC Answer 已接收", userName)
					}
				}
				if cm.RTCIce != nil && *cm.RTCIce != "" {
					// 前端 ICE Candidate
					if err := handleRTCICE(userName, *cm.RTCIce); err != nil {
						log.Printf("[%s] ICE Candidate 处理失败: %v", userName, err)
					}
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
		h264Timeouts int // 连续超时计数，3 次后暂回退 MJPEG
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

	// ── 剪贴板轮询（本地变更 → 推送远端）──
	clipTicker := time.NewTicker(500 * time.Millisecond)
	go func() {
		defer clipTicker.Stop()
		for {
			select {
			case <-readErr:
				return
			case <-clipTicker.C:
				seq := getClipboardSeq()
				clipMu.Lock()
				skip := seq == lastClipSeq
				clipMu.Unlock()
				if skip {
					continue
				}
				text := getClipboardText()
				clipMu.Lock()
				lastClipSeq = seq
				if text != "" && text != lastClipText {
					// 文本变更 → 发送文本
					lastClipText = text
					lastClipImage = nil
					clipMu.Unlock()
					if b, _ := json.Marshal(map[string]string{"clipboard": text}); b != nil {
						select {
						case outCh <- wsMessage{websocket.TextMessage, b}:
						default:
						}
					}
					continue
				}
				// 仅当剪贴板无文本时才检查图像（避免频繁 OpenClipboard）
				if text == "" {
					clipMu.Unlock()
					img := getClipboardImage()
					clipMu.Lock()
					lastClipSeq = seq
					if img != nil && !bytes.Equal(img, lastClipImage) {
						lastClipImage = img
						lastClipText = ""
						clipMu.Unlock()
						b64 := base64.StdEncoding.EncodeToString(img)
						if b, _ := json.Marshal(map[string]string{"clipboard_image": b64}); b != nil {
							select {
							case outCh <- wsMessage{websocket.TextMessage, b}:
							default:
							}
						}
					} else {
						clipMu.Unlock()
					}
				} else {
					// 文本非空且未变更 → 无需任何操作
					clipMu.Unlock()
				}
			}
		}
	}()

	for {
		id := int(currentID.Load())
		if id >= screenshot.NumActiveDisplays() {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		q, mw, fps := int(currentQuality.Load()), int(currentMaxW.Load()), int(currentFPS.Load())

		// 自适应码率：仅 H.264 模式生效（MJPEG 无此机制）
		isCtrl := hasControl(userName)
		if isCtrl && useH264.Load() {
			if aq, afps, amw, active := adaptParams(q, fps, mw); active {
				q, fps, mw = aq, afps, amw
			}
		}

		if useFFmpeg {
			// ── MJPEG → H.264 定期重试 ──
			// 曾工作过的编码器仅因过载超时（非兼容性问题），
			// 冷却 30s 后重试，避免永久困在 MJPEG。
			if !useH264.Load() && hasWorkingH264Encoder() &&
				!h264FallbackTime.IsZero() && time.Since(h264FallbackTime) > 30*time.Second {
				if retryH264Encoders() {
					useH264.Store(true)
					if ff != nil {
						ff.unsubscribe(subID)
						releaseFFmpeg(curScreen)
						ff = nil
						ffScreen = -1
						curScreen = -1
					}
				}
			}

			// ── ffmpeg 路径 ──
			h264 := useH264.Load()
			paramsChanged := ffQ != q || ffMW != mw || ffFPS != fps || ffH264 != h264
			isCtrl = hasControl(userName)

			// 仅控制者可因参数变化重启 ffmpeg；非控制者静默接受现有参数
			if ff == nil || ffScreen != id || (paramsChanged && isCtrl) {
				firstJoin := ff == nil
				if ff != nil {
					ff.unsubscribe(subID)
					releaseFFmpeg(curScreen)
				}
				if paramsChanged && isCtrl {
					ff = restartFFmpeg(id, q, mw, fps, h264)
				} else {
					ff = acquireFFmpeg(id, q, mw, fps, h264)
				}
				if ff == nil {
					log.Printf("[%s] ffmpeg 启动失败 display=%d", userName, id)
					if h264 && tryNextH264Encoder() {
						continue // 回退到下一个编码器重试
					}
					if h264 {
						useH264.Store(false)
						log.Printf("H.264 编码器全部失败，回退 MJPEG")
						continue
					}
					time.Sleep(time.Second)
					continue
				}
				subID, subCh = ff.subscribe()

				// 对齐追踪变量到池中实际参数（加入已有会话时可能与用户默认值不同）
				ffPoolMu.Lock()
				if _, ok := ffPool[id]; ok {
					ffQ = ffPoolQ[id]
					ffMW = ffPoolMW[id]
					ffFPS = ffPoolFPS[id]
					ffH264 = ffPoolH264[id]
					currentQuality.Store(int32(ffQ))
					currentMaxW.Store(int32(ffMW))
					currentFPS.Store(int32(ffFPS))
					useH264.Store(ffH264)
					// 同步局部变量，确保本迭代内格式消息携带正确值
					q, mw, fps = ffQ, ffMW, ffFPS
					h264 = ffH264
				}
				ffPoolMu.Unlock()

				ffScreen = id
				curScreen = id
				cacheFrame = 0
				f := "jpeg"
				if ffH264 {
					f = "h264"
				}
				// 仅在有意义的场景打印日志
				if paramsChanged && isCtrl {
					log.Printf("[%s] 调整参数 → %s q=%d mw=%d fps=%d", userName, f, ffQ, ffMW, ffFPS)
				} else if firstJoin {
					log.Printf("[%s] 加入会话 → 显示器%d %s q=%d mw=%d", userName, id, f, ffQ, ffMW)
				}
				if b, _ := json.Marshal(map[string]interface{}{
					"format": f, "quality": ffQ, "maxw": ffMW, "fps": ffFPS,
				}); b != nil {
					outCh <- wsMessage{websocket.TextMessage, b}
				}
			}

			var data []byte
			select {
			case data = <-subCh:
				if data == nil {
					// 记录当前编码器是否曾成功输出帧（用于判断是兼容性问题还是过载）
					hadFrames := ff != nil && ff.sentFrames
					encName := ""
					if ffH264 {
						encName = currentH264Encoder()
					}

					ff.unsubscribe(subID)
					releaseFFmpeg(curScreen)
					ff = nil
					ffScreen = -1
					curScreen = -1

					if ffH264 {
						if hadFrames {
							// 编码器曾工作 → 标记可用，重试同一编码器
							markH264EncoderWorked(encName)
							h264Timeouts++
							if h264Timeouts >= 3 {
								useH264.Store(false)
								h264FallbackTime = time.Now()
								h264Timeouts = 0
								log.Printf("[%s] %s 连续3次异常退出，暂回退 MJPEG（30s 后重试）", userName, encName)
							} else {
								log.Printf("[%s] %s 异常退出（%d/3），重试同一编码器", userName, encName, h264Timeouts)
							}
						} else if tryNextH264Encoder() {
							// 编码器从未输出帧 → 兼容性问题，尝试下一个
							continue
						} else {
							useH264.Store(false)
							h264FallbackTime = time.Now()
							log.Printf("所有 H.264 编码器已耗尽，回退到 MJPEG")
						}
					}
					continue
				}
				h264Timeouts = 0 // 帧正常到达，清零超时计数
			case <-readErr:
				return
			case <-time.After(8 * time.Second):
				log.Printf("[%s] ffmpeg 超时无帧 display=%d", userName, curScreen)

				hadFrames := ff != nil && ff.sentFrames
				encName := ""
				if ffH264 {
					encName = currentH264Encoder()
				}

				ff.unsubscribe(subID)
				releaseFFmpeg(curScreen)
				ff = nil
				ffScreen = -1
				curScreen = -1

				if ffH264 {
					if hadFrames {
						markH264EncoderWorked(encName)
						h264Timeouts++
						if h264Timeouts >= 3 {
							useH264.Store(false)
							h264FallbackTime = time.Now()
							h264Timeouts = 0
							log.Printf("[%s] %s 连续3次超时，暂回退 MJPEG（30s 后重试）", userName, encName)
						} else {
							log.Printf("[%s] %s 超时（%d/3），重试同一编码器", userName, encName, h264Timeouts)
						}
					} else if tryNextH264Encoder() {
						continue
					} else {
						useH264.Store(false)
						h264FallbackTime = time.Now()
						log.Printf("H.264 编码器全部失败，回退 MJPEG")
					}
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

			if !ffH264 {
				data = encodeFrame(int32(cachedBounds.Min.X), int32(cachedBounds.Min.Y),
					int32(cachedBounds.Dx()), int32(cachedBounds.Dy()), cachedZoom, data)
			}
			if ffH264 {
				// 非阻塞发送。IDR 丢失仅短暂花屏（GOP=120 下约 0.85s），
				// 远好过硬背压造成多秒管道卡死。
				select {
				case outCh <- wsMessage{websocket.BinaryMessage, data}:
				default:
				}
			} else {
				select {
				case outCh <- wsMessage{websocket.BinaryMessage, data}:
				default:
				}
			}

			// 帧计数：H.264 已按 AUD 帧边界打包，每消息即一帧；MJPEG 同理
			frames++
			if elapsed := time.Since(lastStats); elapsed >= time.Second {
				fps := float64(frames) / elapsed.Seconds()
				maxW := float64(maxWait.Microseconds()) / 1000
				maxRate := 60
				if hasDDAGrab {
					if r := getDisplayRefreshRate(id); r > 0 {
						maxRate = r
					}
					if px := cachedBounds.Dx() * cachedBounds.Dy(); px > 0 {
						if c := 700_000_000 / px; maxRate > c {
							maxRate = c
						}
					}
					if maxRate < 60 {
						maxRate = 60
					}
				}
				adaptActive, adaptQVal, adaptFpsVal := getAdaptStatus()
				stat := statsMsg{
					FPS: math.Round(fps*10) / 10, EncMs: math.Round(maxW*10) / 10,
					KB: math.Round(float64(len(data))/102.4) / 10, Owner: controlOwner,
					Ox: cachedBounds.Min.X, Oy: cachedBounds.Min.Y, Zoom: cachedZoom,
					Q: q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
					Screens: screenshot.NumActiveDisplays(), MaxRate: maxRate,
					Users:       int(connCount.Load()),
					AdaptActive: adaptActive,
					AdaptQ:      adaptQVal,
					AdaptFPS:    adaptFpsVal,
				}
				if b, _ := json.Marshal(stat); b != nil {
					select {
					case outCh <- wsMessage{websocket.TextMessage, b}:
					default:
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
			adaptActive, adaptQVal, adaptFpsVal := getAdaptStatus()
			stat := statsMsg{
				FPS: math.Round(fps*10) / 10, EncMs: math.Round(maxW*10) / 10,
				KB: math.Round(float64(len(msg))/102.4) / 10, Owner: controlOwner,
				Ox: cachedBounds.Min.X, Oy: cachedBounds.Min.Y, Zoom: cachedZoom,
				Q: q, W: cachedBounds.Dx(), H: cachedBounds.Dy(),
				Screens: screenshot.NumActiveDisplays(), MaxRate: 60,
				Users:       int(connCount.Load()),
				AdaptActive: adaptActive,
				AdaptQ:      adaptQVal,
				AdaptFPS:    adaptFpsVal,
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
