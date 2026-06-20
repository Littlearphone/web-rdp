# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

Web 远程桌面控制（web-rdp）—— 通过浏览器远程控制 Windows 桌面。Go 后端捕获屏幕，**优先通过 WebRTC (UDP/RTP) 推流，WebSocket (TCP) 作为信令通道和视频回退**。Vue 3 前端解码渲染并捕获用户输入回传。

**核心能力：** 屏幕查看 + 输入转发（键盘/鼠标/触控）+ 双向文本剪贴板同步。无文件传输、无音频传输。

## 构建与开发命令

```bash
# 开发模式（前后端分离）
cd views && npm run dev          # 前端 Vite 开发服务器 :5173，WebSocket 代理到 :9000
go run .                         # Go 后端 :9000（需先构建前端到 static/）

# 生产构建
cd views && npm run build        # vue-tsc 类型检查 + vite 构建 → ../static/
go build .                       # 编译为单一可执行文件（static/ 通过 embed 内嵌）

# 运行选项
web-rdp.exe -port 8080           # 指定端口
web-rdp.exe -tls=false           # 禁用 HTTPS
web-rdp.exe -ffmpeg <path>       # 手动指定 ffmpeg 路径
web-rdp.exe -proxy :7890         # 通过代理下载 ffmpeg
```

## 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.26, `gorilla/websocket`, `pion/webrtc/v4`, Win32 syscall API |
| 前端 | Vue 3 + TypeScript + Pinia + Naive UI, Vite 8, RTCPeerConnection API |
| 编码 | ffmpeg (H.264 NVENC/AMF/QSV/libx264, MJPEG), WebCodecs API |
| 传输 | **WebRTC (UDP/RTP 优先)** + WebSocket (TCP/信令/回退)，内网直连无 STUN/TURN |
| 部署 | 单一 exe，前端静态文件通过 `//go:embed static` 内嵌 |

## 架构

### 后端文件结构与职责

```
main.go              HTTP/WS 服务入口 + CLI 标志 + Win32 控制 API（输入模拟、控制权管理）
ws.go                WebSocket 连接生命周期 + 帧处理主循环 + 编码格式切换 + 用户管理 + WebRTC 信令路由
webrtc.go            WebRTC 全局视频轨 + PeerConnection 生命周期 + SDP/ICE 信令管理（pion/webrtc v4）
permission.go        控制权限管理 + 深色 Win32 弹窗（权限请求/控制中），runtime.LockOSThread()
ffmpeg_pipeline.go   ffmpeg 进程池（引用计数）+ H.264 Annex B / MJPEG 读写器 + Fan-out 分发（含 WebRTC 路径）
ffmpeg_install.go    ffmpeg 自动检测/下载、GPU 供应商检测、编码器优先级排序与回退链
screen.go            DPI 缩放缓存、帧二进制打包（24B 头部）、纯 Go 截图回退（双线性降采样）
display_windows.go   多显示器刷新率检测（EnumDisplaySettingsW → dmDisplayFrequency）
```

### WebSocket 消息协议

