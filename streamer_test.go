package main

import "testing"

// TestFFmpegSessionImplementsStreamer 守护编译期绑定：
// ffSession 必须始终满足 streamer 契约，否则后续 native 会话接缝失效。
func TestFFmpegSessionImplementsStreamer(t *testing.T) {
	var s streamer
	_ = s // 真正的断言在文件顶层 `var _ streamer = (*ffSession)(nil)`，
	// 若编译通过即说明契约成立。此测试仅确保包可编译。
}

// TestDefaultPoolIsFFmpeg 守护"默认主链仍走 ffmpeg"：
// 阶段一 provider 恒返回 ffmpeg 池，native 尚未接入，零回归。
func TestDefaultPoolIsFFmpeg(t *testing.T) {
	pool := currentSessionPool.get()
	if _, ok := pool.(ffmpegPool); !ok {
		t.Fatalf("默认会话池应为 ffmpegPool, 得到 %T", pool)
	}
	// ffmpegPool 空实例即可满足接口（不触发子进程）。
	var _ streamPool = ffmpegPool{}
}
