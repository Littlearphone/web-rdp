package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// ── WebRTC 全局状态 ──
// 内网办公环境：不使用 STUN/TURN，仅 host 候选直连。
// 每显示器独立视频轨：多用户查看不同显示器时帧不会交错，
// 各轨独立 SPS/PPS 序列，H.264 解码器不会因分辨率突变花屏。

var (
	webRTCTracks   = make(map[int]*webrtc.TrackLocalStaticSample) // displayID → track
	webRTCTracksMu sync.Mutex
	webRTCAPI      *webrtc.API                 // pion API 实例
	rtcPeers       = make(map[string]*rtcPeer) // userName → peer
	rtcPeersMu     sync.Mutex
)

// rtcPeer 表示单个用户的 WebRTC 会话
type rtcPeer struct {
	pc       *webrtc.PeerConnection
	userName string
	display  int          // 当前订阅的显示器
	sendFn   func([]byte) // 通过 WebSocket 发送 JSON 消息给前端（ICE candidate 回调使用）
}

// initWebRTC 初始化全局 WebRTC 基础设施。
// 注册 H.264 编解码器，失败时 webRTCTracks 保持空映射且静默降级。
func initWebRTC() {
	m := &webrtc.MediaEngine{}

	// 注册 H.264 编解码器（payload type 96, clock rate 90000Hz）
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		log.Printf("WebRTC: 注册 H.264 编解码器失败: %v", err)
		return
	}

	webRTCAPI = webrtc.NewAPI(webrtc.WithMediaEngine(m))
	log.Printf("WebRTC: API 已就绪（内网直连模式，per-display tracks）")
}

// getOrCreateTrack 获取或创建指定显示器的视频轨。
func getOrCreateTrack(displayID int) (*webrtc.TrackLocalStaticSample, error) {
	webRTCTracksMu.Lock()
	defer webRTCTracksMu.Unlock()

	if t, ok := webRTCTracks[displayID]; ok {
		return t, nil
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", fmt.Sprintf("display-%d", displayID),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 display[%d] 视频轨失败: %w", displayID, err)
	}
	webRTCTracks[displayID] = track
	log.Printf("WebRTC: display[%d] 视频轨已创建", displayID)
	return track, nil
}

// createRTCSession 为新连接用户创建 PeerConnection，仅添加指定显示器的视频轨。
// sendFn 用于 ICE candidate 回调时通过 WebSocket 推送给前端。
// 返回的 offer SDP 字符串由调用方通过 WebSocket 发送给前端。
func createRTCSession(userName string, displayID int, sendFn func([]byte)) (offerSDP string, err error) {
	if webRTCAPI == nil {
		return "", fmt.Errorf("WebRTC API 未就绪")
	}

	track, err := getOrCreateTrack(displayID)
	if err != nil {
		return "", err
	}

	// 内网环境：无 STUN/TURN，仅 host 候选
	config := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
	}

	pc, err := webRTCAPI.NewPeerConnection(config)
	if err != nil {
		return "", fmt.Errorf("创建 PeerConnection 失败: %w", err)
	}

	// 添加指定显示器的视频轨（而非全局轨）
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return "", fmt.Errorf("添加 display[%d] 视频轨失败: %w", displayID, err)
	}

	peer := &rtcPeer{pc: pc, userName: userName, display: displayID, sendFn: sendFn}

	// ICE candidate 回调 → 通过 sendFn 推送 JSON 给前端
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // ICE 收集完成
		}
		ci := c.ToJSON()
		b, err := json.Marshal(map[string]interface{}{
			"rtc_ice": ci,
		})
		if err != nil {
			return
		}
		peer.sendFn(b)
	})

	// 连接状态变更日志
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("[WebRTC:%s] %s", userName, s)
		if s == webrtc.PeerConnectionStateClosed ||
			s == webrtc.PeerConnectionStateFailed {
			removeRTCSession(userName)
		}
	})

	// 创建 SDP Offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return "", fmt.Errorf("创建 Offer 失败: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return "", fmt.Errorf("设置本地描述失败: %w", err)
	}

	rtcPeersMu.Lock()
	// 关闭旧会话（如重连场景）
	if old, ok := rtcPeers[userName]; ok {
		old.pc.Close()
	}
	rtcPeers[userName] = peer
	rtcPeersMu.Unlock()

	log.Printf("[WebRTC:%s] 会话已创建 → display[%d]", userName, displayID)
	return offer.SDP, nil
}

