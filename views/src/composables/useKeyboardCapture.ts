/**
 * 全局键盘拦截（桌面端）
 *
 * 当用户拥有控制权时，捕获所有键盘事件并转发到后端。
 * 对可能触发浏览器默认行为的功能键调用 preventDefault。
 */

import { onMounted, onUnmounted } from 'vue';
import { useAppStore } from '@/stores/app';

/** 需要 preventDefault 的功能键 */
const PREVENT_KEYS = new Set([
  'F1', 'F3', 'F5', 'F11', 'F12',
  'Tab', 'Escape',
  'AltLeft', 'AltRight',
  'ControlLeft', 'ControlRight',
  'MetaLeft', 'MetaRight',
]);

export function useKeyboardCapture() {
  const store = useAppStore();

  function onKeyDown(e: KeyboardEvent) {
    if (!store.statsOwner) return;
    if (PREVENT_KEYS.has(e.code) || e.ctrlKey || e.altKey || e.metaKey) {
      e.preventDefault();
      e.stopPropagation();
    }
    store.sendKey(e.code, true);
  }

  function onKeyUp(e: KeyboardEvent) {
    if (!store.statsOwner) return;
    e.preventDefault();
    e.stopPropagation();
    store.sendKey(e.code, false);
  }

  onMounted(() => {
    window.addEventListener('keydown', onKeyDown, { capture: true });
    window.addEventListener('keyup', onKeyUp, { capture: true });
  });

  onUnmounted(() => {
    window.removeEventListener('keydown', onKeyDown, { capture: true });
    window.removeEventListener('keyup', onKeyUp, { capture: true });
  });
}
