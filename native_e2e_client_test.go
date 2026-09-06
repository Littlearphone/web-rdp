package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestNativeE2EWebSocket 连接真实 -native-encode 服务器（端口从 WRDP_PORT 读，默认 9321），
// 端到端验证：收到 init/统计 JSON + H.264 AnnexB 二进制帧。headless，不依赖浏览器。
func TestNativeE2EWebSocket(t *testing.T) {
	port := os.Getenv("WRDP_PORT")
	if port == "" {
		port = "9321"
	}
	url := "ws://127.0.0.1:" + port + "/ws?h264=1&user=e2e-native&password=0"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Skipf("无法连接 native 服务器 %s: %v", url, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(10 * time.Second)
	binFrames := 0
	hasNAL := false
	gotJSON := false
	for time.Now().Before(deadline) && binFrames < 8 {
		conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			binFrames++
			if !hasNAL && hasNALStart(data) {
				hasNAL = true
			}
			if binFrames == 1 {
				t.Logf("首帧 len=%d 前8=% X", len(data), headBytes(data, 8))
			}
		} else if mt == websocket.TextMessage {
			var probe map[string]interface{}
			if json.Unmarshal(data, &probe) == nil {
				if _, ok := probe["format"]; ok {
					gotJSON = true
				}
			}
		}
	}
	t.Logf("native E2E: JSON=%v 二进制帧=%d 含NAL=%v", gotJSON, binFrames, hasNAL)
	if binFrames == 0 || !hasNAL {
		t.Fatalf("未端到端收到 H.264 帧 (json=%v bin=%d nal=%v)", gotJSON, binFrames, hasNAL)
	}
}

func headBytes(b []byte, n int) []byte {
	if n > len(b) {
		n = len(b)
	}
	return b[:n]
}

// TestNativeE2EFps 连接 native 服务器读约 3 秒，统计收到的 H.264 二进制帧数，
// 量化端到端投递帧率（headless）。
func TestNativeE2EFps(t *testing.T) {
	port := os.Getenv("WRDP_PORT")
	if port == "" {
		port = "9321"
	}
	url := "ws://127.0.0.1:" + port + "/ws?h264=1&user=e2e-fps&password=0"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Skipf("无法连接 native 服务器: %v", err)
	}
	defer conn.Close()

	// 预热 500ms，随后计时数帧
	time.Sleep(500 * time.Millisecond)
	count := 0
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage && len(data) > 0 {
			count++
		}
	}
	el := time.Since(start).Seconds()
	fps := float64(count) / el
	t.Logf("native 投递帧率 ≈ %.1f fps（%d 帧 / %.2fs）", fps, count, el)
}
