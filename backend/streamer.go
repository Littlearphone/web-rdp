package main

import (
	"log"
	"sync"

	"web-rdp/native"
)

// ── streamer 抽象 ──
//
// streamer 描述一个"能给单个订阅者持续产出编码帧"的会话的对外契约。
// ffmpeg 已完全移除，唯一的实现是进程内 DXGI→MF 的 native 会话
// （nativeSession，见 native_session.go）：硬件异步 MFT 优先，软件 MF H.264 回退，
// 两者皆不可用时会话退出，由 ws.go 主循环落到纯 Go JPEG 回退。
type streamer interface {
	// subscribe 注册订阅者，返回 (订阅ID, 独立帧通道)。通道满时丢旧保新。
	subscribe() (int, <-chan []byte)
	// unsubscribe 注销订阅者并关闭其通道。
	unsubscribe(id int)
	// stop 停止会话并清理资源。
	stop()
	// hasSentFrames 报告会话是否成功送出过帧（用于"产不出帧"判定）。
	hasSentFrames() bool
	// sourceDisplay 返回会话绑定的显示器 ID。
	sourceDisplay() int
	// h264Mode 报告会话是否输出 H.264。native 会话恒为 H.264（true）。
	h264Mode() bool
	// ended 报告会话产帧 goroutine 是否已退出（编码器/采集不可用等异常路径）。
	ended() bool
}

// 编译期绑定：*nativeSession 必须满足 streamer。
var _ streamer = (*nativeSession)(nil)

// ── native H.264 可用性探测 ──
//
// ffmpeg 已移除，H.264 的唯一来源是进程内 MF 编码器（硬件异步 MFT 优先，
// 软件同步 MF 回退，故无 GPU 亦可用）。这里惰性探测一次能否实际构建一个
// MF H.264 编码器，据此在连接握手时决定初始格式。探测只在首次需要时执行，
// 避免拖慢无关单测。

var (
	nativeH264ProbeOnce sync.Once
	nativeH264OK        bool
)

// nativeH264Usable 惰性探测本机是否能构建进程内 MF H.264 编码器。
func nativeH264Usable() bool {
	nativeH264ProbeOnce.Do(func() {
		enc, err := native.NewMFH264EncoderQ(320, 240, 30, 0)
		if err != nil {
			log.Printf("[native] MF H.264 编码器探测不可用: %v", err)
			return
		}
		enc.Close()
		nativeH264OK = true
	})
	return nativeH264OK
}

// h264Available 报告当前是否可走 H.264（进程内 MF H.264 编码器可构建）。
func h264Available() bool { return nativeH264Usable() }
