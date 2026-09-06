package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestCaptureH264Frame 连接 native 服务器抓取 H.264 裸流写盘，供 ffmpeg 解码查看真实画质。
func TestCaptureH264Frame(t *testing.T) {
	port := "9321"
	url := "ws://127.0.0.1:" + port + "/ws?h264=1&user=cap&password=0"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer conn.Close()
	var buf []byte
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			buf = append(buf, data...)
			if len(buf) > 4_000_000 {
				break
			}
		}
	}
	path := filepath.Join(os.TempDir(), "native_cap.h264")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("captured %d bytes → %s", len(buf), path)
	if len(buf) == 0 {
		t.Fatalf("no frames captured")
	}
}
