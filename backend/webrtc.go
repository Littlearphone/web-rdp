package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// ── WebRTC 全局状态 ──
// 内网办公环境：不使用 STUN/TURN，仅 host 候选直连。
// 视频轨按档位键控（trackKey = 显示器:maxW:fps）：每个档位一条独立 Track，
// 避免同屏多档（不同分辨率/帧率）帧交错或分辨率突变花屏。
// 同一档位多名观众共享同一条 Track（可被多个 PeerConnection 绑定）。

var (
	tierTracks   = make(map[string]*webrtc.TrackLocalStaticSample) // trackKey → track
	tierTracksMu sync.Mutex
	webRTCAPI    *webrtc.API                 // pion API 实例
	rtcPeers     = make(map[string]*rtcPeer) // userName → peer
	rtcPeersMu   sync.Mutex
)

// rtcPeer 表示单个用户的 WebRTC 会话。
type rtcPeer struct {
	pc        *webrtc.PeerConnection
	userName  string
	display   int
	trackKey  string
	sendFn    func([]byte) // 通过 WebSocket 发送 JSON 消息给前端（ICE candidate 回调使用）
	connected atomic.Bool  // PeerConnection 是否已连通（视频真正走通）
}

// initWebRTC 初始化全局 WebRTC 基础设施。
// 注册 H.264 编解码器，失败时 tierTracks 保持空映射且静默降级。
func initWebRTC() {
	m := &webrtc.MediaEngine{}
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
	log.Printf("WebRTC: API 已就绪（内网直连，per-tier tracks）")
}

// getOrCreateTierTrack 获取或创建指定档位(trackKey)的视频轨。
func getOrCreateTierTrack(tk string) (*webrtc.TrackLocalStaticSample, error) {
	tierTracksMu.Lock()
	defer tierTracksMu.Unlock()
	if t, ok := tierTracks[tk]; ok {
		return t, nil
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "tier-"+tk,
	)
	if err != nil {
		return nil, fmt.Errorf("创建档位[%s]视频轨失败: %w", tk, err)
	}
	tierTracks[tk] = track
	log.Printf("WebRTC: 档位[%s]视频轨已创建", tk)
	return track, nil
}

// createRTCSession 为新连接用户创建 PeerConnection，仅添加其当前档位(trackKey)的视频轨。
// sendFn 用于 ICE candidate 回调时通过 WebSocket 推送给前端。
func createRTCSession(userName, tk string, display int, sendFn func([]byte)) (offerSDP string, err error) {
	if webRTCAPI == nil {
		return "", fmt.Errorf("WebRTC API 未就绪")
	}
	track, err := getOrCreateTierTrack(tk)
	if err != nil {
		return "", err
	}
	config := webrtc.Configuration{ICETransportPolicy: webrtc.ICETransportPolicyAll}
	pc, err := webRTCAPI.NewPeerConnection(config)
	if err != nil {
		return "", fmt.Errorf("创建 PeerConnection 失败: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return "", fmt.Errorf("添加档位[%s]视频轨失败: %w", tk, err)
	}
	peer := &rtcPeer{pc: pc, userName: userName, display: display, trackKey: tk, sendFn: sendFn}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		b, _ := json.Marshal(map[string]interface{}{"rtc_ice": ci})
		if b != nil {
			peer.sendFn(b)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("[WebRTC:%s] %s", userName, s)
		peer.connected.Store(s == webrtc.PeerConnectionStateConnected)
		if s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateFailed {
			removeRTCSession(userName)
		}
	})
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
	if old, ok := rtcPeers[userName]; ok {
		old.pc.Close()
	}
	rtcPeers[userName] = peer
	rtcPeersMu.Unlock()
	log.Printf("[WebRTC:%s] 会话已创建 → display[%d] tier[%s]", userName, display, tk)
	return offer.SDP, nil
}

// handleRTCAnswer 处理前端返回的 SDP Answer。
func handleRTCAnswer(userName, sdpStr string) error {
	rtcPeersMu.Lock()
	peer, ok := rtcPeers[userName]
	rtcPeersMu.Unlock()
	if !ok {
		return fmt.Errorf("peer %s 不存在", userName)
	}
	return peer.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdpStr,
	})
}

// handleRTCICE 处理前端发来的 ICE Candidate。
func handleRTCICE(userName, iceJSON string) error {
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

// userRTCVideoStatus 报告用户 WebRTC 视频是否已连通及所绑定显示器。
func userRTCVideoStatus(userName string) (active bool, display int) {
	rtcPeersMu.Lock()
	defer rtcPeersMu.Unlock()
	if p, ok := rtcPeers[userName]; ok && p.connected.Load() {
		return true, p.display
	}
	return false, -1
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

// restartRTC 显示器或档位变化时重建 WebRTC 会话。
// 关闭旧 PC 并通知前端 {rtc_restart:true}（前端随后重建并重发 rtc_webrtc 绑定新档位轨）。
// 若该用户尚无既有会话则不做任何事（首次由前端主动发起）。
func restartRTC(userName, newTk string, newDisplay int, sendFn func([]byte)) bool {
	rtcPeersMu.Lock()
	old, ok := rtcPeers[userName]
	if ok {
		old.pc.Close()
		delete(rtcPeers, userName)
	}
	rtcPeersMu.Unlock()
	if !ok {
		return false
	}
	log.Printf("[WebRTC:%s] 档位/显示器变化 → display[%d] tier[%s]，通知前端重建", userName, newDisplay, newTk)
	b, _ := json.Marshal(map[string]bool{"rtc_restart": true})
	if b != nil {
		sendFn(b)
	}
	return true
}

// writeTierWebRTCSample 向指定档位(trackKey)的视频轨写入 H.264 编码帧。
// duration 为相邻帧理论间隔，用于 RTP 时间戳插值。
// 非阻塞：Track 满或无订阅者时静默丢弃，避免反压阻塞档位产帧。
func writeTierWebRTCSample(tk string, data []byte, dur time.Duration) {
	tierTracksMu.Lock()
	track, ok := tierTracks[tk]
	tierTracksMu.Unlock()
	if !ok {
		return // 该档位无轨（无观众走 WebRTC）
	}
	if dur <= 0 {
		dur = time.Second / 30
	}
	_ = track.WriteSample(media.Sample{Data: data, Duration: dur})
}

// webRTCEnabled 返回 WebRTC 是否已就绪（API 初始化成功）。
func webRTCEnabled() bool {
	return webRTCAPI != nil
}