| 方向 | 类型 | 内容 | 用途 |
|------|------|------|------|
| 后端→前端 | JSON (init) | `{user, format}` | 连接建立后立即发送，告知用户名和编码格式 |
| 前端→后端 | JSON (ctrlMsg) | `{screen/quality/maxw/webcodecs/fps}` | 流参数调整（仅控制者可调） |
| 前端→后端 | JSON (ctrlMsg) | `{control: bool}` | 请求/释放控制权 |
| 前端→后端 | JSON (ctrlMsg) | `{mx/my/mb/md/rx/ry}` | 鼠标事件（移动/按钮/拖拽） |
| 前端→后端 | JSON (ctrlMsg) | `{key/down/text}` | 键盘事件 / 文本输入 |
| 前端→后端 | JSON (ctrlMsg) | `{dx1/dy1/dx2/dy2}` | 拖拽起止坐标 |
| 后端→前端 | Binary | H.264 Annex B 裸流 | H.264 模式下的视频帧（含 AUD/SPS/PPS/SEI） |
| 后端→前端 | Binary | `[24B meta + JPEG]` | MJPEG 模式：ox/oy/pw/ph/zoom(8B) + JPEG 数据 |
| 后端→前端 | JSON | `{format, quality, maxw, fps}` | 格式切换通知（初始连接或 ffmpeg 重启时） |
| 后端→前端 | JSON (每秒) | `{fps, enc_ms, kb, owner, q, w, h, ox, oy, zoom, screens, maxrate, users}` | 性能统计 |
| 后端→前端 | JSON | `{control_status, control_msg}` | 控制状态变更：granted/denied/busy/pending |
| 前端→后端 | JSON | `{rtc_webrtc: true}` | 告知支持 WebRTC，触发后端创建 PeerConnection（仅 H.264 模式） |
| 后端→前端 | JSON | `{rtc_sdp: "<offer>"}` | WebRTC SDP Offer（后端创建 PeerConnection 后发出） |
| 前端→后端 | JSON | `{rtc_sdp: "<answer>"}` | WebRTC SDP Answer（前端 SetRemoteDescription 后返回） |
| 双向 | JSON | `{rtc_ice: {...}}` | WebRTC ICE Candidate 交换（自动双向） |
| 后端→前端 | RTP/UDP | H.264 Annex B | WebRTC 视频轨（仅 H.264，UDP 直连，不经过 WebSocket 信道） |

### ffmpeg 会话池（核心）

```
ffPool[displayID] → *ffSession（每显示器一个 ffmpeg 进程，多用户共享）
ffRefs[displayID] → int（引用计数，所有用户断开时停进程）
```

- `acquireFFmpeg`：获取或创建会话（参数匹配则复用，否则用池中现有参数）
- `restartFFmpeg`：**仅控制者**调用，停止旧会话并用新参数重建。**必须在调用前通过 releaseFFmpeg 释放自己的引用**，且必须保留其他订阅者的引用计数迁移到新会话
- `releaseFFmpeg`：减引用，至 0 时停止进程并清理
- Fan-out goroutine：将 `frameCh` 的每帧复制给所有订阅者独立通道（WebSocket 路径），同时 H.264 帧写入全局 WebRTC Track（`writeWebRTCSample`，非阻塞丢弃）。双路并行，前端 WebRTC 活跃时跳过 WS 帧避免重复渲染
- 池参数（`ffPoolQ/MW/FPS/H264`）在 `acquireFFmpeg` 后同步回调用方的 atomic 变量，确保非控制者的本地追踪变量与池一致
- 支持 `ddagrab`（DXGI 零拷贝桌面捕获）和 `gdigrab`（传统 GDI）两种捕获模式
- 像素率限制：每显示器 700M 像素/秒上限，防止编码器积压
- H.264 帧丢弃：非 IDR 帧在 channel 满时丢弃旧帧保留新帧，IDR 关键帧阻塞送达确保解码器不花屏。h264Reader → frameCh → 扇出 → outCh 三级均可丢弃，防止管道反向阻塞 ffmpeg stdout

### 编码器回退链

`h264Encoders` 按优先级排列：
```
h264_nvenc (NVIDIA, preset=p1 + tune=ll + rc=vbr + cq)
  → h264_amf (AMD, quality=speed + rc=cqp)
    → h264_qsv (Intel, preset=veryfast + look_ahead=0 + async_depth=1)
      → libx264 (软件, preset=ultrafast + tune=zerolatency + crf + slices=1 + threads=1)
```

编码失败时 `tryNextH264Encoder()` 递增索引回退。所有 H.264 编码器耗尽后 `useH264=false` 回退到 MJPEG（ffmpeg mjpeg 编码器）。最终回退：纯 Go `image/jpeg` 编码 + `draw.BiLinear.Scale` 降采样，限速 60 fps。

