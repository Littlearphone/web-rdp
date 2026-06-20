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
// 单全局视频轨：所有显示器帧写入同一 Track，不同分辨率由 H.264 SPS/PPS 动态切换。

var (
	webRTCTrack *webrtc.TrackLocalStaticSample // 全局 H.264 视频轨（所有显示器共享）
	webRTCAPI   *webrtc.API                    // pion API 实例
	rtcPeers    = make(map[string]*rtcPeer)    // userName → peer
	rtcPeersMu  sync.Mutex
)

// rtcPeer 表示单个用户的 WebRTC 会话
type rtcPeer struct {
	pc       *webrtc.PeerConnection
	userName string
	sendFn   func([]byte) // 通过 WebSocket 发送 JSON 消息给前端（ICE candidate 回调使用）
}

// initWebRTC 初始化全局 WebRTC 基础设施。
// 注册 H.264 编解码器 + 创建全局视频轨，失败时 webRTCTrack 保持 nil 且静默降级。
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

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "web-rdp",
	)
	if err != nil {
		log.Printf("WebRTC: 创建视频轨失败: %v", err)
		return
	}
	webRTCTrack = track
	log.Printf("WebRTC: 视频轨已就绪（内网直连模式）")
}

// createRTCSession 为新连接用户创建 PeerConnection，生成 SDP Offer 返回给调用方。
// sendFn 用于 ICE candidate 回调时通过 WebSocket 推送给前端。
// 返回的 offer SDP 字符串由调用方通过 WebSocket 发送给前端。
func createRTCSession(userName string, sendFn func([]byte)) (offerSDP string, err error) {
	if webRTCAPI == nil || webRTCTrack == nil {
		return "", fmt.Errorf("WebRTC 未就绪")
	}

	// 内网环境：无 STUN/TURN，仅 host 候选
	config := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
	}

	pc, err := webRTCAPI.NewPeerConnection(config)
	if err != nil {
		return "", fmt.Errorf("创建 PeerConnection 失败: %w", err)
	}

	// 添加全局视频轨
	if _, err := pc.AddTrack(webRTCTrack); err != nil {
		pc.Close()
		return "", fmt.Errorf("添加视频轨失败: %w", err)
	}

	peer := &rtcPeer{pc: pc, userName: userName, sendFn: sendFn}

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

	log.Printf("[WebRTC:%s] 会话已创建", userName)
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

// writeWebRTCSample 向全局视频轨写入 H.264 编码帧。
// 非阻塞：Track 满或无订阅者时静默丢弃，避免反压阻塞 ffmpeg 管道。
// 由 ffmpeg_pipeline.go 的 fan-out goroutine 调用。
func writeWebRTCSample(data []byte) {
	if webRTCTrack == nil {
		return
	}
	sample := media.Sample{
		Data:     data,
		Duration: time.Second / 30, // 假设 30fps，仅用于 RTP 时间戳插值
	}
	// 静默忽略写入失败（无 PeerConnection 订阅时 track 返回错误）
	_ = webRTCTrack.WriteSample(sample)
}

// webRTCEnabled 返回 WebRTC 是否已就绪（视频轨创建成功）。
func webRTCEnabled() bool {
	return webRTCTrack != nil
}
