/**
 * H.264 WebCodecs 解码器
 *
 * 纯逻辑模块，不依赖 Vue。
 * 负责 Annex B → AVCC 转换 + VideoDecoder 生命周期管理。
 *
 * 渲染策略：requestAnimationFrame 同步
 *   解码回调仅暂存帧，rAF 中统一绘制，与显示器刷新率对齐，
 *   同一 vsync 间隔内多个解码帧只绘制最后一帧（自动跳帧）。
 *
 * 用法:
 *   const dec = createH264Decoder(canvas, onFirstFrame);
 *   dec.feed(arrayBuffer);  // 每次收到 WebSocket binary 消息时调用
 *   dec.reset();            // 切换格式或重连时
 *   dec.close();            // 组件卸载时
 */

export interface H264Decoder {
  feed(raw: ArrayBuffer): void;
  reset(): void;
  close(): void;
  /** 返回 VideoDecoder.decodeQueueSize，无解码器时返回 0 */
  getQueueSize(): number;
}

export function createH264Decoder(
  canvas: HTMLCanvasElement,
  onFirstFrame: () => void,
): H264Decoder {
  const ctx = canvas.getContext('2d')!;

  // ── 解码器状态 ──
  let decoder: VideoDecoder | null = null;
  let ready = false;
  let buf = new Uint8Array(0);
  let ts = 0;
  let firstDecode = false;

  // ── rAF 渲染状态 ──
  let pendingFrame: VideoFrame | null = null;
  let rafId = 0;
  let firstFrameFired = false;

  /** 确保 rAF 已调度（幂等），仅在有 pending frame 时才执行 */
  function ensureRaf() {
    if (rafId !== 0) return;
    rafId = requestAnimationFrame(() => {
      rafId = 0;
      const frame = pendingFrame;
      pendingFrame = null;
      if (!frame) return;

      const pw = frame.displayWidth;
      const ph = frame.displayHeight;
      if (canvas.width !== pw || canvas.height !== ph) {
        canvas.width = pw;
        canvas.height = ph;
      }
      ctx.drawImage(frame, 0, 0);
      frame.close();

      if (!firstFrameFired) {
        firstFrameFired = true;
        onFirstFrame();
      }
    });
  }

  // ── Annex B 起始码检测 ──

  /** 在 data[o:] 中查找起始码，返回位置（-1 表示未找到） */
  function findSC(d: Uint8Array, o: number): number {
    for (let i = o; i < d.length - 2; i++) {
      if (d[i] === 0 && d[i + 1] === 0) {
        if (d[i + 2] === 1) return i;                                       // 3 字节: 00 00 01
        if (i < d.length - 3 && d[i + 2] === 0 && d[i + 3] === 1) return i; // 4 字节: 00 00 00 01
      }
    }
    return -1;
  }

  /** 返回位置 pos 处的起始码长度（3 或 4） */
  function scLen(d: Uint8Array, pos: number): number {
    if (pos + 3 < d.length && d[pos] === 0 && d[pos + 1] === 0 &&
        d[pos + 2] === 0 && d[pos + 3] === 1) return 4;
    return 3;
  }

  // ── WebCodecs 配置描述符 ──

  /** 从 SPS NAL 提取 codec 字符串（如 avc1.42C034） */
  function buildCodecString(sps: Uint8Array): string {
    const p = sps[1].toString(16).toUpperCase().padStart(2, '0');
    const c = sps[2].toString(16).toUpperCase().padStart(2, '0');
    const l = sps[3].toString(16).toUpperCase().padStart(2, '0');
    return 'avc1.' + p + c + l;
  }

  /** 用 SPS + PPS 构建 avcC extradata（ISO 14496-15 格式） */
  function buildAvcC(sps: Uint8Array, pps: Uint8Array): Uint8Array {
    const d = new Uint8Array(11 + sps.length + pps.length);
    d[0] = 1;
    d[1] = sps[1];
    d[2] = sps[2];
    d[3] = sps[3];
    d[4] = 0xFF;
    d[5] = 0xE1; // lengthSizeMinusOne=3 + numOfSPS=1
    d[6] = sps.length >> 8;
    d[7] = sps.length & 0xFF;
    d.set(sps, 8);
    d[8 + sps.length] = 1; // numOfPPS = 1
    const p = 9 + sps.length;
    d[p] = pps.length >> 8;
    d[p + 1] = pps.length & 0xFF;
    d.set(pps, p + 2);
    return d;
  }

  // ── Annex B → AVCC 格式转换 ──

  /** Annex B [00 00 01] NAL → AVCC [4B长度] NAL */
  function annexbToAvcc(data: Uint8Array): Uint8Array {
    const parts: Uint8Array[] = [];
    let pos = 0;
    while (pos < data.length - 2) {
      const sc = findSC(data, pos);
      if (sc < 0) break;
      const sl = scLen(data, sc);
      pos = sc + sl;
      const n = findSC(data, pos);
      const end = n >= 0 ? n : data.length;
      const nal = data.slice(pos, end);
      const h = new Uint8Array(4);
      h[0] = (nal.length >> 24) & 0xFF;
      h[1] = (nal.length >> 16) & 0xFF;
      h[2] = (nal.length >> 8) & 0xFF;
      h[3] = nal.length & 0xFF;
      parts.push(h);
      parts.push(nal);
      pos = end;
    }
    if (parts.length === 0) return data;
    const total = parts.reduce((s, a) => s + a.length, 0);
    const r = new Uint8Array(total);
    let off = 0;
    for (const a of parts) {
      r.set(a, off);
      off += a.length;
    }
    return r;
  }

  // ── 解码器初始化 ──

  /** 扫描缓冲区中的 SPS/PPS，配置 VideoDecoder */
  function init(): boolean {
    let pos = 0;
    let sps: Uint8Array | null = null;
    let pps: Uint8Array | null = null;
    let firstIDR = -1;

    while (pos < buf.length - 3) {
      const sc = findSC(buf, pos);
      if (sc < 0) break;
      const sl = scLen(buf, sc);
      const n = findSC(buf, sc + sl);
      const end = n >= 0 ? n : buf.length;
      const nal = buf.slice(sc + sl, end);
      const t = nal[0] & 0x1F;
      if (t === 7) sps = nal;
      if (t === 8) pps = nal;
      if (t === 5 && firstIDR < 0) firstIDR = sc;
      if (n < 0) break;
      pos = n;
    }

    if (!sps || !pps) return false;

    // 关闭旧 VideoDecoder
    closeDecoder();

    decoder = new VideoDecoder({
      output: (frame: VideoFrame) => {
        // rAF 渲染：关闭上一帧，暂存当前帧
        if (pendingFrame) {
          pendingFrame.close();
        }
        pendingFrame = frame;
        ensureRaf();
      },
      error: (e: Error) => console.error('H264 解码错误:', e.message),
    });

    try {
      decoder.configure({
        codec: buildCodecString(sps),
        description: buildAvcC(sps, pps),
      });
    } catch (e) {
      console.error('H264 configure 失败:', (e as Error).message);
      closeDecoder();
      return false;
    }

    ready = true;
    ts = 0;
    firstDecode = false;

    // IDR 已在缓冲中 → 裁剪到 IDR 位置
    if (firstIDR >= 0) {
      buf = buf.slice(firstIDR);
      firstDecode = false;
    }
    return true;
  }

  /** 关闭并释放 VideoDecoder */
  function closeDecoder() {
    if (decoder) {
      try { decoder.close(); } catch (_) { /* 已关闭 */ }
      decoder = null;
    }
  }

  // ── 清理 rAF / pending 帧 ──

  function cancelRaf() {
    if (rafId !== 0) {
      cancelAnimationFrame(rafId);
      rafId = 0;
    }
  }

  function dropPendingFrame() {
    if (pendingFrame) {
      pendingFrame.close();
      pendingFrame = null;
    }
  }

  // ── 单个 NAL 解码 ──

  /** 解码单个 Annex B 格式的 NAL 单元 */
  function decode(data: Uint8Array) {
    const sl = scLen(data, 0);
    if (data.length < sl + 1 || !decoder) return;
    const t = data[sl] & 0x1F;

    // ── 激进队列保护 ──
    // 仅跳过 delta 帧不足以防止高分辨率下解码器队列无限增长 —
    // IDR 帧（200-500KB）解码耗时远超 P 帧，GOP 内连续 IDR 可堆积数秒延迟。
    // 队列 > 8 时 flush 解码器丢弃所有待解码帧，下一个 IDR 重建画面。
    const qs = decoder.decodeQueueSize;
    if (qs > 8) {
      decoder.flush().catch(() => {});
      // flush 后解码器可能状态异常，标记等待下一个 IDR
      firstDecode = true;
      return;
    }
    if (t !== 5 && qs > 3) return;

    try {
      const avcc = annexbToAvcc(data);
      decoder.decode(new EncodedVideoChunk({
        type: t === 5 ? 'key' : 'delta',
        timestamp: ts++ * 33333,
        data: avcc,
      }));
    } catch (_) { /* 解码器关闭或状态异常 */ }
  }

  // ── 数据入口 ──

  /** 接收 WebSocket 二进制数据，送入解码流水线 */
  function feed(raw: ArrayBuffer) {
    const chunk = new Uint8Array(raw);
    const t = new Uint8Array(buf.length + chunk.length);
    t.set(buf);
    t.set(chunk, buf.length);
    buf = t;

    // 解码器未就绪：尝试扫描 SPS/PPS
    if (!ready) {
      if (!init()) return;
    }

    // 循环提取并解码 NAL 单元
    while (ready && buf.length > 3) {
      const sl = scLen(buf, 0);

      // 首帧保护：configure() 后首个 decode 必须是 keyframe
      if (firstDecode) {
        const nalType = buf[sl] & 0x1F;
        if (nalType !== 5) {
          const sc = findSC(buf, sl);
          if (sc < 0) {
            buf = new Uint8Array(0);
            break;
          }
          buf = buf.slice(sc);
          continue;
        }
        firstDecode = false;
      }

      const sc = findSC(buf, sl);
      if (sc < 0) {
        // 仅一个 NAL 单元：整体解码
        const c = buf;
        buf = new Uint8Array(0);
        if (c.length > sl) decode(c);
        break;
      }
      // 切出第一个 NAL
      const c = buf.slice(0, sc);
      buf = buf.slice(sc);
      if (c.length > sl) decode(c);
    }
  }

  /** 重置解码器 */
  function reset() {
    cancelRaf();
    dropPendingFrame();
    closeDecoder();
    buf = new Uint8Array(0);
    ready = false;
    ts = 0;
    firstDecode = false;
    firstFrameFired = false;
  }

  /** 关闭解码器并释放资源 */
  function close() {
    cancelRaf();
    dropPendingFrame();
    closeDecoder();
  }

  /** 返回当前解码队列深度 */
  function getQueueSize(): number {
    return decoder?.decodeQueueSize ?? 0;
  }

  return { feed, reset, close, getQueueSize };
}
