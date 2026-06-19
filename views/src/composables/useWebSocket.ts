/**
 * WebSocket 连接管理
 *
 * 负责生命周期管理：连接、认证握手、消息路由、自动重连（指数退避）。
 * 文本消息 → 更新 Pinia store
 * 二进制消息 → 委托给注册的 binaryHandler
 */

import { useAppStore } from '@/stores/app';
import type { InitMsg, StatsMsg, ControlStatusMsg, StreamFormat } from '@/types';

type BinaryHandler = (data: ArrayBuffer, format: 'h264' | 'jpeg') => void;

let binaryHandler: BinaryHandler | null = null;

/** 注册二进制帧处理器（ScreenCanvas 调用） */
export function registerBinaryHandler(fn: BinaryHandler) {
  binaryHandler = fn;
}

/** SHA-256 摘要（用于认证 challenge-response） */
async function sha256Hex(s: string): Promise<string> {
  const buf = new TextEncoder().encode(s);
  const hash = await crypto.subtle.digest('SHA-256', buf);
  return Array.from(new Uint8Array(hash))
    .map(b => b.toString(16).padStart(2, '0'))
    .join('');
}

export function useWebSocket() {
  const store = useAppStore();

  // ═══════════════════════════════════════════
  // 剪贴板
  // ═══════════════════════════════════════════

  /** 从远端同步剪贴板文本到浏览器 */
  async function applyRemoteClipboard(text: string) {
    if (text === store.remoteClipboard) return;
    store.remoteClipboard = text;
    try {
      await navigator.clipboard.writeText(text);
    } catch (_) {
      // 非 HTTPS 或权限不足时静默失败
    }
  }

  // ═══════════════════════════════════════════
  // WebSocket 消息处理
  // ═══════════════════════════════════════════

  function onMessage(event: MessageEvent) {
    if (typeof event.data === 'string') {
      try {
        const s = JSON.parse(event.data) as InitMsg & StatsMsg & ControlStatusMsg;

        // 剪贴板推送（后端 → 前端）
        if (s.clipboard) {
          applyRemoteClipboard(s.clipboard);
          return;
        }

        // 控制权限状态
        if (s.control_status) {
          store.controlStatus = s.control_status;
          store.controlMsg = s.control_msg || '';
          if (s.control_status === 'granted' || s.control_status === 'denied' || s.control_status === 'busy') {
            setTimeout(() => {
              if (store.controlStatus === s.control_status) {
                store.controlStatus = 'idle';
                store.controlMsg = '';
              }
            }, 2000);
          }
          if (s.control_status === 'pending') {
            setTimeout(() => {
              if (store.controlStatus === 'pending') {
                store.controlStatus = 'idle';
                store.controlMsg = '请求超时，请重试';
              }
            }, 60000);
          }
          return;
        }

        // 用户名（首次连接）
        if (s.user) {
          store.statsUser = s.user;
        }

        // 编码格式通知（初始连接或切换编码器）
        if (s.format) {
          const wanted = s.format as StreamFormat;
          if (wanted === 'h264' && !store.canH264) return;
          if (store.streamFormat !== wanted) {
            store.streamFormat = wanted;
            store.connectionStatus = 'switching';
          }
          if (s.quality !== undefined) store.currentQ = s.quality;
          if (s.maxw !== undefined) store.currentMW = s.maxw;
          if (s.fps !== undefined) store.currentFPS = s.fps;
          return;
        }

        // 性能统计（每秒推送）
        store.statsFps = s.fps || 0;
        store.statsEncMs = s.enc_ms || 0;
        store.statsKb = s.kb || 0;
        store.statsW = s.w || 0;
        store.statsH = s.h || 0;
        store.statsQ = s.q || 0;
        store.statsMaxRate = s.maxrate || 0;
        if (s.users !== undefined) store.statsUsers = s.users;

        if (s.owner !== undefined) {
          store.statsOwner = s.owner;
        }

        if (s.ox !== undefined) {
          store.meta = { ox: s.ox, oy: s.oy, pw: s.w, ph: s.h, zoom: s.zoom };
        }

        if (s.screens > 0 && s.screens !== store.screenCount) {
          store.screenCount = s.screens;
          store.mobileUIBuilt = false;
        }
      } catch (_) { /* JSON 解析失败 */ }
      return;
    }

    // 二进制帧 → 委托给 ScreenCanvas 注册的 handler
    if (binaryHandler) {
      binaryHandler(event.data as ArrayBuffer, store.streamFormat);
    }
  }

  // ═══════════════════════════════════════════
  // 连接 / 重连
  // ═══════════════════════════════════════════

  let savedUser = '';
  let savedPassword = ''; // 用户在弹窗中输入的密码（会话内记忆，用于重连）

  function connect(user?: string, password?: string) {
    if (user) savedUser = user;
    if (password !== undefined) savedPassword = password;
    // 关闭旧连接
    if (store.ws) {
      store.ws.onclose = null;
      store.ws.close();
      store.ws = null;
    }

    store.clearReconnectTimer();
    store.showReconnectHint = false;
    store.lastResKey = '';
    store.connectionStatus = 'connecting';
    store.streamFormat = (store.useH264 && store.canH264) ? 'h264' : 'jpeg';

    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
    let wsUrl = `${proto}://${store.serverAddr}/ws`;
    if (savedUser) wsUrl += `?user=${encodeURIComponent(savedUser)}`;
    const wsInst = new WebSocket(wsUrl);
    wsInst.binaryType = 'arraybuffer';

    // ── 认证握手状态机 ──
    let authDone = false;

    wsInst.onopen = () => {
      store.wasConnected = true;
      store.connectionStatus = 'connected';
      store.reconnectDelay = 5;

      // 首条消息一定是 {challenge} 或 {user, format}
      // 我们把消息处理器临时包装一层来拦截首条消息
      const origHandler = wsInst.onmessage;
      wsInst.onmessage = async (ev: MessageEvent) => {
        if (authDone) {
          origHandler?.call(wsInst, ev);
          return;
        }

        if (typeof ev.data === 'string') {
          try {
            const init = JSON.parse(ev.data);
            // 收到 challenge → 需要认证
            if (init.challenge) {
              const challenge = init.challenge;
              let authToken: string;
              if (savedPassword) {
                authToken = await sha256Hex(challenge + savedPassword);
              } else {
                authToken = 'anonymous';
              }
              store.send({ auth: authToken });
              authDone = true;
              // 恢复原始处理器，后续消息（包含 user/format）正常处理
              wsInst.onmessage = origHandler;
              return;
            }
          } catch (_) {}
        }

        // 无 challenge → 无需认证，直接进入正常流程
        authDone = true;
        wsInst.onmessage = origHandler;
        origHandler?.call(wsInst, ev);
      };
    };

    wsInst.onmessage = onMessage;

    wsInst.onclose = (ev: CloseEvent) => {
      store.connectionStatus = 'disconnected';
      // 4001 = 同 IP 新连接顶替，不重连
      // 不在此处设置 wasConnected = false，由 App.vue 的 watch 检测并弹出登录框
      if (ev.code === 4001) {
        store.clearReconnectTimer();
        store.showReconnectHint = false;
        return;
      }
      if (store.wasConnected) {
        startReconnect();
      }
    };

    wsInst.onerror = () => {
      if (!store.wasConnected) {
        store.connectionStatus = 'failed';
      }
    };

    store.ws = wsInst;
  }

  function startReconnect() {
    scheduleReconnect();
  }

  function scheduleReconnect() {
    store.clearReconnectTimer();
    store.reconnectCountdown = store.reconnectDelay;
    store.showReconnectHint = true;

    store.reconnectTimer = setInterval(() => {
      store.reconnectCountdown--;
      if (store.reconnectCountdown <= 0) {
        store.clearReconnectTimer();
        store.showReconnectHint = false;
        store.reconnectDelay = Math.min(store.reconnectDelay * 2, 30);
        connect();
        return;
      }
    }, 1000);
  }

  function manualReconnect() {
    store.clearReconnectTimer();
    store.showReconnectHint = false;
    connect();
  }

  // ═══════════════════════════════════════════
  // 初始化
  // ═══════════════════════════════════════════

  function init(user: string, password?: string) {
    store.isMobile =
      /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) &&
      window.innerWidth <= 900;
    connect(user, password);
  }

  return { connect, manualReconnect, init };
}
