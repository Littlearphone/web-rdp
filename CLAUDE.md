# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

Web 远程桌面控制（web-rdp）—— 通过浏览器远程控制 Windows 桌面。Go 后端捕获屏幕，**优先通过 WebRTC (UDP/RTP) 推流，WebSocket (TCP) 作为信令通道和视频回退**。Vue 3 前端解码渲染并捕获用户输入回传。

**核心能力：** 屏幕查看 + 输入转发（键盘/鼠标/触控）+ 双向文本剪贴板同步。无文件传输、无音频传输。

## 构建与开发命令

```bash
# 项目布局: Go 后端集中在 backend/(main 包 + native 包 + go.mod), 前端 views/,
# 前端构建产物输出 backend/static(供 go:embed), 顶层 package.json = pnpm 一键入口。

# 开发模式（前后端分离）— 根目录一键或手动
pnpm install && pnpm dev          # 同时起 前端 Vite(:5173) + Go 后端(:9000)
cd views && pnpm run dev          # 仅前端 Vite :5173，WebSocket 代理到 :9000
cd backend && go run .            # 仅后端 :9000（backend/static 缺失需先构建前端一次）

# 生产构建
pnpm run build                    # scripts\build.bat: views→backend/static → backend → dist\web-rdp.exe
# 手工等价:
cd views && pnpm run build        # → ../backend/static
cd backend && go build -trimpath -ldflags "-s -w" -o ../dist/web-rdp.exe .

# 运行选项
web-rdp.exe -port 8080           # 指定端口
web-rdp.exe -tls=false           # 禁用 HTTPS
web-rdp.exe -probe-native        # 探测进程内 MF/GPU 编码后端
web-rdp.exe -mf-encode           # 单帧 MF H.264 编码冒烟
```

## 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.26, `gorilla/websocket`, `pion/webrtc/v4`, Win32 syscall API |
| 前端 | Vue 3 + TypeScript + Pinia + Naive UI, Vite 8, RTCPeerConnection API |
| 编码 | 进程内 native(MF) H.264（硬件异步 MFT 优先，软件 MF 回退）→ 纯 Go JPEG 兜底, WebCodecs API |
| 传输 | **WebRTC (UDP/RTP 优先)** + WebSocket (TCP/信令/回退)，内网直连无 STUN/TURN |
| 部署 | 单一 exe，前端静态文件通过 `//go:embed static` 内嵌 |

## 架构

### 后端文件结构与职责

```
main.go              HTTP/WS 服务入口 + CLI 标志 + Win32 控制 API（输入模拟、控制权管理）
ws.go                WebSocket 连接生命周期 + 帧处理主循环 + H.264(进程内 native) ↔ 纯 Go JPEG 切换 + 用户管理 + WebRTC 信令路由
webrtc.go            WebRTC 全局视频轨 + PeerConnection 生命周期 + SDP/ICE 信令管理（pion/webrtc v4）
permission.go        控制权限管理 + 深色 Win32 弹窗（权限请求/控制中），runtime.LockOSThread()
streamer.go          streamer 抽象 + native H.264 可用性探测
tiers.go             per-display 共享 DXGI 采集 broker + (display,maxW,fps) 档位键控会话池（同档位共享编码、同屏多档共存）
native_session.go    档位编码会话（streamer 实现）：消费 broker 帧 → 硬件异步 MFT 优先/软件 MF 回退 → fan-out + 档位 WebRTC
screen.go            DPI 缩放缓存、帧二进制打包（24B 头部）、纯 Go JPEG 回退（双线性降采样）
display_windows.go   多显示器刷新率检测（EnumDisplaySettingsW → dmDisplayFrequency）
```

> 画质维度已移除：档位参数 = 分辨率(maxW) × 帧率(fps)，码率按固定质量估算。
> WebRTC 视频轨按档位键控（per-tier track）。

> 已完全移除 ffmpeg（`ffmpeg_install.go`/`ffmpeg_pipeline.go` 已删）。H.264 唯一来源是
> 进程内 native(MF) 会话；native 产不出帧（或用户关 H.264）时 ws.go 自动落到纯 Go JPEG。

### WebSocket 消息协议