画质映射：用户滑块 30-100 → H.264 CRF/CQ 1-51 或 MJPEG Q 1-31。

### 前端文件结构与职责

```
App.vue                         根组件，用户名设置弹窗（模态框，默认"用户"+随机4位数）
DesktopControls.vue             桌面顶栏（屏幕选择/控制权/画质滑块/分辨率/H.264开关/FPS/性能统计）
MobileControls.vue              移动端控件（竖屏底部栏/横屏侧边栏，屏幕切换/画质按钮组/分辨率按钮组）
ScreenCanvas.vue                Canvas 渲染 + 鼠标事件 + H.264/JPEG 解码器生命周期管理
StatsDisplay.vue                移动端最小化统计（用户名/fps/KB）
ConnectionOverlay.vue           断线重连覆盖层（倒计时 + "立即重连"按钮）

composables/useWebSocket.ts     WS 连接管理 + WebRTC 信令转发，指数退避重连（5s→10s→20s→最大30s）
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
| 刷新率上限 | `maxrate` | 显示器最大刷新率（仅 ddagrab） |
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
| ffmpeg 进程退出 | H.264 reader 发送 `nil` → 主循环回退到下一编码器或 MJPEG |
| ffmpeg 无帧超时 | 5 秒超时触发回退链 |
| 所有 H.264 编码器失败 | `useH264=false`，回退 MJPEG |
| 无 ffmpeg | 纯 Go `screenshot.CaptureDisplay` + `image/jpeg` 回退，限速 60fps |
| 解码错误 | H.264: `decoder.error` 回调 + 关键帧保护；JPEG: `createImageBitmap` 静默捕获 |
| 首帧保护 | 首个 `decode()` 必须为关键帧，跳过非关键帧直至收到关键帧 |
| 键盘安全 | 失焦/隐藏/断连/剥夺控制权时自动释放所有已按下按键 |
| 格式切换 | `resetDecoders()` 重建解码器，状态 `switching` 阻止渲染。MJPEG 模式自动关闭 WebRTC |
| WebRTC 失败/断连 | `pc.onconnectionstatechange` → `failed/disconnected` 清理会话；前端 `watch connectionStatus` → 重连后重新创建 WebRTC。视频回退到 WebSocket 二进制帧（`isWebRTCConnected()` 为 false 时自动接管） |

## 重要约定

- 解码器创建/关闭必须在 `ScreenCanvas` 生命周期内（`onMounted`/`onUnmounted`）
- `streamFormat` 切换时调用 `resetDecoders()`，格式消息到达前 `connectionStatus = 'switching'`
- 后端 `current*` 变量使用 `atomic.Int32/Bool`，参数变化检查 `paramsChanged` 必须在主循环原子读取后立即计算
- `restartFFmpeg` 的调用者必须先 `ff.unsubscribe(subID)` + `releaseFFmpeg(curScreen)` 再调用
- 非控制者进入 `acquireFFmpeg` 路径后，池同步代码会将 atomic 变量覆写为池中实际值（这是预期行为）
- Win32 UI（权限弹窗）必须 `runtime.LockOSThread()` + 消息循环结束 `UnlockOSThread()`
- **WebRTC 时序**：前端必须在 `connectionStatus === 'connected'` 后（或已在 connected 状态时）初始化 WebRTC，否则 `store.send()` 因 WS 未 OPEN 而静默丢弃 `{rtc_webrtc: true}`
- **WebRTC 双路避让**：`useWebSocket.ts` 的二进制帧 handler 在 `isWebRTCConnected()` 为 true 时跳过——避免同一画面被 WebRTC 和 WS 双重渲染
- **WebRTC 生命周期**：`ScreenCanvas` 的 `watch connectionStatus` 在 `disconnected/failed` 时自动 `webrtc.close()`；`watch streamFormat` 切换为 `jpeg` 时关闭 WebRTC（MJPEG 不适用 RTP）
- **pion 全局视频轨**：单 `TrackLocalStaticSample` 供所有 PeerConnection 共享，所有显示器 H.264 帧写入同一轨。`WriteSample` 非阻塞，无订阅者时静默丢弃——禁止反压阻塞 ffmpeg
- **后端信令在 `ws.go` 处理**：`ctrlMsg.RTCWebRTC/SDP/Ice` 在 read goroutine 中处理，ICE candidate 通过 `sendFn` 回调利用用户级 `outCh` 推送

## 待实现

### 核心功能缺失（高优先级）

- [x] **剪贴板同步**：双向文本 + 图像剪贴板同步。文本通过 `CF_UNICODETEXT` + JSON 消息同步；图像通过 `CF_DIB ↔ PNG` 转换 + base64 JSON 消息同步。前端 `onCopy` 使用同步 `e.clipboardData.getData()`（而非异步 `navigator.clipboard.readText()`）确保可靠性。前端 `onPaste` 支持 `ClipboardEvent.items` 中的 image/png 类型。
- [x] **密码认证**：`-password` 参数，challenge-response (SHA-256) 认证。匿名用户（无密码）需宿主审批。待扩展：失败次数限制 + IP 冷却防暴力破解。
- [x] **WebRTC 传输**：H.264 视频通过 WebRTC (UDP/RTP) 传输，WebSocket 保留为信令通道和 MJPEG/H.264 回退。后端 pion/webrtc v4 → `TrackLocalStaticSample` 全局视频轨，前端 `RTCPeerConnection` + hidden `<video>` 解码 + rAF 绘制。内网直连无 STUN/TURN。`writeWebRTCSample` 非阻塞，无订阅者时静默丢弃帧。格式切换至 MJPEG 或 WebRTC 连接失败时自动回退 WebSocket。
- [ ] **音频传输**：后端 WASAPI Loopback 捕获系统音频 → ffmpeg Opus/AAC 编码 → 前端 Web Audio API 播放。与视频帧 PTS 时间戳对齐。WebRTC 可复用同一 PeerConnection 的音频轨。

### 体验提升（中优先级）

- [x] **动态码率自适应**：前端每 2 秒上报实际接收帧率 + 解码队列深度 → 后端 `adapt.go` 拥塞检测 → 在控制者偏好上限内自动降级画质/帧率/分辨率，恢复时逐级回升。两条策略：画质优先（先降帧率）和流畅优先（先降画质）。仅控制者网络反馈驱动自适应，5s 冷却防抖。WebRTC 路径 GCC 提供额外传输层调节。
- [ ] **光标渲染同步**：后端 `GetCursorInfo` 捕获光标位置 + 形状 → 前端 CSS 绝对定位 canvas 叠加渲染本地光标，消除"光标在哪"的困惑。
- [ ] **全屏模式**：`Element.requestFullscreen()` + `navigator.keyboard.lock()`，全屏时隐藏顶栏/侧边栏。
- [ ] **HEVC/AV1 编码支持**：`hevc_nvenc`/`hevc_amf`/`hevc_qsv` 或 AV1，前端 `VideoDecoder.isConfigSupported()` 能力检测后协商编码格式。
- [ ] **日志与诊断**：分级日志（DEBUG/INFO/WARN/ERROR）+ 文件持久化。`/health` 端点（版本/运行时间/连接数/编码器状态）。开发模式 `/debug/pprof`。
- [ ] **多流独立编码**：当前同显示器所有用户共享一个 ffmpeg 进程，参数由控制者决定。改为 `pool[{displayID, userID}]` 每用户独立 ffmpeg（或至少控制者独立），使不同用户可使用不同分辨率/画质/帧率。需评估 GPU 编码器并发能力（NVENC 通常支持 2-3 路）。

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
