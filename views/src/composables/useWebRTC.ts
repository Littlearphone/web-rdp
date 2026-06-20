/**
 * WebRTC 视频传输管理
 *
 * 内网直连模式（无 STUN/TURN），通过 RTCPeerConnection 接收 H.264 视频轨。
 * 使用隐藏 <video> 元素接收解码，绘制到 canvas，与现有渲染管线并行。
 *
 * 信令通过 WebSocket 交换（SDP Offer/Answer + ICE Candidates）。
 *
 * 高帧率防抖策略（180fps 源 → 60Hz 显示器）：
 *   1. playoutDelayHint=50ms 限制 jitter buffer，缓冲满时自动丢旧帧防堆积
 *   2. VFC (VideoFrameCallback) 精确感知新帧到达 → 置脏标记
 *   3. rAF 以显示器刷新率绘制，多帧间只取最后一帧（自动跳帧）
 *   4. 退路：无 VFC 支持的浏览器回退到纯 rAF + currentTime 变化检测
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

/** WebRTC 是否已渲染过至少一帧（用于 WS 帧交接门控） */
let webrtcActive = false;

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

  // ── rAF 渲染调度状态 ──
  let rafId = 0;
  let vfcId = 0;
  let hasNewFrame = false; // VFC 置 true，rAF 消费后置 false
  let lastDrawnTime = 0;   // video.currentTime 上次绘制值（VFC 不可用时用于去重）

  // ── 诊断：记录渲染耗时（确认瓶颈后删除）──
  let diagDrawSamples = 0;
  let diagDrawTotal = 0;
  let diagResizeTotal = 0;
  let vfcTimestamp = 0; // VFC 触发时间，用于计算 VFC→rAF 延迟

  // 检测 requestVideoFrameCallback 支持
  const hasVFC = typeof (video as HTMLVideoElement & {
    requestVideoFrameCallback?: (cb: () => void) => number;
  }).requestVideoFrameCallback === 'function';

  // 接收远程视频轨 → 设置 video.srcObject
  pc.ontrack = (event: RTCTrackEvent) => {
    console.log('[WebRTC] 收到远程视频轨:', event.track.kind);

    // ── 限制抖动缓冲区：远程桌面场景低延迟优先 ──
    // 不设置时浏览器默认 200-500ms，180fps 持续输入下缓冲累积可达数秒延迟。
    // 设 50ms（~3 帧 @60fps），缓冲满时 WebRTC 栈自动丢旧包而非无限堆积。
    if (event.receiver &&
        (event.receiver as RTCRtpReceiver & { playoutDelayHint?: number }).playoutDelayHint !== undefined) {
      (event.receiver as RTCRtpReceiver & { playoutDelayHint: number }).playoutDelayHint = 0.05;
      console.log('[WebRTC] jitter buffer 上限: 50ms');
    }

    const stream = event.streams[0];
    if (!stream) return;

    // 避免重复设置同一个 stream（ontrack 可能因 renegotiation 重复触发）
    if (video.srcObject === stream) return;
    video.srcObject = stream;
    video.play().catch(() => { /* autoplay policy */ });

    if (!firstFrameFired) {
      startRenderLoop();
    }
  };

  /** 启动渲染循环，根据浏览器能力选择最优策略 */
  function startRenderLoop() {
    lastDrawnTime = 0;
    hasNewFrame = false;

    if (hasVFC) {
      // ── 策略 A：VFC + rAF 双轨制 ──
      // VFC 精确捕获解码器输出新帧（可 > 显示器刷新率），仅置脏标记。
      // rAF 按显示器刷新率消费脏标记，多次 VFC 间只绘制最后一帧。
      const onVideoFrame = () => {
        hasNewFrame = true;
        vfcTimestamp = performance.now();
        // 重新注册必须在本次回调内，确保下一帧也不漏
        vfcId = (video as HTMLVideoElement & {
          requestVideoFrameCallback: (cb: () => void) => number;
        }).requestVideoFrameCallback(onVideoFrame);
      };
      vfcId = (video as HTMLVideoElement & {
        requestVideoFrameCallback: (cb: () => void) => number;
      }).requestVideoFrameCallback(onVideoFrame);

      const onRaf = () => {
        rafId = requestAnimationFrame(onRaf);
        if (!hasNewFrame) return;
        hasNewFrame = false;

        if (video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;

        const vw = video.videoWidth;
        const vh = video.videoHeight;
        if (vw === 0 || vh === 0) return;

        const t0 = performance.now();
        const vfcLag = vfcTimestamp > 0 ? t0 - vfcTimestamp : 0;

        if (canvas.width !== vw || canvas.height !== vh) {
          const t1 = performance.now();
          canvas.width = vw;
          canvas.height = vh;
          diagResizeTotal += performance.now() - t1;
        }

        ctx.drawImage(video, 0, 0);
        const dt = performance.now() - t0;
        diagDrawTotal += dt;
        diagDrawSamples++;

        if (dt > 10) console.warn('[WebRTC] 慢绘制:', dt.toFixed(1), 'ms canvas:', vw, 'x', vh, 'VFC→rAF:', vfcLag.toFixed(1), 'ms');
        if (diagDrawSamples % 120 === 0) {
          console.log('[WebRTC perf] draw:', diagDrawTotal.toFixed(1), 'ms resize:', diagResizeTotal.toFixed(1), 'ms (累计 120 帧)');
          diagDrawTotal = 0; diagResizeTotal = 0;
        }

        if (!firstFrameFired) {
          firstFrameFired = true;
          webrtcActive = true;
          onFirstFrame?.();
        }
      };
      rafId = requestAnimationFrame(onRaf);

    } else {
      // ── 策略 B：纯 rAF + currentTime 去重 ──
      // requestVideoFrameCallback 不可用时的回退方案
      const onRaf = () => {
        rafId = requestAnimationFrame(onRaf);

        if (video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;

        // currentTime 未变 → 解码器无新帧输出，跳过绘制
        if (video.currentTime === lastDrawnTime) return;
        lastDrawnTime = video.currentTime;

        const vw = video.videoWidth;
        const vh = video.videoHeight;
        if (vw === 0 || vh === 0) return;

        const t0 = performance.now();

        if (canvas.width !== vw || canvas.height !== vh) {
          const t1 = performance.now();
          canvas.width = vw;
          canvas.height = vh;
          diagResizeTotal += performance.now() - t1;
        }

        ctx.drawImage(video, 0, 0);
        const dt = performance.now() - t0;
        diagDrawTotal += dt;
        diagDrawSamples++;

        if (dt > 10) console.warn('[WebRTC] 慢绘制:', dt.toFixed(1), 'ms canvas:', vw, 'x', vh);
        if (diagDrawSamples % 120 === 0) {
          console.log('[WebRTC perf] draw:', diagDrawTotal.toFixed(1), 'ms resize:', diagResizeTotal.toFixed(1), 'ms (累计 120 帧)');
          diagDrawTotal = 0; diagResizeTotal = 0;
        }

        if (!firstFrameFired) {
          firstFrameFired = true;
          webrtcActive = true;
          onFirstFrame?.();
        }
      };
      rafId = requestAnimationFrame(onRaf);
    }
  }

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
      cleanup();
    }
  };

  const session = { pc, video, canvas, ctx, onFirstFrame: onFirstFrame ?? null };
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
    webrtcActive = false;
    if (rafId) {
      cancelAnimationFrame(rafId);
      rafId = 0;
    }
    if (vfcId && hasVFC) {
      (video as HTMLVideoElement & { cancelVideoFrameCallback?: (id: number) => void })
        .cancelVideoFrameCallback?.(vfcId);
      vfcId = 0;
    }
    video.pause();
    video.srcObject = null;
    setTimeout(() => {
      if (video.parentNode) video.parentNode.removeChild(video);
    }, 100);
    pc.close();
    if (webrtcSession === session) webrtcSession = null;
  }

  return control;
}

