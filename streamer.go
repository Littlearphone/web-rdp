package main

import "web-rdp/native"

// ── streamer 抽象：native 会话与 ffmpeg 会话的解耦边界 ──
//
// 目标：让上层（未来 ws.go 的会话循环）只依赖本接口，而不用关心帧来源是
// ffmpeg 子进程还是进程内 Media Foundation / 厂商 SDK 编码。
// 同时严格保持**默认主链仍走 ffmpeg**（零回归）：本文件中 ffmpeg 会话的实现
// 即现有 ffSession，native 会话属阶段二/三，暂不接入。
//
// 接入策略（见 docs/native-no-ffmpeg-design.md §4.1 的"最小侵入"建议）：
//   当前 ws.go 仍直接持有 *ffSession；待真实 native Encoder 可用并经真机推流
//   验证后再把 ws.go 的主循环收敛到本接口。在此之前，本接口只做契约声明 +
//   编译期绑定，保证 `*ffSession` 始终满足它，一旦后续新增 native 会话可平滑切换。

// streamer 描述一个"能给单个订阅者持续产出编码帧"的会话的对外契约。
// 语义取自现有 ffSession 的订阅模型：
//   多用户各自 subscribe 拿到独立帧通道，fan-out 每帧复制给所有订阅者，
//   通道满时丢旧保新以确保低延迟。
type streamer interface {
	// subscribe 注册订阅者，返回 (订阅ID, 独立帧通道)。同现有 ffSession.subscribe。
	subscribe() (int, <-chan []byte)
	// unsubscribe 注销订阅者并关闭其通道。
	unsubscribe(id int)
	// stop 停止会话并清理资源（含关闭 frameCh、回收子进程等）。
	stop()
	// hasSentFrames 报告会话是否成功送出过帧（用于编码器可用性/超时判断）。
	hasSentFrames() bool
	// sourceDisplay 返回会话绑定的显示器 ID。
	sourceDisplay() int
	// h264Mode 返回会话是否输出 H.264（否则为 MJPEG）。
	h264Mode() bool
}

// 编译期绑定：*ffSession 必须始终满足 streamer，否则后续 native 会话接缝会失效。
// 若 ffSession 方法签名变更导致此处编译失败，说明接口契约需同步更新。
var _ streamer = (*ffSession)(nil)

// 会话池的对外视图（对应现有 acquireFFmpeg/restartFFmpeg/releaseFFmpeg 语义）。
// 后续 native 会话池接入时，可将 acquire/release 收敛到同一组高层入口，
// 由内部按探测结果选择 ffmpeg 或 native 实现。此处仅声明，不改变现有调用。
type streamPool interface {
	// acquire 获取或创建指定显示器会话（参数匹配复用 + 引用计数）。
	acquire(displayID, quality, maxW, fps int, h264 bool) streamer
	// restart 强制重建（仅控制者调用）。语义见 restartFFmpeg。
	restart(displayID, quality, maxW, fps int, h264 bool) streamer
	// release 释放引用，归零时停止会话并清理。
	release(displayID int)
}

// ── 默认实现：ffmpeg 会话池（保留为主链，零回归）──
//
// ffmpegPool 把现有 package-level 的 acquireFFmpeg/restartFFmpeg/releaseFFmpeg
// 收敛到一个满足 streamPool 的具体类型，作为默认 provider 注册。
// ws.go 仍直接调用原函数（行为不变）；此适配器仅用于在引入 native 会话池时，
// 让上层能在 "ffmpeg provider" 与 "native provider" 之间选择，而默认恒为 ffmpeg。

// defaultPool 是全局默认（ffmpeg）会话池。现阶段恒用 ffmpeg。
type ffmpegPool struct{}

// acquire 复用现有 acquireFFmpeg，返回满足 streamer 的 *ffSession。
func (ffmpegPool) acquire(displayID, quality, maxW, fps int, h264 bool) streamer {
	return acquireFFmpeg(displayID, quality, maxW, fps, h264)
}

// restart 复用现有 restartFFmpeg。
func (ffmpegPool) restart(displayID, quality, maxW, fps int, h264 bool) streamer {
	return restartFFmpeg(displayID, quality, maxW, fps, h264)
}

// release 复用现有 releaseFFmpeg。
func (ffmpegPool) release(displayID int) { releaseFFmpeg(displayID) }

// 编译期绑定：ffmpegPool 必须满足 streamPool。
var _ streamPool = ffmpegPool{}

// sessionPoolProvider 提供统一的会话池入口，让上层在 native 与 ffmpeg 间切换。
// 默认恒为 ffmpeg（主链不变）；仅当 forceNative 置位时尝试 native 编码会话。
type sessionPoolProvider struct {
	// ffmpeg 恒可用，作为兜底与主链。
	ffmpeg ffmpegPool
	// forceNative 为 true 时优先 native 编码会话（实验/验证用，默认 false）。
	forceNative bool
}

// get 返回当前应使用的会话池。
// 默认恒为 ffmpeg（零回归）。forceNative 且 H.264 时才可切 native（由调用方保证 h264=true）。
func (p *sessionPoolProvider) get() streamPool {
	if p.forceNative {
		return nativePool{}
	}
	return p.ffmpeg
}

// nativePool 提供 native 编码会话（进程内 DXGI→MF），供待办 C 验证。
type nativePool struct{}

// acquire 创建 native 编码会话。maxW/fps 语义与 ffmpeg 对齐。
func (nativePool) acquire(displayID, quality, maxW, fps int, h264 bool) streamer {
	if !h264 {
		// native 当前只做 H.264；非 H.264 退回 ffmpeg
		return acquireFFmpeg(displayID, quality, maxW, fps, h264)
	}
	return newNativeSession(displayID, quality, maxW, fps)
}

func (nativePool) restart(displayID, quality, maxW, fps int, h264 bool) streamer {
	if !h264 {
		return restartFFmpeg(displayID, quality, maxW, fps, h264)
	}
	return newNativeSession(displayID, quality, maxW, fps)
}

func (nativePool) release(displayID int) {
	// native 会话引用计数由会话自身管理；此处保持空实现以符合接口。
	// 注意：ws.go 在 release 后会将 ff=nil 并停止对会话的引用；nativeSession.stop
	// 需由会话所有者（或 stop 路径）触发。此处仅作占位，避免破坏接口。
}

// 编译期绑定
var _ streamPool = nativePool{}

// nativeEncodeRequested 表示 H.264 推流应走进程内 native(MF) 会话（由 -native-encode 设置）。
var nativeEncodeRequested bool

// nativeH264Ready 报告 native MF 路径下是否有可用 H.264 编码器 MFT。
func nativeH264Ready() bool {
	return nativeEncodeRequested && native.H264Available()
}

// nativeEncoderAvailable 别名（nativeH264Ready 的直接底层判断）。
func nativeEncoderAvailable() bool { return native.H264Available() }

// h264Available 报告当前是否可走 H.264：ffmpeg 有编码器，或 native MF 路径可用。
func h264Available() bool {
	return currentH264Encoder() != "" || nativeH264Ready()
}

// usingNativePool 报告当前会话池是否为本机 native(MF) 编码池。
func usingNativePool() bool {
	_, ok := currentSessionPool.get().(nativePool)
	return ok
}

// currentSessionPool 是全局可用的会话池 provider。
// ws.go 现有调用不受影响；未来收敛时经由此取得池。
var currentSessionPool = &sessionPoolProvider{ffmpeg: ffmpegPool{}}

