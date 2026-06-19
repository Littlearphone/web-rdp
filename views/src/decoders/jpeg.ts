/**
 * JPEG 帧解码器
 *
 * 数据格式：[ox(4B)] [oy(4B)] [pw(4B)] [ph(4B)] [zoom(8B)] [JPEG数据]
 * 总头部 24 字节，JPEG 数据从偏移 24 开始。
 *
 * 渲染策略：requestAnimationFrame 同步
 *   createImageBitmap 是异步的，旧 bitmap 完成时可能已有新帧 —
 *   用帧序号（frameId）标记，过期 bitmap 直接丢弃。
 *   rAF 中统一绘制，仅保留最新已解码位图。
 */

import type { Meta } from '@/types';

export interface JpegDecoder {
  feed(buf: ArrayBuffer): void;
  reset(): void;
  close(): void;
}

export function createJpegDecoder(
  canvas: HTMLCanvasElement,
  onMetaUpdate: (meta: Meta) => void,
  onFirstFrame: () => void,
): JpegDecoder {
  const ctx = canvas.getContext('2d')!;

  // ── rAF 渲染状态 ──
  let pendingBitmap: ImageBitmap | null = null;
  let rafId = 0;
  let firstFrameFired = false;

  // ── 帧序号（丢弃过期 bitmap） ──
  let frameId = 0;

  /** 确保 rAF 已调度（幂等），仅在有 pending bitmap 时才执行 */
  function ensureRaf() {
    if (rafId !== 0) return;
    rafId = requestAnimationFrame(() => {
      rafId = 0;
      const bmp = pendingBitmap;
      pendingBitmap = null;
      if (!bmp) return;

      if (canvas.width !== bmp.width || canvas.height !== bmp.height) {
        canvas.width = bmp.width;
        canvas.height = bmp.height;
      }
      ctx.drawImage(bmp, 0, 0);
      bmp.close();

      if (!firstFrameFired) {
        firstFrameFired = true;
        onFirstFrame();
      }
    });
  }

  function cancelRaf() {
    if (rafId !== 0) {
      cancelAnimationFrame(rafId);
      rafId = 0;
    }
  }

  function dropPendingBitmap() {
    if (pendingBitmap) {
      pendingBitmap.close();
      pendingBitmap = null;
    }
  }

  function feed(raw: ArrayBuffer) {
    const dv = new DataView(raw);
    const pw = dv.getInt32(8, true);
    const ph = dv.getInt32(12, true);

    // 长宽校验，防止误解析 H.264 数据
    if (pw < 100 || pw > 10000 || ph < 100 || ph > 10000) return;

    const m: Meta = {
      ox: dv.getInt32(0, true),
      oy: dv.getInt32(4, true),
      pw,
      ph,
      zoom: dv.getFloat64(16, true),
    };
    onMetaUpdate(m);

    // JPEG 数据从偏移 24 开始
    const jpg = new Uint8Array(raw, 24);
    const thisFid = ++frameId;

    createImageBitmap(new Blob([jpg], { type: 'image/jpeg' }))
      .then(bmp => {
        // 过期帧：后续 feed() 已递增 frameId，丢弃
        if (thisFid !== frameId) {
          bmp.close();
          return;
        }
        // 关闭上一帧未绘制的位图
        if (pendingBitmap) {
          pendingBitmap.close();
        }
        pendingBitmap = bmp;
        ensureRaf();
      })
      .catch(() => { /* 位图解码失败，静默丢弃 */ });
  }

  function reset() {
    cancelRaf();
    dropPendingBitmap();
    frameId = 0;
    firstFrameFired = false;
  }

  function close() {
    cancelRaf();
    dropPendingBitmap();
  }

  return { feed, reset, close };
}
