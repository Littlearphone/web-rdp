package main

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestNativeTransportH264Frames 连接 native 模式服务器（-native-encode），验证 H.264 帧
// 经真实 WebSocket 端到端投递到客户端。前置：build 后以 -port 9212 -tls=false -password 0
// -native-encode 启动。
func TestNativeTransportH264Frames(t *testing.T) {
	url := "ws://127.0.0.1:9212/ws?h264=1&user=e2e-native"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Skipf("无法连接 native 服务器: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(8 * time.Second)
	binMsg := 0
	firstNAL := false
	for time.Now().Before(deadline) && binMsg < 6 {
		conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			binMsg++
			if !firstNAL {
				firstNAL = hasNALStart(data)
			}
			if len(data) > 0 && binMsg == 1 {
				t.Logf("native 首帧 len=%d 前8=% X", len(data), data[:min(8, len(data))])
			}
		}
	}
	t.Logf("native 模式: 收到二进制帧=%d 首帧NAL=%v", binMsg, firstNAL)
	if binMsg == 0 || !firstNAL {
		t.Fatalf("native 模式未端到端收到 H.264 帧 (bin=%d nal=%v)", binMsg, firstNAL)
	}
}
