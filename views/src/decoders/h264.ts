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
  /** 用后端推送的 SPS/PPS（base64）提前配置解码器，省掉 init() 扫描 */
  preConfigure(spsB64: string, ppsB64: string): void;
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

      const t0 = DIAG ? performance.now() : 0;
      const pw = frame.displayWidth;
      const ph = frame.displayHeight;
      if (canvas.width !== pw || canvas.height !== ph) {
        canvas.width = pw;
        canvas.height = ph;
      }
      ctx.drawImage(frame, 0, 0);
      frame.close();
      if (DIAG) {
        const dt = performance.now() - t0;
        diagRender += dt;
        if (dt > 10) console.warn('[H264] 慢渲染:', dt.toFixed(1), 'ms canvas:', canvas.width, 'x', canvas.height);
        diagReport();
      }

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
    let lastIDR = -1;

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
      // 记录最后一个 IDR 而非第一个：跳过积压的旧帧，立即显示最新画面
      if (t === 5) lastIDR = sc;
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

    // 裁剪到最后一个 IDR 位置，跳过积压的旧帧
    // ffmpeg h264 格式会在 IDR 前重复 SPS/PPS，解码器可立即从该处开始
    if (lastIDR >= 0) {
      buf = buf.slice(lastIDR);
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

    // ── 队列保护 ──
    // IDR 帧（200-500KB）解码耗时远超 P 帧，队列堆积 → 延迟增加。
    // 分层策略：中度堆积时跳过 delta 帧减压；严重堆积时 flush 清空队列。
    // 阈值从 8→16 / 3→5 放宽，降低误触发导致等 IDR（GOP=300≈5s）的频率。
    const qs = decoder.decodeQueueSize;
    if (qs > 16) {
      decoder.flush().catch(() => {});
      // flush 后解码器可能状态异常，标记等待下一个 IDR
      firstDecode = true;
      return;
    }
    if (t !== 5 && qs > 5) return;

    try {
      const t0 = DIAG ? performance.now() : 0;
      const avcc = annexbToAvcc(data);
      decoder.decode(new EncodedVideoChunk({
        type: t === 5 ? 'key' : 'delta',
        timestamp: ts++ * 33333,
        data: avcc,
      }));
      if (DIAG) {
        const dt = performance.now() - t0;
        diagDecode += dt;
        if (dt > 10) console.warn('[H264] 慢解码:', dt.toFixed(1), 'ms NAL_len:', data.length, 'type:', t);
      }
    } catch (_) { /* 解码器关闭或状态异常 */ }
  }

  // ── 诊断：记录各阶段耗时（确认瓶颈后删除）──
  const DIAG = true;
  let diagFeedCopy = 0;     // feed() 中 buf 拷贝耗时累计
  let diagDecode = 0;       // decode() 中 annexbToAvcc + decoder.decode 耗时累计
  let diagRender = 0;       // rAF output 回调中 drawImage + canvas resize 耗时累计
  let diagSamples = 0;
  function diagReport() {
    if (!DIAG) return;
    diagSamples++;
    if (diagSamples % 120 === 0) { // 每~2秒(60fps)输出一次
      console.log('[H264 perf] feedCopy:', diagFeedCopy.toFixed(1), 'ms',
        'decode:', diagDecode.toFixed(1), 'ms',
        'render:', diagRender.toFixed(1), 'ms',
        '(累计 120 帧)');
      diagFeedCopy = 0; diagDecode = 0; diagRender = 0;
    }
  }
  // 首次 init() 后缓冲区可能仍有大量积压 NAL，同步 while 循环一次性
  // 处理上百帧会阻塞主线程，浏览器无法执行 rAF 和 decoder output 回调，
  // 导致解码队列溢出 → flush → 等 IDR → 卡顿。分批 drain 每批处理少量
  // NAL 后 setTimeout(0) 让出主线程，rAF 和 output 回调得以执行。
  let drainScheduled = false;

  /** 调度一次分批 drain（幂等，避免同时排多个 setTimeout） */
  function scheduleDrain() {
    if (drainScheduled) return;
    drainScheduled = true;
    setTimeout(() => {
      drainScheduled = false;
      drainBatch(20);
    }, 0);
  }

  /** 从 buf 中提取并解码 NAL，每次最多处理 maxNals 个。
   *  返回 true 表示 buf 中还有剩余数据待处理。 */
  function drainBatch(maxNals: number): boolean {
    let nalCount = 0;
    while (ready && buf.length > 3 && nalCount < maxNals) {
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
      nalCount++;
    }
    return ready && buf.length > 3;
  }

  // ── 数据入口 ──

  /** 接收 WebSocket 二进制数据，送入解码流水线 */
  function feed(raw: ArrayBuffer) {
    const t0 = DIAG ? performance.now() : 0;
    const chunk = new Uint8Array(raw);
    const t = new Uint8Array(buf.length + chunk.length);
    t.set(buf);
    t.set(chunk, buf.length);
    buf = t;
    if (DIAG) {
      const dt = performance.now() - t0;
      diagFeedCopy += dt;
      if (dt > 5) console.warn('[H264] 慢拷贝:', dt.toFixed(1), 'ms buf:', (buf.length/1024).toFixed(0), 'KB chunk:', raw.byteLength);
    }

    // 解码器未就绪：尝试扫描 SPS/PPS
    if (!ready) {
      // ── 缓冲区硬上限：防止初始化前帧无限积累 ──
      // 高帧率（60-180fps）下积累数秒可达数十 MB，解码器就绪后同步
      // 洪水式解码导致队列溢出 → flush → 等 IDR（~5s）→ 卡顿。
      // 超过 4MB 时仅保留尾部 ~1MB，在起始码边界裁剪。
      if (buf.length > 4 * 1024 * 1024) {
        const cutoff = buf.length - 1024 * 1024;
        const sc = findSC(buf, cutoff);
        if (sc >= 0) {
          buf = buf.slice(sc);
        } else {
          buf = buf.slice(cutoff);
        }
      }
      if (!init()) return;
      // init() 成功 → 启动分批 drain，避免同步 while 阻塞主线程
      scheduleDrain();
      return;
    }

    // 已就绪：直接 drain（缓冲区通常仅本帧数据，量小无需分批）
    if (drainBatch(999)) {
      // 如果还有剩余（不常见），调度继续 drain 避免积压
      scheduleDrain();
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

  /** 用后端推送的 SPS/PPS（base64）提前配置解码器。
   *  抢在二进制帧到达之前完成 configure()，省掉 init() 同步扫描延迟。
   *  若解码器已就绪则跳过（重复调用幂等）。 */
  function preConfigure(spsB64: string, ppsB64: string): void {
    if (ready) return; // 已就绪，幂等

    const sps = Uint8Array.from(atob(spsB64), c => c.charCodeAt(0));
    const pps = Uint8Array.from(atob(ppsB64), c => c.charCodeAt(0));

    closeDecoder();

    decoder = new VideoDecoder({
      output: (frame: VideoFrame) => {
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
      console.error('H264 preConfigure 失败:', (e as Error).message);
      closeDecoder();
      return;
    }

    ready = true;
    ts = 0;
    firstDecode = true; // 等第一个 IDR 到达才 decode
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

  return { feed, reset, close, getQueueSize, preConfigure };
}
