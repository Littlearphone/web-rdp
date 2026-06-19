/**
 * JPEG 帧解码器
 *
 * 数据格式：[ox(4B)] [oy(4B)] [pw(4B)] [ph(4B)] [zoom(8B)] [JPEG数据]
 * 总头部 24 字节，JPEG 数据从偏移 24 开始。
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
    createImageBitmap(new Blob([jpg], { type: 'image/jpeg' })).then(bmp => {
      if (canvas.width !== bmp.width || canvas.height !== bmp.height) {
        canvas.width = bmp.width;
        canvas.height = bmp.height;
      }
      ctx.drawImage(bmp, 0, 0);
      bmp.close();
      onFirstFrame();
    }).catch(() => { /* 位图解码失败 */ });
  }

  function reset() { /* JPEG 无状态 */ }
  function close() { /* JPEG 无资源 */ }

  return { feed, reset, close };
}