| 方向 | 类型 | 内容 | 用途 |
|------|------|------|------|
| 后端→前端 | JSON | `{challenge}` | 认证挑战（`-password` 非空时连接建立后的第一条消息） |
| 后端→前端 | JSON | `{auth_result, auth_msg}` | 认证失败原因：`bad_password` / `denied` / `timeout`，随后以 Close 帧（code 1008）断开 |
| 前端→后端 | JSON | `{auth}` | 认证应答：`sha256(challenge+password)` 或 `anonymous`。**必须是认证窗口内第一条消息** |
| 后端→前端 | JSON (init) | `{user, format}` | 认证放行后发送，告知用户名和编码格式 |
| 前端→后端 | JSON (ctrlMsg) | `{screen/quality/maxw/webcodecs/fps}` | 流参数调整（仅控制者可调） |
| 前端→后端 | JSON (ctrlMsg) | `{control: bool}` | 请求/释放控制权 |
| 前端→后端 | JSON (ctrlMsg) | `{mx/my/mb/md/rx/ry}` | 鼠标事件（移动/按钮/拖拽） |
| 前端→后端 | JSON (ctrlMsg) | `{key/down/text}` | 键盘事件 / 文本输入 |
| 前端→后端 | JSON (ctrlMsg) | `{dx1/dy1/dx2/dy2}` | 拖拽起止坐标 |
| 后端→前端 | Binary | H.264 Annex B 裸流 | H.264 模式下的视频帧（含 AUD/SPS/PPS/SEI） |
| 后端→前端 | Binary | `[24B meta + JPEG]` | JPEG(纯 Go) 模式：ox/oy/pw/ph/zoom(8B) + JPEG 数据 |
| 后端→前端 | JSON | `{format, quality, maxw, fps}` | 格式切换通知（初始连接或 native H.264↔JPEG 切换时） |
| 后端→前端 | JSON (每秒) | `{fps, enc_ms, kb, owner, q, w, h, ox, oy, zoom, screens, maxrate, users}` | 性能统计 |
| 后端→前端 | JSON | `{control_status, control_msg}` | 控制状态变更：granted/denied/busy/pending |
| 前端→后端 | JSON | `{rtc_webrtc: true}` | 告知支持 WebRTC，触发后端创建 PeerConnection（仅 H.264 模式） |
| 后端→前端 | JSON | `{rtc_sdp: "<offer>"}` | WebRTC SDP Offer（后端创建 PeerConnection 后发出） |
| 前端→后端 | JSON | `{rtc_sdp: "<answer>"}` | WebRTC SDP Answer（前端 SetRemoteDescription 后返回） |
| 双向 | JSON | `{rtc_ice: {...}}` | WebRTC ICE Candidate 交换（自动双向） |
| 后端→前端 | RTP/UDP | H.264 Annex B | WebRTC 视频轨（仅 H.264，UDP 直连，不经过 WebSocket 信道） |

### 档位会话与共享采集（核心）

`tiers.go` 每显示器一个 `captureBroker`（引用计数，共享一路 DXGI 采集，规避 `DuplicateOutput` 每输出单采集限制）；`ws.go` 按档位 `(display, maxW, fps)` 经 `acquireTier` 加入/新建编码会话。画质维度已移除，码率按固定质量估算。

- 档位编码会话（`native_session.go`）：消费 broker 帧 → 降采样→NV12 → MFH264Encoder（硬件异步 MFT 优先→软件 MF）→ fan-out 到同档位订阅者，并写档位 WebRTC 轨（`writeTierWebRTCSample`，非阻塞丢弃）
- 订阅模型：多订阅者各自 `subscribe()` 拿独立通道，通道满丢旧保新；`unsubscribe` 后无订阅者自动停会话
- 会话退出（编码器/采集不可用、连续编码错误）会广播 `nil`(EOF) 并置 `ended`；ws.go 据此判定"native 产不出帧"
- 前端 WebRTC 活跃时 ws.go 跳过 WS 帧（`userRTCVideoStatus` 检查）避免双路重复渲染
- 像素率上限：stats 按 700M 像素/秒钳制显示刷新率，防编码积压

### 编码回退链（native 唯一 H.264 源）

```
硬件异步 MFT（NativeAsyncMFH264Encoder，跨厂商，优先）
  → 软件同步 MF（NewMFH264EncoderQ，无 GPU 亦可）
    → 两者都失败：会话广播 EOF → ws.go 落纯 Go JPEG
```

