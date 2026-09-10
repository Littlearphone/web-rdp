// 采集速率探针：只做 DXGI 采集，统计每秒真正拿到多少"桌面已变化"的帧。
// 用来独立验证画面变化发生器是否真的在按目标速率制造变化。
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"web-rdp/native"
)

func main() {
	display := flag.Int("display", 0, "显示索引")
	secs := flag.Int("secs", 5, "测量秒数")
	flag.Parse()

	c, err := native.NewDesktopCapture(*display)
	if err != nil {
		fmt.Println("✗ 打开桌面直捕失败:", err)
		os.Exit(1)
	}
	defer c.Close()

	// 预热
	for i := 0; i < 5; i++ {
		c.AcquireBGRA(500)
	}

	frames, timeouts := 0, 0
	start := time.Now()
	deadline := start.Add(time.Duration(*secs) * time.Second)
	for time.Now().Before(deadline) {
		b, _, _, e := c.AcquireBGRA(200)
		if e != nil {
			fmt.Println("采集错误:", e)
			break
		}
		if b == nil {
			timeouts++
			continue
		}
		frames++
	}
	el := time.Since(start).Seconds()
	fmt.Printf("采集到变化帧 %d 个 / %.2fs = %.1f Hz（空闲超时 %d 次）\n", frames, el, float64(frames)/el, timeouts)
}
