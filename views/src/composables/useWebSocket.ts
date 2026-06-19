/**
 * WebSocket 连接管理
 *
 * 负责生命周期管理：连接、消息路由、自动重连（指数退避）。
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

export function useWebSocket() {
  const store = useAppStore();

  // ═══════════════════════════════════════════
  // WebSocket 消息处理
  // ═══════════════════════════════════════════

  function onMessage(event: MessageEvent) {
    if (typeof event.data === 'string') {
      try {
        const s = JSON.parse(event.data) as InitMsg & StatsMsg & ControlStatusMsg;

        // 控制权限状态
        if (s.control_status) {
          store.controlStatus = s.control_status;
          store.controlMsg = s.control_msg || '';
          // granted/denied/busy 2 秒后重置为空闲（让提示消息有一定展示时间）
          if (s.control_status === 'granted' || s.control_status === 'denied' || s.control_status === 'busy') {
            setTimeout(() => {
              if (store.controlStatus === s.control_status) {
                store.controlStatus = 'idle';
                store.controlMsg = '';
              }
            }, 2000);
          }
          return; // 控制状态消息不含其他数据
        }

        // 用户名（首次连接）
        if (s.user) {
          store.statsUser = s.user;
        }

        // 编码格式通知（初始连接或切换编码器）
        // 如果浏览器不支持 WebCodecs，忽略后端的 H.264 通知，坚持用 JPEG
        if (s.format) {
          const wanted = s.format as StreamFormat;
          if (wanted === 'h264' && !store.canH264) return;
          if (store.streamFormat !== wanted) {
            store.streamFormat = wanted;
            store.connectionStatus = 'switching';
          }
          return; // 格式消息不含 stats
        }

        // 性能统计（每秒推送）
        store.statsFps = s.fps || 0;
        store.statsEncMs = s.enc_ms || 0;
        store.statsKb = s.kb || 0;
        store.statsW = s.w || 0;
        store.statsH = s.h || 0;
        store.statsQ = s.q || 0;
        store.statsMaxRate = s.maxrate || 0;

        // 控制权
        if (s.owner !== undefined) {
          store.statsOwner = s.owner;
        }

        // 屏幕元数据（JPEG 路径通过帧头获取，这里做 fallback）
        if (s.ox !== undefined) {
          store.meta = { ox: s.ox, oy: s.oy, pw: s.w, ph: s.h, zoom: s.zoom };
        }

        // 屏幕数量变化
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

  function connect() {
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

    const wsUrl = `ws://${store.serverAddr}/ws`;
    const wsInst = new WebSocket(wsUrl);
    wsInst.binaryType = 'arraybuffer';

    wsInst.onopen = () => {
      store.wasConnected = true;
      store.connectionStatus = 'connected';
      store.reconnectDelay = 5;
      store.sendSettings();
    };

    wsInst.onmessage = onMessage;

    wsInst.onclose = () => {
      store.connectionStatus = 'disconnected';
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

  function init() {
    store.isMobile =
      /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) &&
      window.innerWidth <= 900;
    connect();
  }

  return { connect, manualReconnect, init };
}