/** 获取当前 WebRTC 会话的连接状态（供外部判断是否跳过 WS 帧） */
export function isWebRTCConnected(): boolean {
  return webrtcSession !== null &&
    webrtcSession.pc.connectionState === 'connected';
}

/** WebRTC 是否已渲染过至少一帧（WS 帧交接的真正时机） */
export function isWebRTCActive(): boolean {
  return webrtcActive;
}

export interface WebRTCReceiveStats {
  fps: number;       // 接收帧率（framesPerSecond 或推算值）
  jitterMs: number;  // 抖动缓冲延迟（ms）
  packetsLost: number; // 累计丢包数
}

let lastStatsTimestamp = 0;
let lastStatsFramesReceived = 0;

/** 获取 WebRTC 接收统计（供自适应码率上报使用） */
export async function pollWebRTCStats(): Promise<WebRTCReceiveStats | null> {
  const s = webrtcSession;
  if (!s || s.pc.connectionState !== 'connected') return null;

  try {
    const report = await s.pc.getStats(null);
    let framesReceived = 0;
    let framesPerSecond = 0;
    let jitterBufferDelay = 0;
    let jitterBufferDelayCount = 0;
    let packetsLost = 0;

    report.forEach((stat) => {
      if (stat.type === 'inbound-rtp' && stat.kind === 'video') {
        framesReceived = stat.framesReceived ?? 0;
        framesPerSecond = stat.framesPerSecond ?? 0;
        packetsLost = stat.packetsLost ?? 0;
        const jbd = stat.jitterBufferDelay;
        const jbe = stat.jitterBufferEmittedCount;
        if (typeof jbd === 'number' && typeof jbe === 'number' && jbe > 0) {
          jitterBufferDelay = jbd;
          jitterBufferDelayCount = jbe;
        }
      }
    });

    // framesPerSecond 在某些实现中不可用，用帧计数差值推算
    let fps = framesPerSecond;
    if (fps <= 0 && framesReceived > 0) {
      const now = performance.now();
      if (lastStatsTimestamp > 0 && framesReceived > lastStatsFramesReceived) {
        const dt = (now - lastStatsTimestamp) / 1000;
        fps = dt > 0.2 ? (framesReceived - lastStatsFramesReceived) / dt : 0;
      }
      lastStatsTimestamp = now;
      lastStatsFramesReceived = framesReceived;
    }

    // 平均抖动缓冲延迟
    const jitterMs = jitterBufferDelayCount > 0
      ? (jitterBufferDelay / jitterBufferDelayCount) * 1000
      : 0;

    return { fps: Math.round(fps * 10) / 10, jitterMs: Math.round(jitterMs), packetsLost };
  } catch {
    return null;
  }
}