- native 会话启动即失败或中途产不出帧 → ws.go 置 `nativeDead` 并发 `format=jpeg`，此后该连接恒用纯 Go JPEG（`screenshot.CaptureDisplay` + `image/jpeg`，限速 60 fps）
- H.264 可用性：`h264Available()` 惰性探测一次能否构建 MF H.264 编码器（硬件或软件皆可）
- 画质映射：用户滑块 30-100 → 码率估算（`native.EstimateBitrateForQuality`）或 JPEG 质量

### 前端文件结构与职责

```
App.vue                         根组件，用户名设置弹窗（模态框，默认"用户"+随机4位数）
DesktopControls.vue             桌面顶栏（屏幕选择/控制权/画质滑块/分辨率/H.264开关/FPS/性能统计）
MobileControls.vue              移动端控件（竖屏底部栏/横屏侧边栏，屏幕切换/画质按钮组/分辨率按钮组）
ScreenCanvas.vue                Canvas 渲染 + 鼠标事件 + H.264/JPEG 解码器生命周期管理
StatsDisplay.vue                移动端最小化统计（用户名/fps/KB）
ConnectionOverlay.vue           断线重连覆盖层（倒计时 + "立即重连"按钮）

composables/useWebSocket.ts     WS 连接管理（认证握手状态机 + WebRTC 信令转发），指数退避重连（5s→10s→20s→最大30s）
utils/sha256.ts                 SHA-256 摘要：WebCrypto 优先 + 非安全上下文纯 JS 回退（认证用）
composables/useWebRTC.ts        WebRTC 接收端：RTCPeerConnection + hidden video 元素解码 + rAF 绘制到 canvas
composables/useKeyboardCapture.ts  全局键盘捕获，跟踪按下键，失焦/断连时释放所有键，拦截浏览器快捷键
composables/useCoordinateMapping.ts  浏览器像素→远程桌面物理坐标映射（letterbox/pillarbox + DPI）
composables/useResolutionOptions.ts  分辨率选项构建（原始→1080p→720p→480p），FPS选项
stores/app.ts                   Pinia 集中状态管理（连接/流/显示/性能/控制状态）
decoders/h264.ts                WebCodecs VideoDecoder，Annex B→AVCC 转换，SPS/PPS 提取，队列深度>3 跳过 delta 帧
decoders/jpeg.ts                createImageBitmap 异步解码，帧序号机制丢弃过期位图
types/index.ts                  TypeScript 类型定义
```

### 关键数据流

- **WebRTC 视频（优先）**：后端 fan-out → `writeWebRTCSample()` → pion TrackLocalStaticSample → RTP/UDP → 前端 `RTCPeerConnection.ontrack` → hidden `<video>` 解码 → rAF `ctx.drawImage` 绘制到 canvas
- **WebSocket 视频（回退）**：`useWebSocket.registerBinaryHandler` → `ScreenCanvas.handleBinary` → `h264Decoder.feed` / `jpegDecoder.feed` → rAF 绘制到 canvas。WebRTC 活跃时跳过此路径（`isWebRTCConnected()` 检查）
- **WebRTC 信令**：前端 `watch connectionStatus→connected` → `tryStartWebRTC()` → 发送 `{rtc_webrtc: true}` → 后端创建 PeerConnection+Offer → SDP/ICE 通过 WebSocket JSON 交换 → 连接建立
- **坐标映射**：`useCoordinateMapping.screenCoords` 将浏览器像素映射到远程桌面物理坐标，考虑 letterbox/pillarbox 黑边 + DPI 缩放（`meta.ox/oy/zoom`）
- **Canvas 尺寸策略**：CSS `width/height: 100%` 保证画布始终填满容器，分辨率变更只影响画质/带宽，不改变显示尺寸
- **流式拖拽**：LEFDOWN + 光标位置 → 持续光标移动（限频 30ms）→ LEFTUP + 最终位置

## 安全特性

