// 端到端管线基准：真实"采集 → 缩放 → 色彩转换 → NVENC 编码"逐帧串行/流水线对比。
// 目的：量化 2K 高分屏上当前实现的每帧成本，找出提帧降延迟的最大收益点。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"web-rdp/native"
)

type stage struct {
	name string
	durs []time.Duration
}

func (s *stage) add(d time.Duration) { s.durs = append(s.durs, d) }

func (s *stage) report() {
	if len(s.durs) == 0 {
		fmt.Printf("  %-22s 无样本\n", s.name)
		return
	}
	cp := append([]time.Duration(nil), s.durs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	p := func(q float64) float64 { return float64(cp[int(float64(len(cp)-1)*q)].Microseconds()) / 1000 }
	sum := time.Duration(0)
	for _, d := range cp {
		sum += d
	}
	avg := float64(sum.Microseconds()) / 1000 / float64(len(cp))
	fmt.Printf("  %-22s n=%-5d 平均 %6.2f ms  p50 %6.2f  p95 %6.2f  max %6.2f  → 上限 %5.1f fps\n",
		s.name, len(cp), avg, p(0.50), p(0.95), p(1.0), 1000/avg)
}

func main() {
	display := flag.Int("display", 0, "显示索引")
	targetW := flag.Int("w", 2560, "编码目标宽度")
	fpsTarget := flag.Int("fps", 0, "目标帧率(0=不限)")
	secs := flag.Int("secs", 6, "测量时长")
	flag.Parse()
	runtime.GOMAXPROCS(runtime.NumCPU())

	capture := &stage{name: "① 采集+GPU回读"}
	cvt := &stage{name: "② 融合缩放+色彩转换"}
	encS := &stage{name: "③ NVENC 编码"}
	e2e := &stage{name: "E2E 采集→编码产出"}
	workers := native.OptimalNV12Workers(0) // 稍后按实际 th 重算
	var refFrame []byte
	var refW, refH int

	capSrc, err := native.NewDesktopCapture(*display)
	if err != nil {
		fmt.Println("✗ 打开桌面直捕失败:", err)
		os.Exit(1)
	}
	defer capSrc.Close()

	// 预热确定尺寸
	var w, h int
	for i := 0; i < 5 && w == 0; i++ {
		b, cw, ch, e := capSrc.AcquireBGRA(500)
		if e != nil {
			fmt.Println("✗", e)
			os.Exit(1)
		}
		if b != nil {
			w, h = cw, ch
		}
	}
	if w == 0 {
		fmt.Println("✗ 桌面静止取不到帧（请先运行 Motion.exe 制造画面变化）")
		os.Exit(1)
	}
	tw, th := w, h
	if *targetW > 0 && tw > *targetW {
		th = h * *targetW / w
		tw = *targetW
	}
	if tw%2 != 0 {
		tw--
	}
	if th%2 != 0 {
		th--
	}
	fmt.Printf("=== 端到端管线基准 ===\n源 %dx%d → 编码 %dx%d\n\n", w, h, tw, th)
	workers = native.OptimalNV12Workers(th)

	// ── 内核 A/B：旧两步 vs 新融合（串行/并行），含逐字节一致性 ──
	// 用一个真实采集帧，对"等尺寸"和"需缩放"两种目标分别测。
	if fr, fw, fh, e := capSrc.AcquireBGRA(2000); e == nil && fr != nil {
		fmt.Println("内核对比（真实采集帧 " + fmt.Sprint(fw) + "x" + fmt.Sprint(fh) + "）:")
		for _, cfg := range []struct{ name string; tw, th int }{
			{"等尺寸(原始分辨率)", fw, fh},
			{fmt.Sprintf("缩放到 %dx%d", tw, th), tw, th},
		} {
			cw, ch := cfg.tw, cfg.th
			if cw%2 != 0 {
				cw--
			}
			if ch%2 != 0 {
				ch--
			}
			// 旧实现（与生产等价）：等尺寸时 scaleBGRA 直接返回原帧、不做缩放，
			// 只有需要缩小时才先 DownscaleBGRA 再转换。
			var oldOut []byte
			tOld := timeKernel(6, func() {
				small := fr
				if fw != cw || fh != ch {
					small = native.DownscaleBGRA(fr, fw, fh, cw, ch)
				}
				oldOut = native.BGRAToNV12Into(nil, small, cw, ch)
			})
			// 新融合串行
			var serOut []byte
			tSer := timeKernel(6, func() {
				serOut = native.DownscaleBGRAToNV12Into(nil, fr, fw, fh, cw, ch)
			})
			// 新融合并行
			var parOut []byte
			tPar := timeKernel(6, func() {
				parOut = native.DownscaleBGRAToNV12ParallelInto(nil, fr, fw, fh, cw, ch, workers)
			})
			fmt.Printf("  %-22s 旧两步 %6.2fms | 新串行 %6.2fms | 新并行(%d线程) %6.2fms\n",
				cfg.name, ms(tOld), ms(tSer), workers, ms(tPar))
			fmt.Printf("  %-22s 一致性: 串行 vs 旧 %s | 并行 vs 旧 %s\n", "",
				byteDiff(oldOut, serOut), byteDiff(oldOut, parOut))
		}
		fmt.Println()
	}

	// NVENC 编码器（与 nativeSession 相同：异步 Push/Next 流水线）
	br := native.EstimateBitrateForQuality(tw, th, 80)
	enc, err := native.NewAsyncMFH264Encoder(tw, th, br)
	if err != nil {
		fmt.Println("✗ 硬件编码器不可用:", err)
		os.Exit(1)
	}
	defer enc.Close()

	type encOut struct {
		t    time.Time
		size int
	}
	outCh := make(chan encOut, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			b, e := enc.Next()
			if e != nil {
				return
			}
			select {
			case outCh <- encOut{time.Now(), len(b)}:
			default:
			}
		}
	}()

	var nv12 []byte
	var pushAt []time.Time // push 时间戳队列（与输出顺序一致）
	interval := time.Duration(0)
	if *fpsTarget > 0 {
		interval = time.Second / time.Duration(*fpsTarget)
	}
	deadline := time.Now().Add(time.Duration(*secs) * time.Second)
	lastPush := time.Time{}
	produced := 0

	for time.Now().Before(deadline) {
		t0 := time.Now()
		bgra, cw, ch, e := capSrc.AcquireBGRA(2000)
		if e != nil {
			fmt.Println("采集错误:", e)
			continue
		}
		if bgra == nil {
			continue // 静止
		}
		if refFrame == nil {
			refFrame = append([]byte(nil), bgra...) // 留一帧真实数据做正确性对照
			refW, refH = cw, ch
		}
		capture.add(time.Since(t0))

		if interval > 0 && !lastPush.IsZero() {
			if d := interval - time.Since(lastPush); d > 0 {
				time.Sleep(d)
			}
		}
		lastPush = time.Now()

		// 融合"缩放+色彩转换"（与 native_session.go 生产路径完全一致：
		// 传 nil 表示每帧新分配 NV12 缓冲——异步编码器的 Push 只入队，
		// 缓冲复用会导致帧撕裂，因此生产路径必须每帧新建）
		t2 := time.Now()
		nv12 = native.DownscaleBGRAToNV12ParallelInto(nil, bgra, cw, ch, tw, th, workers)
		cvt.add(time.Since(t2))

		t3 := time.Now()
		ok := enc.Push(nv12)
		encS.add(time.Since(t3))
		if ok {
			pushAt = append(pushAt, lastPush)
			produced++
		}

		// 尽量取走输出，记录端到端延迟
		for {
			select {
			case o := <-outCh:
				if len(pushAt) > 0 {
					e2e.add(o.t.Sub(pushAt[0]))
					pushAt = pushAt[1:]
				}
			default:
				goto drained
			}
		}
	drained:
	}

	// 排空
	drainUntil := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(drainUntil) {
		select {
		case o := <-outCh:
			if len(pushAt) > 0 {
				e2e.add(o.t.Sub(pushAt[0]))
				pushAt = pushAt[1:]
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	fmt.Println("各阶段单帧成本（越小越好）:")
	capture.report()
	cvt.report()
	encS.report()
	e2e.report()

	serial := lastAvg(capture) + lastAvg(cvt)
	fmt.Printf("\nCPU 串行段（采集 + 融合转换）= %.2f ms → 单线程上限 %.1f fps\n", serial, 1000/serial)
	fmt.Printf("端到端上限 %.1f fps（含 NVENC 流水线）\n", 1000/lastAvg(e2e))
	fmt.Printf("融合转换并发度 = %d 线程\n", workers)

	// 正确性：用真实采集帧对照 融合 vs 两步，必须逐字节一致
	if refFrame != nil {
		mid := native.DownscaleBGRA(refFrame, refW, refH, tw, th)
		ref := native.BGRAToNV12Into(nil, mid, tw, th)
		got := native.DownscaleBGRAToNV12ParallelInto(nil, refFrame, refW, refH, tw, th, workers)
		diff, maxd := 0, 0
		for i := 0; i < len(ref) && i < len(got); i++ {
			d := int(ref[i]) - int(got[i])
			if d < 0 {
				d = -d
			}
			if d != 0 {
				diff++
			}
			if d > maxd {
				maxd = d
			}
		}
		verdict := "✓ 逐字节一致"
		if diff != 0 {
			verdict = "✗ 不一致"
		}
		fmt.Printf("\n正确性对照（真实采集帧 %dx%d）：融合并发 vs 旧两步，不同字节 %d/%d (最大偏差 %d) %s\n",
			refW, refH, diff, len(ref), maxd, verdict)
	}
}

func lastAvg(s *stage) float64 {
	if len(s.durs) == 0 {
		return 0
	}
	sum := time.Duration(0)
	for _, d := range s.durs {
		sum += d
	}
	return float64(sum.Microseconds()) / 1000 / float64(len(s.durs))
}

// timeKernel 跑 n 次取中位数耗时（先预热）。
func timeKernel(n int, fn func()) time.Duration {
	fn()
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t := time.Now()
		fn()
		ds = append(ds, time.Since(t))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// byteDiff 比较两份 NV12 输出，返回可读结论。
func byteDiff(a, b []byte) string {
	if len(a) != len(b) {
		return fmt.Sprintf("✗ 长度不同 %d vs %d", len(a), len(b))
	}
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
		}
	}
	if diff == 0 {
		return "✓ 逐字节一致"
	}
	return fmt.Sprintf("✗ 不同 %d/%d", diff, len(a))
}
