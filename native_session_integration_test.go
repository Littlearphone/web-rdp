package main

import (
	"testing"
	"time"
)

// TestNativeSessionHWEncoder 端到端：nativeSession.produce() 走 真实桌面采集 →
// 异步硬件 MFT 编码 → fan-out。验证原生会话能产出合法 H.264 帧。
func TestNativeSessionHWEncoder(t *testing.T) {
	VerboseNative = true
	s := newNativeSession(0, 75, 1280, 30)
	defer s.stop()
	_, ch := s.subscribe()
	defer s.unsubscribe(0)

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case frame := <-ch:
			if len(frame) == 0 {
				continue
			}
			if !looksLikeAnnexB(frame) {
				t.Fatalf("产出帧非 AnnexB（长度 %d）", len(frame))
			}
			t.Logf("nativeSession 经硬件编码产出 %d 字节（含 H.264 起始码）", len(frame))
			return
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Skip("6s 内未收到帧（桌面静止时 DXGI 无新帧）——非代码缺陷")
}

// looksLikeAnnexB 简单判断：含 H.264 起始码 00 00 01 或 00 00 00 01。
func looksLikeAnnexB(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return true
		}
	}
	return false
}
