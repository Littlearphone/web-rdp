/**
 * WebRTC 视频传输管理
 *
 * 内网直连模式（无 STUN/TURN），通过 RTCPeerConnection 接收 H.264 视频轨。
 * 使用隐藏 <video> 元素接收解码，rAF 绘制到 canvas，与现有渲染管线并行。
 *
 * 信令通过 WebSocket 交换（SDP Offer/Answer + ICE Candidates）。
 *
 * 用法:
 *   const rtc = startWebRTC(canvas, sendJson, onFirstFrame);
 *   // WebSocket onmessage 中: rtc.onSignal(msg);
 *   rtc.close(); // 组件卸载时
 */

let webrtcSession: {
  pc: RTCPeerConnection;
  video: HTMLVideoElement;
  canvas: HTMLCanvasElement;
  ctx: CanvasRenderingContext2D;
  onFirstFrame: (() => void) | null;
} | null = null;

export interface WebRTCControl {
  /** 处理来自 WebSocket 的信令消息（rtc_sdp / rtc_ice） */
  onSignal(msg: Record<string, unknown>): void;
  /** 关闭 WebRTC 连接并释放资源 */
  close(): void;
  /** 是否已连接且视频轨活跃 */
  readonly connected: boolean;
}

/**
 * 启动 WebRTC 接收端。
 * 立即创建 RTCPeerConnection 并发送 {rtc_webrtc: true} 告知后端。
 * 后端创建 Offer 后通过 WebSocket 回传 SDP。
 *
 * @param canvas    渲染目标 canvas
 * @param sendJson  通过 WebSocket 发送 JSON 消息
 * @param onFirstFrame 首帧渲染后回调
 */
export function startWebRTC(
  canvas: HTMLCanvasElement,
  sendJson: (msg: Record<string, unknown>) => void,
  onFirstFrame?: () => void,
): WebRTCControl {
  if (webrtcSession) {
    webrtcSession.pc.close();
    webrtcSession = null;
  }

  const ctx = canvas.getContext('2d')!;

  // 隐藏的 video 元素用于接收 WebRTC 视频轨
  const video = document.createElement('video');
  video.muted = true;
  video.playsInline = true;
  video.style.display = 'none';
  document.body.appendChild(video);

  // 创建 PeerConnection（内网：不配 ICE 服务器）
  const pc = new RTCPeerConnection({
    iceTransportPolicy: 'all',
  });

  let firstFrameFired = false;
  let rafId = 0;

  // 接收远程视频轨 → 设置 video.srcObject
  pc.ontrack = (event: RTCTrackEvent) => {
    console.log('[WebRTC] 收到远程视频轨:', event.track.kind);
    const stream = event.streams[0];
    if (!stream) return;
    video.srcObject = stream;
    video.play().catch(() => { /* autoplay policy */ });

    if (!firstFrameFired) {
      // 启动 rAF 渲染循环
      const render = () => {
        rafId = requestAnimationFrame(render);
        if (video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;

        const vw = video.videoWidth;
        const vh = video.videoHeight;
        if (vw === 0 || vh === 0) return;

        // 分辨率变化时调整 canvas
        if (canvas.width !== vw || canvas.height !== vh) {
          canvas.width = vw;
          canvas.height = vh;
        }

        ctx.drawImage(video, 0, 0);

        if (!firstFrameFired) {
          firstFrameFired = true;
          onFirstFrame?.();
        }
      };
      rafId = requestAnimationFrame(render);
    }
  };

  // ICE candidate → 通过 WebSocket 发送给后端
  pc.onicecandidate = (event: RTCPeerConnectionIceEvent) => {
    if (event.candidate) {
      sendJson({ rtc_ice: JSON.stringify(event.candidate.toJSON()) });
    }
  };

  // 连接状态监控
  pc.onconnectionstatechange = () => {
    console.log('[WebRTC] 连接状态:', pc.connectionState);
    if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') {
      // 连接断开时清理（但不清除 canvas 上的最后一帧）
      cleanup();
    }
  };

  const session = {
    pc,
    video,
    canvas,
    ctx,
    onFirstFrame: onFirstFrame ?? null,
  };
  webrtcSession = session;

  const control: WebRTCControl = {
    onSignal(msg: Record<string, unknown>) {
      // 处理 SDP Offer（后端创建）
      if (typeof msg.rtc_sdp === 'string') {
        console.log('[WebRTC] 收到 SDP Offer，开始协商...');
        const sdp = msg.rtc_sdp;
        pc.setRemoteDescription({ type: 'offer', sdp })
          .then(() => pc.createAnswer())
          .then((answer) => pc.setLocalDescription(answer))
          .then(() => {
            console.log('[WebRTC] Answer 已生成，发送到后端');
            sendJson({ rtc_sdp: pc.localDescription!.sdp });
          })
          .catch((err) => {
            console.error('[WebRTC] SDP 协商失败:', err);
          });
        return;
      }

      // 处理 ICE Candidate（后端发来）
      if (msg.rtc_ice && typeof msg.rtc_ice === 'object') {
        pc.addIceCandidate(new RTCIceCandidate(msg.rtc_ice as RTCIceCandidateInit))
          .catch((err) => {
            console.error('[WebRTC] ICE candidate 添加失败:', err);
          });
      }
    },

    get connected(): boolean {
      return pc.connectionState === 'connected';
    },

    close() {
      cleanup();
    },
  };

  function cleanup() {
    if (rafId) {
      cancelAnimationFrame(rafId);
      rafId = 0;
    }
    // 停止 video 播放但不立即移除 DOM（避免 canvas 闪白）
    video.pause();
    video.srcObject = null;
    setTimeout(() => {
      if (video.parentNode) {
        video.parentNode.removeChild(video);
      }
    }, 100);
    pc.close();
    if (webrtcSession === session) {
      webrtcSession = null;
    }
  }

  return control;
}

/** 获取当前 WebRTC 会话的连接状态（供外部判断是否跳过 WS 帧） */
export function isWebRTCConnected(): boolean {
  return webrtcSession !== null &&
    webrtcSession.pc.connectionState === 'connected';
}
