/**
 * 屏幕坐标映射
 *
 * 将浏览器像素坐标映射到远程桌面实际坐标，考虑：
 * 1. Canvas letterbox/pillarbox 后的实际显示区域
 * 2. 远程桌面的 DPI 缩放比和偏移量
 */

import { useAppStore } from '@/stores/app';

/** 获取事件的客户端坐标（兼容鼠标和触摸） */
export function eventPos(e: MouseEvent | TouchEvent): {
  clientX: number;
  clientY: number;
  target: EventTarget | null;
} {
  const te = e as TouchEvent;
  const t = (te.touches && te.touches[0]) || (te.changedTouches && te.changedTouches[0]);
  if (t) return { clientX: t.clientX, clientY: t.clientY, target: e.target };
  const me = e as MouseEvent;
  return { clientX: me.clientX, clientY: me.clientY, target: e.target };
}

/** 将浏览器像素坐标转换为远程桌面实际坐标 */
export function screenCoords(
  e: MouseEvent | TouchEvent,
  canvas: HTMLCanvasElement,
): { fx: number; fy: number } | Record<string, never> {
  const store = useAppStore();
  const p = eventPos(e);
  const r = canvas.getBoundingClientRect();
  const ra = store.meta.pw / store.meta.ph;
  const cr = r.width / r.height;

  // 计算 letterbox/pillarbox 后的实际显示区域
  let aw: number, ah: number;
  let ox = 0, oy = 0;
  if (ra > cr) {
    // 远程更宽 → 上下留黑边
    aw = r.width;
    ah = r.width / ra;
    oy = (r.height - ah) / 2;
  } else {
    // 远程更高 → 左右留黑边
    ah = r.height;
    aw = r.height * ra;
    ox = (r.width - aw) / 2;
  }

  // 鼠标在显示区域内的相对位置 [0, 1]
  const cx = (p.clientX - r.left - ox) / aw;
  const cy = (p.clientY - r.top - oy) / ah;

  // 鼠标在显示区域外
  if (cx < 0 || cx > 1 || cy < 0 || cy > 1) return {};

  return {
    fx: Math.round((store.meta.ox * store.meta.zoom) + (cx * store.meta.pw * store.meta.zoom)),
    fy: Math.round((store.meta.oy * store.meta.zoom) + (cy * store.meta.ph * store.meta.zoom)),
  };
}
