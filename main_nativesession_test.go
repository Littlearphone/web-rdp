package main

import (
	"testing"
	"time"
)

// TestNativeSessionDeliversFrames headless 验证 native 编码会话满足 streamer 契约：
// 构造会话 → subscribe 拿通道 → 持续读到合法 H.264 帧 → stop 清理。
// 证明 native provider 能作为 ws.go 的帧来源（服务端侧待办 C）。
func TestNativeSessionDeliversFrames(t *testing.T) {
	s := newNativeSession(0, 70, 480, 30)
	defer s.stop()

	id, ch := s.subscribe()
	defer s.unsubscribe(id)

	deadline := time.Now().Add(8 * time.Second)
	var frames, nonEmpty int
	var firstNAL bool
	for time.Now().Before(deadline) && frames < 5 {
		select {
		case f := <-ch:
			if f == nil {
				t.Fatalf("收到 nil 帧")
			}
			frames++
			if len(f) > 0 {
				nonEmpty++
			}
			if !firstNAL {
				firstNAL = hasNALStart(f)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("native 会话超时未产帧")
		}
	}
	if nonEmpty == 0 {
		t.Fatalf("native 会话未产出任何 H.264 帧")
	}
	t.Logf("native 会话成功交付 %d 帧(非空 %d), 首帧 NAL=%v", frames, nonEmpty, firstNAL)
}

func hasNALStart(d []byte) bool {
	for i := 0; i+3 < len(d) && i < 64; i++ {
		if d[i] == 0 && d[i+1] == 0 && d[i+2] == 1 {
			return true
		}
		if i+4 < len(d) && d[i] == 0 && d[i+1] == 0 && d[i+2] == 0 && d[i+3] == 1 {
			return true
		}
	}
	return false
}
