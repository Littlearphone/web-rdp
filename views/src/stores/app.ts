import { defineStore } from 'pinia';
import { ref, computed } from 'vue';
import type { Meta, ResolutionOption, StreamFormat, ConnectionStatus, ControlStatus } from '@/types';

export const useAppStore = defineStore('app', () => {
  // ═══════════════════════════════════════════
  // 连接状态
  // ═══════════════════════════════════════════
  const ws = ref<WebSocket | null>(null);
  const connectionStatus = ref<ConnectionStatus>('connecting');
  const wasConnected = ref(false);
  const reconnectDelay = ref(5);
  const reconnectCountdown = ref(0);
  const reconnectTimer = ref<ReturnType<typeof setInterval> | null>(null);
  const showReconnectHint = ref(false);

  // ═══════════════════════════════════════════
  // 流设置
  // ═══════════════════════════════════════════
  const streamFormat = ref<StreamFormat>('jpeg');
  const useH264 = ref(true);

  // ═══════════════════════════════════════════
  // 用户设置
  // ═══════════════════════════════════════════
  const currentQ = ref(75);
  const currentFPS = ref(0);
  const currentMW = ref(0);
  const currentScreen = ref(0);

  // ═══════════════════════════════════════════
  // 显示信息
  // ═══════════════════════════════════════════
  const screenCount = ref(1);
  const origPw = ref(0);
  const origPh = ref(0);
  const meta = ref<Meta>({ ox: 0, oy: 0, pw: 1, ph: 1, zoom: 1.0 });

  // ═══════════════════════════════════════════
  // 性能统计（由后端推送）
  // ═══════════════════════════════════════════
  const statsUser = ref('');
  const statsFps = ref(0);
  const statsEncMs = ref(0);
  const statsKb = ref(0);
  const statsW = ref(0);
  const statsH = ref(0);
  const statsQ = ref(0);
  const statsOwner = ref('');
  const statsMaxRate = ref(0);

  // ═══════════════════════════════════════════
  // UI 状态
  // ═══════════════════════════════════════════
  const mobileResOpts = ref<ResolutionOption[]>([]);
  const mobileUIBuilt = ref(false);
  const lastResKey = ref('');
  const isMobile = ref(false);

  // ═══════════════════════════════════════════
  // 控制权限状态
  // ═══════════════════════════════════════════
  const controlStatus = ref<ControlStatus>('idle');
  const controlMsg = ref('');

  // ═══════════════════════════════════════════
  // 计算属性
  // ═══════════════════════════════════════════
  const basePw = computed(() => origPw.value || meta.value.pw);
  const basePh = computed(() => origPh.value || meta.value.ph);

  const serverAddr = computed(() => window.location.host);

  /** 是否有 H.264 解码能力 */
  const canH264 = computed(() => typeof VideoDecoder !== 'undefined');

  // ═══════════════════════════════════════════
  // 操作
  // ═══════════════════════════════════════════

  /** 发送 JSON 到后端 */
  function send(o: Record<string, unknown>) {
    if (ws.value && ws.value.readyState === WebSocket.OPEN) {
      ws.value.send(JSON.stringify(o));
    }
  }

  /** 发送当前设置（画质/分辨率/帧率/H.264） */
  function sendSettings() {
    send({
      quality: currentQ.value,
      maxw: currentMW.value,
      fps: currentFPS.value,
      // 仅在浏览器支持 WebCodecs 时才告知后端使用 H.264
      webcodecs: useH264.value && typeof VideoDecoder !== 'undefined',
    });
  }

  /** 发送键盘事件 */
  function sendKey(code: string, down: boolean) {
    send({ key: code, down });
  }

  /** 更新远程桌面原始分辨率 */
  function updateOrigRes(pw: number, ph: number): boolean {
    if (origPw.value === 0 || currentMW.value === 0 || pw > origPw.value || ph > origPh.value) {
      if (origPw.value !== pw || origPh.value !== ph) {
        origPw.value = pw;
        origPh.value = ph;
        return true;
      }
    }
    return false;
  }

  /** 清除重连计时器 */
  function clearReconnectTimer() {
    if (reconnectTimer.value) {
      clearInterval(reconnectTimer.value);
      reconnectTimer.value = null;
    }
  }

  return {
    // 状态
    ws, connectionStatus, wasConnected, reconnectDelay,
    reconnectCountdown, reconnectTimer, showReconnectHint,
    streamFormat, useH264,
    currentQ, currentFPS, currentMW, currentScreen,
    screenCount, origPw, origPh, meta,
    statsUser, statsFps, statsEncMs, statsKb, statsW, statsH, statsQ,
    statsOwner, statsMaxRate,
    mobileResOpts, mobileUIBuilt, lastResKey, isMobile,
    controlStatus, controlMsg,
    // 计算属性
    basePw, basePh, serverAddr, canH264,
    // 操作
    send, sendSettings, sendKey, updateOrigRes, clearReconnectTimer,
  };
});
