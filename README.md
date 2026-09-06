# web-rdp — Web 远程桌面控制

通过浏览器远程控制 Windows 桌面。Go 后端捕获屏幕，优先通过 **WebRTC (UDP/RTP)** 推流，WebSocket (TCP) 作为信令通道和视频回退。Vue 3 前端解码渲染并捕获用户输入回传。

## ⚠️ AI 生成声明

**本项目全部代码由生成式 AI（Anthropic Claude Code）生成，未经人工编写或审查。** 使用者应自行评估代码质量、安全性和适用性。本项目不提供任何形式的保证或担保。

## 核心能力

- 📺 **屏幕查看** — H.264 优先（进程内 native/MF，硬件异步 MFT → 软件 MF），纯 Go JPEG 回退
- 🖱️ **输入转发** — 键盘、鼠标（左键/右键/拖拽）、触控
- 📋 **剪贴板同步** — 双向文本 + 图像剪贴板
- 🔐 **权限管理** — 单用户控制权 + 密码认证 + Win32 深色弹窗审批

## 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.26, gorilla/websocket, pion/webrtc/v4, Win32 syscall API |
| 前端 | Vue 3 + TypeScript + Pinia + Naive UI, Vite 8, RTCPeerConnection API |
| 编码 | 进程内 native(MF) H.264（硬件异步 MFT → 软件 MF），纯 Go JPEG 兜底, WebCodecs API |
| 传输 | WebRTC (UDP/RTP 优先) + WebSocket (TCP/信令/回退)，内网直连无 STUN/TURN |
| 部署 | 单一 exe，前端静态文件通过 `//go:embed static` 内嵌 |

## 快速开始

### 前置条件

- Windows 10/11（需要支持 H.264 的 Media Foundation 编码器 MFT；无 GPU 时走软件 MF，极少数情况无任何编码器则回退 JPEG）
- Go 1.26+
- Node.js 22+（仅开发/构建前端时需要）
- 无需 ffmpeg（已完全移除，无检测/下载/进程管道）

### 开发模式（前后端分离）

推荐用根目录 `package.json`（pnpm workspace）一键起前后端：

```bash
pnpm install                      # 安装根 + views 依赖（含 concurrently）
pnpm dev                          # 同时起前端 Vite(:5173) 与 Go 后端(:9000)
```

等价手动手动：

```bash
# 终端 1 — 前端 Vite 开发服务器（:5173，WebSocket 代理到 :9000）
cd views && pnpm install && pnpm run dev

# 终端 2 — Go 后端（backend/，模块根 :9000）
cd backend && go run .            # backend/static 缺失时会编译失败，需先构建一次前端
```

### 一键构建

```bash
# 顶层脚本：前端 views -> backend/static，再编译 backend -> dist/web-rdp.exe
pnpm run build                    # 或直接双击 scripts\build.bat
pnpm run build:be                 # 仅重编后端（假定前端已构建）
```

手工等价：

```bash
# 1. 构建前端（输出到 backend/static，供 go:embed）
cd views && pnpm run build

# 2. 编译单一可执行文件（-s -w -trimpath 显著减小体积）
cd backend && go build -ldflags "-s -w" -trimpath -o ../dist/web-rdp.exe .
```

### 运行选项

```bash
web-rdp.exe -port 8080           # 指定端口（默认 443）
web-rdp.exe -tls=false           # 禁用 HTTPS（开发用）
web-rdp.exe -password <pwd>      # 设置访问密码
web-rdp.exe -probe-native        # 探测进程内 MF/GPU 编码后端
web-rdp.exe -mf-encode           # 单帧 MF H.264 编码冒烟
```

> 说明：已完全移除 ffmpeg 依赖与下载。H.264 唯一来源为进程内 native(MF) 编码
> （硬件异步 MFT 优先，软件 MF 回退，无 GPU 亦可），产不出帧时自动回退纯 Go JPEG。


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
│  Go 后端 (backend/)                                     │
│  ws.go: 连接 + 档位调度 (分辨率/帧率)                    │
│  tiers.go: 每显示器共享 DXGI 采集 + 档位会话(共享编码)   │
│  webrtc.go: per-tier PeerConnection · native_session MF │
│  screen.go: 纯 Go JPEG 回退                             │
└─────────────────────────────────────────────────────┘
```

## 编码回退链（native 唯一 H.264 源）

```
硬件异步 MFT（NativeAsyncMFH264Encoder，跨厂商，优先）
  → 软件同步 MF（NewMFH264EncoderQ，无 GPU 亦可）
    → 两者都失败：会话广播 EOF → ws.go 落纯 Go JPEG（screenshot + image/jpeg，限速 60fps）
```

## 已知限制

- 无双指缩放、双指滚动、长按右键（移动端）
- 无音频传输
- 无文件传输
- 单用户控制（同一时间仅一人可操作）
- 同屏多档位：每显示器共享一路 DXGI 采集（`tiers.go` broker），按档位(分辨率,帧率)各编码；同档位多观众共享同一路编码（仅多订阅 fan-out）。硬件编码器 MFT 并发有限(约 2–3 路)时，多余档位回退软件 MF 或纯 Go JPEG。
- WebSocket 无连接限速/IP 冷却

## 许可证

本项目采用 [Apache License 2.0](LICENSE)。

---

*⚠️ 本项目全部代码由生成式 AI 生成，未经人工审查。使用者自行承担风险。*
