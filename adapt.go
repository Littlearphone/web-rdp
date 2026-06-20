package main

import (
	"sync"
	"time"
)

// ═══════════════════════ 自适应码率控制 ═══════════════════════
//
// 根据控制者的网络反馈动态调节编码参数（画质/帧率/分辨率），
// 在用户设定的偏好上限内自动降级以适应网络拥塞。
//
// 两条策略线：
//   - "quality"（偏好画质）：拥塞时优先降帧率，尽量保住画质和分辨率
//   - "smooth"（偏好流畅）：拥塞时优先降画质，尽量保住帧率
//
// 恢复时逐级回升，冷却期内不重复调整防止抖动。
//
// 仅控制者触发自适应；无控制者时参数保持用户设定不变。
// 自适应编码参数通过 adaptParams() 获取，调用方替代用户设定值。

var (
	adaptMu sync.Mutex

	// ── 用户偏好上限（由控制者 UI 设定）──
	adaptUserQ   int    // 画质上限（30-100）
	adaptUserFPS int    // 帧率上限
	adaptUserMW  int    // 分辨率上限（0=原始）
	adaptMode    string // "quality" 或 "smooth"

	// ── 自适应目标值 ──
	adaptQ   int // 当前目标画质（30-100）
	adaptFPS int // 当前目标帧率
	adaptMW  int // 当前目标分辨率（0=原始）

	// ── 状态 ──
	adaptActive   bool // 自适应当前是否在覆盖用户设定
	adaptDegraded bool // 是否处于降级状态

	// ── 防抖 ──
	lastAdaptAt     time.Time         // 上次调整时间
	adaptCooldown   = 5 * time.Second // 最小调整间隔
	lastFeedback    adaptFeedback
	hasFeedback     bool              // 是否收到过网络反馈
	feedbackTimeout = 8 * time.Second // 反馈超时：超过此时间无反馈视为断开，恢复默认
)

// adaptFeedback 前端上报的网络统计数据
type adaptFeedback struct {
	NetFPS   float64 `json:"net_fps"`   // 前端实际接收帧率
	NetQueue int     `json:"net_queue"` // 解码队列深度（取两秒内的最大值）
	Time     time.Time
}

// setAdaptConfig 由控制者连接时调用，设定偏好上限和策略模式。
// 非控制者调用无效。
func setAdaptConfig(userQ, userFPS, userMW int, mode string) {
	adaptMu.Lock()
	defer adaptMu.Unlock()

	if mode != "smooth" {
		mode = "quality" // 默认偏好画质
	}
	adaptUserQ = userQ
	adaptUserFPS = userFPS
	adaptUserMW = userMW
	adaptMode = mode
}

// feedNetworkStats 由 read goroutine 调用，记录前端上报的网络统计。
func feedNetworkStats(fps float64, queue int) {
	adaptMu.Lock()
	defer adaptMu.Unlock()

	lastFeedback = adaptFeedback{
		NetFPS:   fps,
		NetQueue: queue,
		Time:     time.Now(),
	}
	hasFeedback = true
}

