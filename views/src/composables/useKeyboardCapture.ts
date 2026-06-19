/**
 * 全局键盘拦截（桌面端）
 *
 * 当用户拥有控制权时，捕获所有键盘事件并转发到后端。
 * 对可能触发浏览器默认行为的功能键调用 preventDefault。
 *
 * 按键追踪：维护当前按下的按键集合。在以下场景自动释放所有按键，
 * 防止修饰键（Ctrl/Alt/Meta/Shift）在远程粘滞：
 *   - 窗口失焦（blur / focusout）
 *   - 标签页隐藏（visibilitychange / pagehide）
 *   - WebSocket 连接断开
 *   - 控制权被剥夺
 *   - 浏览器快捷键拦截后的焦点检查（safety check）
 *   - 组件卸载
 *
 * 浏览器快捷键拦截问题：
 * Ctrl+T / Ctrl+W / Ctrl+N / Alt+D 等快捷键无法通过 preventDefault 阻止，
 * 浏览器会在捕获阶段之后将其拦截（打开新标签页、聚焦地址栏等）。
 * 这会导致原页面的 keyup 事件丢失，修饰键在远端粘滞。
 * 为此引入双重保障：focusout 事件 + 修饰键组合后的焦点巡检。
 */

import { onMounted, onUnmounted, watch } from 'vue';
import { useAppStore } from '@/stores/app';

/** 需要 preventDefault 的功能键 */
const PREVENT_KEYS = new Set([
  'F1', 'F3', 'F5', 'F11', 'F12',
  'Tab', 'Escape',
  'AltLeft', 'AltRight',
  'ControlLeft', 'ControlRight',
  'MetaLeft', 'MetaRight',
]);

/** 修饰键 code，用于判断是否为浏览器快捷键组合 */
const MODIFIER_CODES = new Set([
  'ControlLeft', 'ControlRight',
  'AltLeft', 'AltRight',
  'MetaLeft', 'MetaRight',
]);