// handleRTCAnswer 处理前端返回的 SDP Answer。
func handleRTCAnswer(userName string, sdpStr string) error {
	rtcPeersMu.Lock()
	peer, ok := rtcPeers[userName]
	rtcPeersMu.Unlock()
	if !ok {
		return fmt.Errorf("peer %s 不存在", userName)
	}
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdpStr,
	}
	return peer.pc.SetRemoteDescription(answer)
}

// handleRTCICE 处理前端发来的 ICE Candidate。
func handleRTCICE(userName string, iceJSON string) error {
	rtcPeersMu.Lock()
	peer, ok := rtcPeers[userName]
	rtcPeersMu.Unlock()
	if !ok {
		return fmt.Errorf("peer %s 不存在", userName)
	}
	var ice webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(iceJSON), &ice); err != nil {
		return fmt.Errorf("ICE candidate 解析失败: %w", err)
	}
	return peer.pc.AddICECandidate(ice)
}

// removeRTCSession 关闭并移除用户的 WebRTC 会话。
func removeRTCSession(userName string) {
	rtcPeersMu.Lock()
	defer rtcPeersMu.Unlock()
	if peer, ok := rtcPeers[userName]; ok {
		peer.pc.Close()
		delete(rtcPeers, userName)
		log.Printf("[WebRTC:%s] 会话已移除", userName)
	}
}

// restartRTCForDisplay 在用户切换显示器时重建 WebRTC 会话。
// 关闭旧 PC，为新显示器创建 PC，发送 {rtc_restart: true} 通知前端重建。
// 前端收到后关闭本地 PC 并重新发送 {rtc_webrtc: true} 触发完整重建。
func restartRTCForDisplay(userName string, newDisplay int, sendFn func([]byte)) {
	rtcPeersMu.Lock()
	old, ok := rtcPeers[userName]
	if ok {
		old.pc.Close()
		delete(rtcPeers, userName)
	}
	rtcPeersMu.Unlock()

	if !ok {
		return // 没有旧会话，无需重启
	}

	log.Printf("[WebRTC:%s] 显示器切换 → display[%d]，通知前端重建 WebRTC", userName, newDisplay)
	// 通知前端：WebRTC 需要重建（display track 已变更）
	b, _ := json.Marshal(map[string]bool{"rtc_restart": true})
	if b != nil {
		sendFn(b)
	}
}

// writeWebRTCSample 向指定显示器的视频轨写入 H.264 编码帧。
// duration 应为相邻帧的理论间隔（如 60fps → ~16.7ms），用于 RTP 时间戳插值。
// 非阻塞：Track 满或无订阅者时静默丢弃，避免反压阻塞 ffmpeg 管道。
// 由 ffmpeg_pipeline.go 的 fan-out goroutine 调用。
func writeWebRTCSample(displayID int, data []byte, dur time.Duration) {
	webRTCTracksMu.Lock()
	track, ok := webRTCTracks[displayID]
	webRTCTracksMu.Unlock()
	if !ok {
		return // 该显示器无 track（无用户订阅）
	}
	if dur <= 0 {
		dur = time.Second / 30 // 兜底
	}
	sample := media.Sample{
		Data:     data,
		Duration: dur,
	}
	// 静默忽略写入失败（无 PeerConnection 订阅时 track 返回错误）
	_ = track.WriteSample(sample)
}

// webRTCEnabled 返回 WebRTC 是否已就绪（API 初始化成功）。
func webRTCEnabled() bool {
	return webRTCAPI != nil
}