| 特性 | 当前实现 |
|------|----------|
| 传输加密 | 默认 HTTPS/WSS，自签名 ECDSA P-256 证书（365 天有效期），可 `-tls=false` 禁用 |
| 证书存储 | `%APPDATA%/web-rdp/cert.pem` + `key.pem`，密钥文件 `0600` 权限 |
| 认证 | 可选密码认证（`-password` 参数），challenge-response (SHA-256)。无密码时仅靠用户名标识（IP 映射 + `?user=` 参数），匿名用户需宿主审批 |
| 控制权访问 | 单用户控制（`controlOwner` + 互斥锁），每次输入操作前检查 `hasControl(user)` |
| 权限管理 | 内存白名单/黑名单（`alwaysAllow` / `permanentlyDeny`），Win32 深色弹窗请求/确认 |
| WebSocket 来源 | `CheckOrigin` 返回 `true`（允许任意来源）—— 无 CSRF 保护 |
| 文件权限 | 截图缓存 `0700`，PEM 密钥 `0600` |

**已知安全风险：** 允许任意 WebSocket 来源、无连接限速/IP 冷却、无失败次数限制。

## 输入事件支持

### 键盘

- 通过 Windows `keybd_event` API 模拟
- 47 个命名键映射（`keyCodeMap`）：Backspace/Tab/Enter/修饰键/方向键/F1-F12 等
- 动态解析：`KeyA`-`KeyZ` → ASCII，`Digit0`-`Digit9` → ASCII，`VK<number>` → 直接虚拟键码
- 文本输入：`doTypeText` 逐字符发送 keydown+keyup
- 浏览器快捷键拦截（Ctrl+T/Ctrl+W 等）：`preventDefault()` + 双重焦点检测（rAF + 100ms 兜底）
- 安全：失焦/标签页隐藏/断连/控制权被剥夺时自动释放所有已按下按键

### 鼠标

- `SetCursorPos` 设置光标位置（同步）
- `mouse_event` 发送事件（异步排队）
- 左键/右键/拖拽（LEFDOWN → 移动 → LEFTUP）
- 移动端支持单点触控坐标映射（鼠标事件兼容 `TouchEvent`）

### 移动端限制

- 无双指缩放、双指滚动、长按右键手势
- 无屏幕键盘辅助

## 性能监控

### 服务端（每秒推送到所有客户端）

| 指标 | 字段 | 说明 |
|------|------|------|
| 实际帧率 | `fps` | 每秒帧数 |
| 编码延迟 | `enc_ms` | 最大编码耗时（ms） |
| 帧大小 | `kb` | 最后一帧估计大小 |
| 捕获分辨率 | `w`, `h` | 当前编码分辨率 |
| 显示偏移 | `ox`, `oy` | 相对于虚拟桌面原点 |
| DPI 缩放 | `zoom` | 显示器缩放比例 |
| 显示器数 | `screens` | 活动显示器数量 |
| 刷新率上限 | `maxrate` | 显示器最大刷新率（native DXGI 采集，像素率钳制） |
| 在线用户 | `users` | 当前 WebSocket 连接数 |
| 控制者 | `owner` | 当前控制者用户名 |
| 画质 | `q` | 当前画质设置 |

### 前端额外指标

- `decoder.decodeQueueSize` 监控（队列深度 >3 跳过 delta 帧）
- 带宽估算：`statsKb * statsFps` → MB/s
- 帧间间隔追踪（最小/最大/总等待时间）

## 错误处理与重连

| 场景 | 处理机制 |
|------|----------|
| WebSocket 断线 | 指数退避重连：5s→10s→20s→最大30s，前端倒计时覆盖层 |
| native 会话退出 | 产帧 goroutine 广播 `nil`(EOF) 并置 `ended` → ws.go 判"产不出帧"，回退纯 Go JPEG |
| native 会话 8s 无帧且已结束 | 从未产帧 → 判产不出帧，回退 JPEG |
| native 编码器(硬件/软件)均失败 | 会话广播 EOF → ws.go 落到纯 Go JPEG |
| 静止桌面 | native 采集仅在桌面变化时产帧，静止无帧属正常，不视为故障 |
| 解码错误 | H.264: `decoder.error` 回调 + 关键帧保护；JPEG: `createImageBitmap` 静默捕获 |
| 首帧保护 | 首个 `decode()` 必须为关键帧，跳过非关键帧直至收到关键帧 |
| 键盘安全 | 失焦/隐藏/断连/剥夺控制权时自动释放所有已按下按键 |
| 格式切换 | `resetDecoders()` 重建解码器，状态 `switching` 阻止渲染。切到 JPEG 时自动关闭 WebRTC |
| WebRTC 失败/断连 | `pc.onconnectionstatechange` → `failed/disconnected` 清理会话；前端 `watch connectionStatus` → 重连后重新创建 WebRTC。视频回退到 WebSocket 二进制帧（`isWebRTCConnected()` 为 false 时自动接管） |