export function useKeyboardCapture() {
  const store = useAppStore();

  /** 当前按下的按键集合，用于窗口失焦时批量释放 */
  const pressedKeys = new Set<string>();

  /** 浏览器快捷键拦截后的焦点巡检定时器 */
  let safetyCheckTimer: ReturnType<typeof setTimeout> | null = null;

  /** 检查 WebSocket 是否可用，不可用时跳过发送但保持本地状态清理 */
  function wsOpen(): boolean {
    return store.ws !== null && store.ws.readyState === WebSocket.OPEN;
  }

  /** 检查当前文档是否仍有焦点，若无则释放所有按键 */
  function checkFocusAndRelease() {
    if (!document.hasFocus() && pressedKeys.size > 0) {
      releaseAllKeys();
    }
  }

  function onKeyDown(e: KeyboardEvent) {
    if (!store.statsOwner) return;
    // WebSocket 断开时不拦截任何按键，确保浏览器快捷键（F5 刷新等）正常
    if (!wsOpen()) return;
    // 输入框聚焦时放行所有组合键，确保 Ctrl+V 粘贴等正常工作
    const tag = (e.target as HTMLElement)?.tagName;
    const isInput = tag === 'INPUT' || tag === 'TEXTAREA' || (e.target as HTMLElement)?.isContentEditable;
    if (isInput) return;
    // 阻止默认行为但不阻止传播 — 浏览器需要事件正常传播来维护内部键盘状态
    if (PREVENT_KEYS.has(e.code) || e.ctrlKey || e.altKey || e.metaKey) {
      e.preventDefault();
    }
    // 避免重复按下导致计数偏差（某些平台会连续触发 keydown）
    if (!pressedKeys.has(e.code)) {
      pressedKeys.add(e.code);
      store.sendKey(e.code, true);
    }

    // ═══════════════════════════════════════════
    // 浏览器快捷键拦截检测
    // 当用户按下 Ctrl+T、Alt+D 等组合键时，浏览器可能在捕获阶段之后拦截该快捷键
    // （preventDefault 无法阻止），导致原页面丢失 keyup 事件。
    // 此处检测修饰键 + 非修饰键的组合，在两次时机检查焦点状态：
    //   1. 下一个动画帧（通常 <16ms，浏览器处理完快捷键后）
    //   2. 100ms 后兜底
    // ═══════════════════════════════════════════
    if ((e.ctrlKey || e.metaKey || e.altKey) && !MODIFIER_CODES.has(e.code)) {
      // 清除旧定时器，避免重复调度
      if (safetyCheckTimer !== null) {
        clearTimeout(safetyCheckTimer);
      }
      // 时机 1：下一个动画帧（浏览器完成当前帧处理）
      requestAnimationFrame(checkFocusAndRelease);
      // 时机 2：100ms 兜底（处理动画帧无法覆盖的异步焦点切换）
      safetyCheckTimer = setTimeout(() => {
        safetyCheckTimer = null;
        checkFocusAndRelease();
      }, 100);
    }
  }

  function onKeyUp(e: KeyboardEvent) {
    if (!store.statsOwner) return;
    // 无论 WebSocket 是否可用，都要清理本地状态，
    // 否则重连后 releaseAllKeys 会发送过期的 keyup
    pressedKeys.delete(e.code);
    // WebSocket 断开时不拦截按键，也不发送
    if (!wsOpen()) return;
    // keyup 上 preventDefault 防止释放按键后触发浏览器菜单等
    e.preventDefault();
    e.stopPropagation();
    store.sendKey(e.code, false);

    // 所有修饰键都释放时，取消安全检查定时器
    if (MODIFIER_CODES.has(e.code) && safetyCheckTimer !== null) {
      // 检查是否还有其他修饰键被按住
      let hasModifier = false;
      for (const c of pressedKeys) {
        if (MODIFIER_CODES.has(c)) { hasModifier = true; break; }
      }
      if (!hasModifier) {
        clearTimeout(safetyCheckTimer);
        safetyCheckTimer = null;
      }
    }
  }

  /** 释放所有当前按下的按键，防止窗口失焦后修饰键粘滞 */
  function releaseAllKeys() {
    // 清除安全检查定时器 — 既然我们已经主动释放，无需再检查
    if (safetyCheckTimer !== null) {
      clearTimeout(safetyCheckTimer);
      safetyCheckTimer = null;
    }
    if (pressedKeys.size === 0) return;
    for (const code of pressedKeys) {
      // 尽量发送 keyup，即使 WS 即将关闭
      store.sendKey(code, false);
    }
    pressedKeys.clear();
  }

  function onBlur() {
    releaseAllKeys();
  }

  function onFocusOut() {
    // focusout 可能先于 blur 触发，尤其在浏览器快捷键（Ctrl+T 等）拦截场景下
    // 检查文档是否真的失去焦点，避免误释放
    if (!document.hasFocus()) {
      releaseAllKeys();
    }
  }

  function onVisibilityChange() {
    if (document.hidden) {
      releaseAllKeys();
    }
  }

  function onPageHide() {
    releaseAllKeys();
  }

  onMounted(() => {
    window.addEventListener('keydown', onKeyDown, { capture: true });
    window.addEventListener('keyup', onKeyUp, { capture: true });
    window.addEventListener('blur', onBlur);
    window.addEventListener('focusout', onFocusOut);
    document.addEventListener('visibilitychange', onVisibilityChange);
    window.addEventListener('pagehide', onPageHide);
  });

  // 控制权被剥夺时释放所有按键
  watch(() => store.statsOwner, (newVal, oldVal) => {
    if (!newVal && oldVal) {
      releaseAllKeys();
    }
  });

  // WebSocket 连接断开时释放所有按键
  // 场景：用户按住 Ctrl 期间网络断开，keyup 事件虽触发但 sendKey 被跳过，
  // 远端机器仍认为 Ctrl 被按住。此处确保断连时即刻清理。
  watch(() => store.connectionStatus, (newStatus, oldStatus) => {
    if ((newStatus === 'disconnected' || newStatus === 'failed') && oldStatus === 'connected') {
      releaseAllKeys();
    }
  });

  onUnmounted(() => {
    window.removeEventListener('keydown', onKeyDown, { capture: true });
    window.removeEventListener('keyup', onKeyUp, { capture: true });
    window.removeEventListener('blur', onBlur);
    window.removeEventListener('focusout', onFocusOut);
    document.removeEventListener('visibilitychange', onVisibilityChange);
    window.removeEventListener('pagehide', onPageHide);
    // 清除安全检查定时器
    if (safetyCheckTimer !== null) {
      clearTimeout(safetyCheckTimer);
      safetyCheckTimer = null;
    }
    releaseAllKeys();
  });
}
