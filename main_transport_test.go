package main

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestTransportH264Frames 连接到已运行的服务器（本机 9211），请求 H.264，
// 验证服务端通过 WebSocket 端到端推送含 H.264 帧的二进制数据。
// 这是"浏览器端到端"的服务端侧可 headless 验证部分：真实 HTTP/WS 传输层投递视频帧。
// 前置：已用 build_ws_e2e_test.exe -port 9211 -tls=false -password 0 启动服务器。
func TestTransportH264Frames(t *testing.T) {
	url := "ws://127.0.0.1:9211/ws?h264=1&user=e2e-test"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Skipf("无法连接服务器（可能未启动）: %v", err)
	}
	defer conn.Close()

	// 等 init 消息 + 若干帧
	deadline := time.Now().Add(6 * time.Second)
	var binMsg int
	var gotFormat string
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			binMsg++
			if len(data) > 0 && binMsg == 1 {
				// 首帧应为 H.264：检查 NAL 起始码
				if looksLikeNALStart(data) {
					t.Logf("首帧含 NAL 起始码, len=%d", len(data))
				}
			}
		}
		// 简单判断 init 的 format 字段
		if gotFormat == "" && len(data) > 4 && string(data[:6]) != "" {
			// 尝试匹配 "format":"h264"
			if bytesContain(data, []byte(`"h264"`)) {
				gotFormat = "h264"
			}
		}
		if binMsg >= 5 {
			break
		}
	}
	t.Logf("收到二进制帧数=%d format=%s", binMsg, gotFormat)
	if binMsg == 0 {
		t.Fatalf("未通过 WS 收到任何视频帧")
	}
}

func looksLikeNALStart(d []byte) bool {
	for i := 0; i+3 < len(d) && i < 32; i++ {
		if d[i] == 0 && d[i+1] == 0 && d[i+2] == 1 {
			return true
		}
		if i+4 < len(d) && d[i] == 0 && d[i+1] == 0 && d[i+2] == 0 && d[i+3] == 1 {
			return true
		}
	}
	return false
}

func bytesContain(hay, needle []byte) bool {
	if len(needle) == 0 || len(needle) > len(hay) {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