## 重要约定

- 解码器创建/关闭必须在 `ScreenCanvas` 生命周期内（`onMounted`/`onUnmounted`）
- `streamFormat` 切换时调用 `resetDecoders()`，格式消息到达前 `connectionStatus = 'switching'`
- 后端 `current*` 变量使用 `atomic.Int32/Bool`，参数变化检查必须在主循环原子读取后立即计算
- 档位变化：先 `teardownSession()`（unsubscribe；若是该档位最后观众即自动停并释放采集 feed），再 `acquireTier(id, maxW, fps)` 加入/新建档位会话
- ws.go 帧循环于本迭代内原子读取后立即判定档位是否变化（curScreen/maxW/fps 任一变化 → 切档位；画质维度已移除）
- Win32 UI（权限弹窗）必须 `runtime.LockOSThread()` + 消息循环结束 `UnlockOSThread()`
- **WebRTC 时序**：前端必须在 `connectionStatus === 'connected'` 且认证放行后（或已在 connected 状态时）初始化 WebRTC，否则 `store.send()` 因 WS 未 OPEN 而静默丢弃 `{rtc_webrtc: true}`
- **认证握手独占性（关键）**：后端认证窗口内读取的消息只认第一条为 auth 应答，任何抢发的控制消息都会被判为非认证消息。因此前端 `connectionStatus` 在认证放行（收到 `{user,format}`）之后才置 `connected`，认证完成前的 `store.send()` 一律入队（`store.wsReady`/`markReady()`），认证摘要走 `store.sendRaw()` 直写 socket。**破坏这条会在安全上下文（127.0.0.1/localhost/https）下直接登录失败**——`ScreenCanvas` 挂载即发 `{rtc_webrtc:true}` 是历史上踩过的坑
- **crypto.subtle 仅安全上下文可用**：`http://<局域网IP>:9000`（非安全上下文）下 `crypto.subtle` 为 `undefined`，SHA-256 摘要必须走 `@/utils/sha256.ts` 的纯 JS 回退，禁止直接调用 `crypto.subtle.digest`
- **认证失败必须回传原因**：后端拒绝时调用 `notifyAuthFailure()`（JSON `auth_result`/`auth_msg` + Close 帧 reason + 读应答避免 RST），前端在登录弹窗显示 `store.authError`
- **WebRTC 双路避让**：`useWebSocket.ts` 的二进制帧 handler 在 `isWebRTCConnected()` 为 true 时跳过——避免同一画面被 WebRTC 和 WS 双重渲染
- **WebRTC 生命周期**：`ScreenCanvas` 的 `watch connectionStatus` 在 `disconnected/failed` 时自动 `webrtc.close()`；`watch streamFormat` 切换为 `jpeg` 时关闭 WebRTC（JPEG 不适用 RTP）
- **pion per-tier 视频轨**：`tierTracks[trackKey]`(显示器-maxW-fps) 每档位一条 `TrackLocalStaticSample`；用户 PeerConnection 订阅其当前档位轨，档位/显示器变化时 `restartRTC` 通知前端重建。`WriteSample` 非阻塞，无订阅者时静默丢弃——禁止反压阻塞档位产帧
- **后端信令在 `ws.go` 处理**：`ctrlMsg.RTCWebRTC/SDP/Ice` 在 read goroutine 中处理，ICE candidate 通过 `sendFn` 回调利用用户级 `outCh` 推送

## 待实现

### 核心功能缺失（高优先级）

