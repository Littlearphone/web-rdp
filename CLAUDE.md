# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

Web 远程桌面控制（web-rdp）—— 通过浏览器远程控制 Windows 桌面。Go 后端捕获屏幕并通过 WebSocket 实时推流，Vue 3 前端解码渲染并捕获用户输入回传。

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
| 后端 | Go 1.26, `gorilla/websocket`, Win32 syscall API |
| 前端 | Vue 3 + TypeScript + Pinia + Naive UI, Vite 8 |
| 编码 | ffmpeg (H.264 NVENC/AMF/QSV/libx264, MJPEG), WebCodecs API |
| 部署 | 单一 exe，前端静态文件通过 `//go:embed static` 内嵌 |

## 架构

### 后端进程模型

```
main.go (HTTP/WS 服务 + Win32 控制 API)
├── ws.go           WebSocket 连接生命周期 + 帧处理主循环 + 编码格式切换
├── ffmpeg_pipeline.go   ffmpeg 进程池（引用计数）、H.264/MJPEG 读写器
├── ffmpeg_install.go    ffmpeg 自动检测/下载、编码器检测与回退链
├── screen.go       DPI 缩放缓存、帧二进制打包、纯 Go 截图回退
├── display_windows.go   多显示器刷新率检测（EnumDisplaySettings）
├── permission.go   控制权限管理 + 深色 Win32 弹窗（权限请求/控制中）
```

### WebSocket 消息流

1. **初始化**：连接后立即发送 `{user, format}`（用户名 + 编码格式）
2. **控制消息**（前端→后端）：JSON，字段均为可选指针 `ctrlMsg`
   - `screen/quality/maxw/webcodecs/fps` — 流参数
   - `control` — 请求/释放控制权
   - `mx/my/mb/md/rx/ry/key/down/text` — 输入事件
3. **二进制帧**（后端→前端）：H.264 Annex B 裸流 或 MJPEG `[24B meta + JPEG]`
4. **格式切换**：`{format, quality, maxw, fps}` — 初始连接或 ffmpeg 重启时推送
5. **性能统计**（每秒）：`{fps, enc_ms, kb, owner, q, w, h, ox, oy, zoom, screens, maxrate}`
6. **控制状态**：`{control_status, control_msg}` — granted/denied/busy/pending

### ffmpeg 会话池（核心）

```
ffPool[displayID] → *ffSession（每显示器一个 ffmpeg 进程，多用户共享）
ffRefs[displayID] → int（引用计数，所有用户断开时停进程）
```

- `acquireFFmpeg`：获取或创建会话（参数匹配则复用，否则用池中现有参数）
- `restartFFmpeg`：**仅控制者**调用，停止旧会话并用新参数重建。**必须在调用前通过 releaseFFmpeg 释放自己的引用**，且必须保留其他订阅者的引用计数迁移到新会话
- `releaseFFmpeg`：减引用，至 0 时停止进程并清理
- Fan-out goroutine：将 `frameCh` 的每帧复制给所有订阅者独立通道，解决多用户共享时 Go channel 单消费者问题
- 池参数（`ffPoolQ/MW/FPS/H264`）在 `acquireFFmpeg` 后同步回调用方的 atomic 变量，确保非控制者的本地追踪变量与池一致

### 编码器回退链

`h264Encoders` 按优先级排列：GPU 编码器 → `libx264`（末尾）。编码失败时 `tryNextH264Encoder()` 递增索引回退。所有编码器耗尽后 `useH264=false` 回退到 MJPEG。H.264 reader 异常退出时发送 `nil` 触发自动回退重试。

### 前端组件树

```
App.vue
├── DesktopControls.vue   # 桌面顶栏（屏幕选择、控制权、画质/分辨率/编码/FPS）
├── ScreenCanvas.vue      # canvas 渲染 + 鼠标事件 + 解码器管理
├── MobileControls.vue    # 移动端触控 + 分辨率选项
├── ConnectionOverlay.vue # 断线重连覆盖层（倒计时/手动重连）
```

### 关键数据流

- **帧渲染**：`useWebSocket.registerBinaryHandler` → `ScreenCanvas.handleBinary` → `h264Decoder.feed` / `jpegDecoder.feed` → rAF 绘制到 canvas
- **坐标映射**：`useCoordinateMapping.screenCoords` 将浏览器像素映射到远程桌面物理坐标，考虑 letterbox/pillarbox 黑边 + DPI 缩放（`meta.ox/oy/zoom`）
- **Canvas 尺寸策略**：CSS `width/height: 100%` 保证画布始终填满容器，分辨率变更只影响画质/带宽，不改变显示尺寸

## 重要约定

- 解码器创建/关闭必须在 `ScreenCanvas` 生命周期内（`onMounted`/`onUnmounted`）
- `streamFormat` 切换时调用 `resetDecoders()`，格式消息到达前 `connectionStatus = 'switching'`
- 后端 `current*` 变量使用 `atomic.Int32/Bool`，参数变化检查 `paramsChanged` 必须在主循环原子读取后立即计算
- `restartFFmpeg` 的调用者必须先 `ff.unsubscribe(subID)` + `releaseFFmpeg(curScreen)` 再调用
- 非控制者进入 `acquireFFmpeg` 路径后，池同步代码会将 atomic 变量覆写为池中实际值（这是预期行为）
- Win32 UI（权限弹窗）必须 `runtime.LockOSThread()` + 消息循环结束 `UnlockOSThread()`

## 待实现

- [ ] **多流独立编码**：当前同显示器所有用户共享一个 ffmpeg 进程，参数由控制者决定。改为 `pool[{displayID, userID}]` 每用户独立 ffmpeg（或至少控制者独立），使不同用户可使用不同分辨率/画质/帧率。需评估 GPU 编码器并发能力（NVENC 通常支持 2-3 路）
