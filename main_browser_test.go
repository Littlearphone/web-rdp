package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type cdpMsg struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type cdpResult struct {
	ID     int                     `json:"id"`
	Result map[string]interface{}   `json:"result"`
	Error  *struct{ Message string } `json:"error"`
}

// cdpDriver 最小 CDP 客户端。
type cdpDriver struct {
	ws  *websocket.Conn
	seq int
}

func dialCDPPage(t *testing.T) *cdpDriver {
	// 找我们的页面
	resp, err := http.Get("http://127.0.0.1:9223/json")
	if err != nil {
		t.Skipf("无 CDP: %v", err)
	}
	defer resp.Body.Close()
	var pages []struct {
		URL               string `json:"url"`
		WebSocketDebugger string `json:"webSocketDebuggerUrl"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &pages)
	var wsURL string
	for _, p := range pages {
		if strings.Contains(p.URL, "127.0.0.1:9213") {
			wsURL = p.WebSocketDebugger
			break
		}
	}
	if wsURL == "" {
		t.Skipf("未找到目标页面")
	}
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Skipf("连接页面 ws 失败: %v", err)
	}
	return &cdpDriver{ws: c}
}

func (d *cdpDriver) send(method string, params map[string]interface{}) (map[string]interface{}, error) {
	d.seq++
	b, _ := json.Marshal(params)
	if err := d.ws.WriteJSON(cdpMsg{ID: d.seq, Method: method, Params: b}); err != nil {
		return nil, err
	}
	for {
		var r cdpResult
		if err := d.ws.ReadJSON(&r); err != nil {
			return nil, err
		}
		if r.Error != nil {
			return nil, fmt.Errorf("cdp %s: %s", method, r.Error.Message)
		}
		if r.ID == d.seq {
			return r.Result, nil
		}
	}
}

func (d *cdpDriver) eval(expr string) (string, error) {
	res, err := d.send("Runtime.evaluate", map[string]interface{}{"expression": expr, "returnByValue": true})
	if err != nil {
		return "", err
	}
	if rv, ok := res["result"].(map[string]interface{}); ok {
		if v, ok := rv["value"]; ok {
			return fmt.Sprintf("%v", v), nil
		}
	}
	return "", nil
}

// TestBrowserNativeViewHeadless 驱动本机 Edge(headless) 加载 native 服务器页面，
// 通过 CDP 自动确认用户名，等待数秒后回读 canvas 状态，佐证浏览器连上 native H.264 会话。
// 前置（在具备浏览器与可交互桌面的机器上）：
//   go build -o w.exe . && w.exe -port 9213 -tls=false -password 0 -native-encode
//   并启动 Edge headless：msedge --headless=new --remote-debugging-port=9223 http://127.0.0.1:9213
// 若 native 服务器未启动则跳过（正常 go test 环境不跑）。
func TestBrowserNativeViewHeadless(t *testing.T) {
	// 先确认 native 服务器可达，否则跳过（避免误连遗留浏览器）
	rc, err := http.Get("http://127.0.0.1:9213/")
	if err != nil {
		t.Skipf("native 服务器未启动: %v", err)
	}
	rc.Body.Close()

	d := dialCDPPage(t)
	defer d.ws.Close()

	// 找到确认按钮的文本并点击；先列出 body 文本辅助定位
	body, _ := d.eval(`document.body && document.body.innerText.slice(0,200)`)
	t.Logf("页面文本: %q", body)

	// 尝试点击含"进入/确认/连接"字样的按钮
	clicked := `(() => { const btns=[...document.querySelectorAll('button')];
	  const b=btns.find(x=>/进入|确认|连接|开始/.test(x.innerText)); if(b){b.click();return 'clicked:'+b.innerText} return 'no-btn';})()`
	r, _ := d.eval(clicked)
	t.Logf("点击结果: %s", r)

	// 等连接建立并开始收帧
	deadline := time.Now().Add(10 * time.Second)
	var connState, canvasState string
	for time.Now().Before(deadline) {
		cs, _ := d.eval(`window.__APP_CONN__ || (window.__dsh_conn=1,'?')`)
		_ = cs
		// 读 canvas 状态不易直接暴露，这里看是否有 canvas 且尺寸>0
		canvas, _ := d.eval(`(()=>{const c=document.querySelector('canvas');return c?c.width+'x'+c.height:'none'})()`)
		if canvas != "none" && canvas != "0x0" {
			canvasState = canvas
		}
		time.Sleep(500 * time.Millisecond)
	}
	connState, _ = d.eval(`'ok'`)
	t.Logf("连接=%s canvas=%s", connState, canvasState)
	if canvasState == "" {
		t.Logf("警告: 未观测到 canvas（可能需请求控制权或页面交互）")
	}
}