// adaptParams 返回当前应使用的编码参数和自适应是否激活。
// 由主循环在重启 ffmpeg 前调用，替代用户设定值。
//
// 返回值：q, fps, mw, active
//   - active=false: 使用返回值等同于用户设定，自适应未介入
//   - active=true: 返回值低于用户设定，自适应正在降级
func adaptParams(userQ, userFPS, userMW int) (int, int, int, bool) {
	adaptMu.Lock()
	defer adaptMu.Unlock()

	// 更新用户上限（用户可能手动调整了滑块）
	if userQ != adaptUserQ || userFPS != adaptUserFPS || userMW != adaptUserMW {
		adaptUserQ = userQ
		adaptUserFPS = userFPS
		adaptUserMW = userMW
		// 用户手动调参 → 重置自适应状态
		if userQ > adaptQ || userFPS > adaptFPS || (userMW > adaptMW && adaptMW > 0) {
			// 用户提高了上限 → 恢复
			adaptQ = userQ
			adaptFPS = userFPS
			adaptMW = userMW
			adaptActive = false
			adaptDegraded = false
		}
	}

	now := time.Now()

	// 反馈超时：长时间无反馈 → 恢复默认
	if hasFeedback && now.Sub(lastFeedback.Time) > feedbackTimeout {
		hasFeedback = false
		if adaptActive {
			adaptQ = userQ
			adaptFPS = userFPS
			adaptMW = userMW
			adaptActive = false
			adaptDegraded = false
			return adaptQ, adaptFPS, adaptMW, true // 需要一次重启恢复
		}
	}

	// 无反馈 → 不介入
	if !hasFeedback {
		return userQ, userFPS, userMW, false
	}

	// 冷却期 → 返回当前自适应值，不重新评估
	if now.Sub(lastAdaptAt) < adaptCooldown {
		if adaptActive {
			return adaptQ, adaptFPS, adaptMW, true
		}
		return userQ, userFPS, userMW, false
	}

	// ── 拥塞检测 ──
	feedbackAge := now.Sub(lastFeedback.Time)
	if feedbackAge > 4*time.Second {
		// 反馈太旧，不评估
		if adaptActive {
			return adaptQ, adaptFPS, adaptMW, true
		}
		return userQ, userFPS, userMW, false
	}

	targetFPS := userFPS
	if targetFPS <= 0 {
		targetFPS = 60
	}

	fpsRatio := lastFeedback.NetFPS / float64(targetFPS)
	congested := fpsRatio < 0.65 || lastFeedback.NetQueue > 6

	// ── 决策 ──
	if congested {
		if !adaptDegraded {
			// 首次拥塞：从用户上限开始降级
			adaptQ = userQ
			adaptFPS = userFPS
			adaptMW = userMW
			adaptDegraded = true
		}

		changed := false

		if adaptMode == "smooth" {
			// 流畅优先：先降画质，再降帧率，最后降分辨率
			if adaptQ > 40 {
				adaptQ = clampDown(adaptQ, 15, 30)
				changed = true
			} else if adaptFPS > 15 {
				adaptFPS = halveDown(adaptFPS, 10)
				changed = true
			} else if adaptMW > 1280 || adaptMW == 0 {
				adaptMW = stepDownRes(adaptMW, 1280)
				changed = true
			}
		} else {
			// 画质优先：先降帧率，再降画质，最后降分辨率
			if adaptFPS > 15 {
				adaptFPS = halveDown(adaptFPS, 10)
				changed = true
			} else if adaptQ > 40 {
				adaptQ = clampDown(adaptQ, 15, 30)
				changed = true
			} else if adaptMW > 1280 || adaptMW == 0 {
				adaptMW = stepDownRes(adaptMW, 1280)
				changed = true
			}
		}

		if changed {
			// 裁剪到用户上限以内
			adaptQ = min(adaptQ, userQ)
			adaptFPS = min(adaptFPS, userFPS)
			if userMW > 0 && (adaptMW == 0 || adaptMW > userMW) {
				adaptMW = userMW
			}
			adaptActive = true
			lastAdaptAt = now
		}
	} else if adaptDegraded {
		// ── 恢复 ──
		changed := false

		// 按降级逆序恢复：先恢复分辨率 → 帧率 → 画质
		if adaptMW > 0 && adaptMW < userMW {
			adaptMW = stepUpRes(adaptMW, userMW)
			changed = true
		} else if adaptMW == 0 {
			// 已是原始分辨率，跳过
		}

		if !changed && adaptFPS < userFPS {
			adaptFPS = min(adaptFPS+15, userFPS)
			changed = true
		}

		if !changed && adaptQ < userQ {
			adaptQ = min(adaptQ+10, userQ)
			changed = true
		}

		if !changed {
			// 全部恢复 → 退出自适应
			adaptQ = userQ
			adaptFPS = userFPS
			adaptMW = userMW
			adaptActive = false
			adaptDegraded = false
			changed = true // 需要一次重启恢复到用户设定
		}

		if changed {
			lastAdaptAt = now
		}
	}

	if adaptActive {
		return adaptQ, adaptFPS, adaptMW, true
	}
	return userQ, userFPS, userMW, false
}

// ── 辅助函数 ──

func clampDown(v, step, floor int) int {
	v -= step
	if v < floor {
		return floor
	}
	return v
}

func halveDown(v, floor int) int {
	v /= 2
	if v < floor {
		return floor
	}
	return v
}

func stepDownRes(v, floor int) int {
	if v == 0 {
		return 1920 // 从原始分辨率降到 1080p
	}
	v = v * 2 / 3
	if v < floor {
		return floor
	}
	return v
}

// getAdaptStatus 返回当前自适应状态，供 statsMsg 上报。
// 并发安全（仅读取，短锁）。
func getAdaptStatus() (active bool, q, fps int) {
	adaptMu.Lock()
	defer adaptMu.Unlock()
	return adaptActive, adaptQ, adaptFPS
}

func stepUpRes(v, ceiling int) int {
	if ceiling == 0 {
		return 0 // 回到原始
	}
	v = v * 3 / 2
	if v >= ceiling {
		return ceiling
	}
	return v
}
