# web-rdp — Web 远程桌面控制

通过浏览器远程控制 Windows 桌面。Go 后端捕获屏幕，优先通过 **WebRTC (UDP/RTP)** 推流，WebSocket (TCP) 作为信令通道和视频回退。Vue 3 前端解码渲染并捕获用户输入回传。

## ⚠️ AI 生成声明

**本项目全部代码由生成式 AI（Anthropic Claude Code）生成，未经人工编写或审查。** 使用者应自行评估代码质量、安全性和适用性。本项目不提供任何形式的保证或担保。

## 核心能力

- 📺 **屏幕查看** — H.264 硬编码优先（NVENC/AMF/QSV），MJPEG 软件回退
- 🖱️ **输入转发** — 键盘、鼠标（左键/右键/拖拽）、触控
- 📋 **剪贴板同步** — 双向文本 + 图像剪贴板
- 🔐 **权限管理** — 单用户控制权 + 密码认证 + Win32 深色弹窗审批

## 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.26, gorilla/websocket, pion/webrtc/v4, Win32 syscall API |
| 前端 | Vue 3 + TypeScript + Pinia + Naive UI, Vite 8, RTCPeerConnection API |
| 编码 | ffmpeg (H.264 NVENC/AMF/QSV/libx264, MJPEG), WebCodecs API |
| 传输 | WebRTC (UDP/RTP 优先) + WebSocket (TCP/信令/回退)，内网直连无 STUN/TURN |
| 部署 | 单一 exe，前端静态文件通过 `//go:embed static` 内嵌 |

## 快速开始

### 前置条件

- Windows 10/11
- Go 1.26+
- Node.js 20+（仅开发/构建前端时需要）
- [ffmpeg](https://ffmpeg.org/)（可选，后端会自动检测已安装的 ffmpeg 或自动下载）

### 开发模式（前后端分离）

```bash
# 终端 1 — 前端 Vite 开发服务器（:5173，WebSocket 代理到 :9000）
cd views && npm install && npm run dev

# 终端 2 — Go 后端（:9000）
go run .
```

### 生产构建

```bash
# 1. 构建前端（输出到 static/）
cd views && npm run build

# 2. 编译单一可执行文件
go build .
```

### 运行选项

```bash
web-rdp.exe -port 8080           # 指定端口（默认 443）
web-rdp.exe -tls=false           # 禁用 HTTPS（开发用）
web-rdp.exe -password <pwd>      # 设置访问密码
web-rdp.exe -ffmpeg <path>       # 手动指定 ffmpeg 路径
web-rdp.exe -proxy :7890         # 通过代理下载 ffmpeg
```

## 架构概览

```
┌─────────────────────────────────────────────────────┐
│  浏览器 (Vue 3)                                      │
│  ScreenCanvas → WebCodecs / RTCPeerConnection        │
│  DesktopControls / MobileControls → 输入事件          │
└────────────┬────────────────────────────┬────────────┘
             │ WebRTC (UDP/RTP)           │ WebSocket (TCP)
             │ 视频优先                    │ 信令 + 回退视频
             ▼                             ▼
┌─────────────────────────────────────────────────────┐
│  Go 后端                                             │
│  main.go → ws.go (连接管理) + webrtc.go (Peer)       │
│  ffmpeg_pipeline.go (进程池 + 帧分发)                 │
│  screen.go → Win32 屏幕捕获                          │
└─────────────────────────────────────────────────────┘
```

## 编码器回退链

H.264 编码器按优先级自动回退：

```
h264_nvenc (NVIDIA) → h264_amf (AMD) → h264_qsv (Intel) → libx264 (软件)
                                                                    ↓
                                                              MJPEG (ffmpeg)
                                                                    ↓
                                                        纯 Go image/jpeg（最终回退）
```

## 已知限制

- 无双指缩放、双指滚动、长按右键（移动端）
- 无音频传输
- 无文件传输
- 单用户控制（同一时间仅一人可操作）
- WebSocket 无连接限速/IP 冷却

## 许可证

本项目采用 [Apache License 2.0](LICENSE)。

---

*⚠️ 本项目全部代码由生成式 AI 生成，未经人工审查。使用者自行承担风险。*