- [x] **剪贴板同步**：双向文本 + 图像剪贴板同步。文本通过 `CF_UNICODETEXT` + JSON 消息同步；图像通过 `CF_DIB ↔ PNG` 转换 + base64 JSON 消息同步。前端 `onCopy` 使用同步 `e.clipboardData.getData()`（而非异步 `navigator.clipboard.readText()`）确保可靠性。前端 `onPaste` 支持 `ClipboardEvent.items` 中的 image/png 类型。
- [x] **密码认证**：`-password` 参数，challenge-response (SHA-256) 认证。匿名用户（无密码）需宿主审批。待扩展：失败次数限制 + IP 冷却防暴力破解。
- [x] **WebRTC 传输**：H.264 视频通过 WebRTC (UDP/RTP) 传输，WebSocket 保留为信令通道和 JPEG/H.264 回退。后端 pion/webrtc v4 → per-display `TrackLocalStaticSample`，前端 `RTCPeerConnection` + hidden `<video>` 解码 + rAF 绘制。内网直连无 STUN/TURN。`writeWebRTCSample` 非阻塞，无订阅者时静默丢弃帧。格式切至 JPEG 或 WebRTC 连接失败时自动回退 WebSocket。
- [ ] **音频传输**：后端 WASAPI Loopback 捕获系统音频 → 进程内 Opus/AAC 编码 → 前端 Web Audio API 播放。与视频帧 PTS 时间戳对齐。WebRTC 可复用同一 PeerConnection 的音频轨。（ffmpeg 已移除，音频编码需自选/自实现 native 编码栈）

### 体验提升（中优先级）

- [x] **动态码率自适应**：前端每 2 秒上报实际接收帧率 + 解码队列深度 → 后端 `adapt.go` 拥塞检测 → 在控制者偏好上限内自动降级画质/帧率/分辨率，恢复时逐级回升。两条策略：画质优先（先降帧率）和流畅优先（先降画质）。仅控制者网络反馈驱动自适应，5s 冷却防抖。WebRTC 路径 GCC 提供额外传输层调节。
- [ ] **光标渲染同步**：后端 `GetCursorInfo` 捕获光标位置 + 形状 → 前端 CSS 绝对定位 canvas 叠加渲染本地光标，消除"光标在哪"的困惑。
- [ ] **全屏模式**：`Element.requestFullscreen()` + `navigator.keyboard.lock()`，全屏时隐藏顶栏/侧边栏。
- [ ] **HEVC/AV1 编码支持**：基于进程内 MF 的 HEVC/AV1 编码器 MFT 探测（对齐现有 H.264 native 会话路径），前端 `VideoDecoder.isConfigSupported()` 能力检测后协商编码格式。
- [ ] **日志与诊断**：分级日志（DEBUG/INFO/WARN/ERROR）+ 文件持久化。`/health` 端点（版本/运行时间/连接数/编码器状态）。开发模式 `/debug/pprof`。
- [x] **共享采集的 native 会话池（已完成）**：`tiers.go` 的 `captureBroker`（每显示器共享一路 DXGI 采集）+ `(display,maxW,fps)` 档位键控会话；同档位多观众共享同一会话（多订阅 fan-out），同屏多档经共享采集共存。画质维度已移除。剩余约束：硬件 MF 编码器并发约 2–3 路，超出时新档位回退软件 MF / 纯 Go JPEG。

### 锦上添花（低优先级）

- [ ] **应用窗口级捕获**：后端 `EnumWindows` 枚举窗口列表 → 前端选择 → `GetWindowDC` + 窗口 rect 裁剪。需处理最小化/遮挡窗口。
- [ ] **移动端手势**：双指缩放（调整 `maxw`）、双指滚动（映射鼠标滚轮）、长按右键。
- [ ] **Wake-on-LAN**：后端记录 MAC 地址 → 前端"唤醒"按钮 → `net.DialUDP` 发送 Magic Packet。配合 `/api/wol` HTTP 端点。
- [ ] **聊天/标注**：简单文本聊天复用 WebSocket 通道。Canvas overlay 层画线/箭头标注（仅本地显示）。
- [ ] **会话录制与回放**：后端 H.264 裸流直接封装 MP4 写入本地文件。前端回放页面。

### 架构改进

- [ ] **配置热重载与持久化**：当前参数仅通过 WS 消息修改，重启即丢失。增加配置文件 + `-config` 参数。
- [ ] **优雅关闭**：引入 `context.Context` 传递取消信号，替代当前的裸退出。
- [ ] **测试覆盖**：当前仅 `test/dlgcheck.go` 原型文件。需单元测试 + WebSocket 集成测试。
- [ ] **反向代理友好**：硬编码路径 `/ws` → 支持 `-base-path /rdp/` 前缀配置。
- [ ] **连接限速**：限制单 IP 连接数和消息频率，防止资源耗尽。
- [ ] **Docker 化**：编写 Dockerfile + docker-compose（需评估 Windows 容器兼容性）。
