// ═══════════════════════════════════════════
// Web 远程控制 — 前端核心脚本
// 负责 WebSocket 通信、H.264/JPEG 解码、桌面/移动端 UI 控制
// ═══════════════════════════════════════════

// ── DOM 引用 ──
const canvas = document.getElementById('screen'),
      ctx = canvas.getContext('2d');
let cw = 0, ch = 0;

const select = document.getElementById('screen-id'),
      statusEl = document.getElementById('status-text');
const controlCheck = document.getElementById('enable-control'),
      qualitySlider = document.getElementById('quality');
const qualityVal = document.getElementById('quality-val'),
      maxwSelect = document.getElementById('maxw');
const statsEl = document.getElementById('stats'),
      reconnectHint = document.getElementById('reconnect-hint');
const reconnectMsg = document.getElementById('reconnect-msg'),
      bar = document.getElementById('bar'),
      view = document.getElementById('view');

// ── 全局状态 ──
const isMobile = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) && window.innerWidth <= 900;

let meta = { ox: 0, oy: 0, pw: 1, ph: 1, zoom: 1.0 },
    serverAddr = window.location.host;
let ws = null,
    reconnectTimer = null,
    reconnectDelay = 5,
    reconnectCountdown = 0,
    wasConnected = false,
    lastResKey = '';
let currentQ = 75,
    currentMW = 0,
    currentScreen = 0,
    screenCount = 1,
    mobileResOpts = [],
    mobileUIBuilt = false;
let streamFormat = 'jpeg',
    useH264 = true; // H.264 为默认编码，MJPEG 为备选
let origPw = 0, origPh = 0; // 远程桌面原始分辨率（缩放前），用于生成分辨率选项列表

// ── 辅助函数：WebSocket 发送 ──

/** 发送 JSON 对象到后端（WebSocket 已连接时） */
function send(o) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(o));
}

/** 将当前画质、分辨率和 H.264 设置发送到后端，后端据此重启 ffmpeg 或切换编码器 */
function sendSettings() {
    send({ quality: currentQ, maxw: currentMW, webcodecs: useH264 });
}

/** 发送键盘事件（按键名 + 按下/释放） */
function sendKey(code, down) {
    send({ key: code, down: down });
}


// ═══════════════════════════════════════════
// H.264 解码器（WebCodecs VideoDecoder）
//   仅在 streamFormat === 'h264' 且浏览器支持 VideoDecoder 时激活
//   数据流：WebSocket 二进制消息 → Annex B 缓冲区 → 切 NAL → AVCC 转换 → VideoDecoder.decode()
// ═══════════════════════════════════════════
const H264 = {
    // ── 解码器状态 ──
    decoder: null,             // VideoDecoder 实例
    ready: false,              // 解码器已 configure()，可以喂入 EncodedVideoChunk
    buf: new Uint8Array(0),    // Annex B 原始数据累积缓冲区
    ts: 0,                     // 帧时间戳计数器（每帧递增 33333，约 30fps 间隔）
    firstDecode: false,        // configure() 后首个 decode() 必须是 keyframe（IDR type=5），
                               // 此标志为 true 时 feed() 会跳过所有非 IDR NAL 直到遇到 type=5

    /** 重置解码器（切换格式或重连时调用） */
    reset() {
        this.close();
        this.buf = new Uint8Array(0);
        this.ready = false;
        this.ts = 0;
        this.firstDecode = false;
    },

    /** 关闭并释放 VideoDecoder 资源 */
    close() {
        if (this.decoder) {
            try { this.decoder.close(); } catch (_) { /* 忽略已关闭状态 */ }
            this.decoder = null;
        }
    },

    // ── H.264 Annex B 起始码检测 ──
    // 在 data[o:] 中查找起始码位置。
    // libx264 对 SPS/PPS/AUD 使用 4 字节起始码 (00 00 00 01)，
    // 对 slice NAL (IDR/P/B) 使用 3 字节起始码 (00 00 01)。
    // 必须同时检测两种，否则 slice NAL 会被整体漏掉。

    /** 在 data[o:] 中查找起始码，返回位置（-1 表示未找到） */
    findSC(d, o) {
        for (let i = o; i < d.length - 2; i++) {
            if (d[i] === 0 && d[i + 1] === 0) {
                if (d[i + 2] === 1) return i;                                       // 3 字节起始码: 00 00 01
                if (i < d.length - 3 && d[i + 2] === 0 && d[i + 3] === 1) return i; // 4 字节起始码: 00 00 00 01
            }
        }
        return -1;
    },

    /** 返回位置 pos 处的起始码长度（3 或 4），调用方保证 pos 是有效起始码位置 */
    scLen(d, pos) {
        if (pos + 3 < d.length && d[pos] === 0 && d[pos + 1] === 0 &&
            d[pos + 2] === 0 && d[pos + 3] === 1) return 4;
        return 3;
    },

    // ── WebCodecs 配置描述符构建 ──

    /** 从 SPS NAL 数据中提取编码器参数，构建 codec 字符串（如 avc1.42C034） */
    buildCodecString(sps) {
        const p = sps[1].toString(16).toUpperCase().padStart(2, '0');
        const c = sps[2].toString(16).toUpperCase().padStart(2, '0');
        const l = sps[3].toString(16).toUpperCase().padStart(2, '0');
        return 'avc1.' + p + c + l;
    },

    /** 用 SPS + PPS NAL 数据构建 avcC extradata（ISO 14496-15 格式） */
    buildAvcC(sps, pps) {
        const d = new Uint8Array(11 + sps.length + pps.length);
        d[0] = 1;
        d[1] = sps[1];
        d[2] = sps[2];
        d[3] = sps[3]; // version + profile + compat + level
        d[4] = 0xFF;
        d[5] = 0xE1; // lengthSizeMinusOne=3 (NAL 长度用 4 字节) + numOfSPS=1
        d[6] = sps.length >> 8;
        d[7] = sps.length & 0xFF; // SPS 数据长度（16bit 大端序）
        d.set(sps, 8);            // SPS 原始数据
        d[8 + sps.length] = 1;    // numOfPPS = 1
        const p = 9 + sps.length;
        d[p] = pps.length >> 8;
        d[p + 1] = pps.length & 0xFF; // PPS 数据长度（16bit 大端序）
        d.set(pps, p + 2);            // PPS 原始数据
        return d;
    },

    // ── Annex B → AVCC 格式转换 ──
    // Annex B: [00 00 01] NAL数据 [00 00 01] NAL数据 ...
    // AVCC:    [4字节长度] NAL数据 [4字节长度] NAL数据 ...
    // VideoDecoder 只接受 AVCC 封装；EncodedVideoChunk 的 data 必须是 AVCC 格式

    /** 将 Annex B 格式的 H.264 数据转换为 AVCC 格式 */
    annexbToAvcc(data) {
        const parts = [];
        let pos = 0;
        while (pos < data.length - 2) {
            const sc = this.findSC(data, pos);
            if (sc < 0) break;
            const sl = this.scLen(data, sc);
            pos = sc + sl; // 跳过当前起始码，pos 指向 NAL 数据开头
            const n = this.findSC(data, pos);
            const end = n >= 0 ? n : data.length; // 下一个起始码位置（或缓冲区末尾）
            const nal = data.slice(pos, end);      // 纯 NAL 数据（不含起始码）
            // 构建 4 字节大端序长度前缀
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
        // 合并所有分段为单个 Uint8Array
        const total = parts.reduce((s, a) => s + a.length, 0),
              r = new Uint8Array(total);
        let off = 0;
        for (const a of parts) {
            r.set(a, off);
            off += a.length;
        }
        return r;
    },

    // ── 解码器初始化 ──
    // 扫描累积缓冲区，查找 SPS(type=7) 和 PPS(type=8)，
    // 构建 avcC 描述符并 configure() VideoDecoder。
    // 同时记录首个 IDR(type=5) 起始码位置，避免 configure() 后首帧为非关键帧触发 WebCodecs 错误。

    /** 初始化 H.264 解码器。返回 true 表示解码器已配置就绪；false 表示 SPS/PPS 不完整 */
    init() {
        let pos = 0, sps = null, pps = null, firstIDR = -1;
        // 遍历缓冲区中所有 Annex B NAL 单元
        while (pos < this.buf.length - 3) {
            const sc = this.findSC(this.buf, pos);
            if (sc < 0) break;                                  // 定位当前起始码
            const sl = this.scLen(this.buf, sc);                // 起始码长度（3 或 4 字节）
            const n = this.findSC(this.buf, sc + sl);           // 查找下一个起始码
            const end = n >= 0 ? n : this.buf.length;
            const nal = this.buf.slice(sc + sl, end);           // 当前 NAL 数据（不含起始码）
            const t = nal[0] & 0x1F;                            // NAL 单元类型（低 5 位）
            if (t === 7) sps = nal;                             // SPS: 序列参数集
            if (t === 8) pps = nal;                             // PPS: 图像参数集
            if (t === 5 && firstIDR < 0) firstIDR = sc;         // IDR: 即时解码刷新关键帧
            // pos = n（下一个起始码位置），不是 n + sl —— 否则会跳过 PPS
            if (n < 0) break;
            pos = n;
        }
        if (!sps || !pps) return false;                         // SPS/PPS 不完整，等待更多数据

        // 关闭旧的 VideoDecoder（如果存在），创建新实例
        this.close();
        this.decoder = new VideoDecoder({
            /** 解码输出回调：将解码后的 VideoFrame 渲染到 Canvas */
            output: frame => {
                const pw = frame.displayWidth,
                      ph = frame.displayHeight;
                // 首帧或分辨率变化时调整 canvas 尺寸
                if (cw !== pw || ch !== ph) {
                    canvas.width = pw;
                    canvas.height = ph;
                    cw = pw;
                    ch = ph;
                    meta.pw = pw;
                    meta.ph = ph;
                    updateOrigRes(pw, ph);
                    const bp = basePw(pw),
                          bh = basePh(ph),
                          k = `${bp}x${bh}`;
                    if (k !== lastResKey) {
                        lastResKey = k;
                        if (isMobile) {
                            mobileUIBuilt = false;
                            updateMobileResolutions(bp, bh);
                        } else {
                            buildDesktopResolutions(bp, bh);
                        }
                    }
                }
                ctx.drawImage(frame, 0, 0);
                frame.close();
                if (statusEl.textContent === '切换中...') {
                    statusEl.textContent = '已连接';
                    statusEl.style.color = '#27ae60';
                }
            },
            error: e => console.error('H264 解码错误:', e.message)
        });

        // configure() 将 SPS/PPS 作为 extradata 注入解码器。
        // WebCodecs 要求 configure() 后的第一个 decode() 必须是 keyframe（IDR）。
        try {
            this.decoder.configure({
                codec: this.buildCodecString(sps),
                description: this.buildAvcC(sps, pps)
            });
        } catch (e) {
            console.error('H264 configure 失败:', e.message);
            this.close();
            return false;
        }
        this.ready = true;
        this.ts = 0;
        this.firstDecode = true;
        // IDR 已在缓冲中 → 裁剪到 IDR 位置，直接解码
        if (firstIDR >= 0) {
            this.buf = this.buf.slice(firstIDR);
            this.firstDecode = false;
        }
        return true;
    },

    // ── 单个 NAL 单元解码 ──
    // 将 Annex B 格式的 NAL 数据转为 AVCC 格式，封装为 EncodedVideoChunk 送入 VideoDecoder。
    // type=5 (IDR) 标记为 key 帧，其余标记为 delta 帧（含 SPS/PPS/SEI 等元数据）。

    /** 解码单个 NAL 单元 */
    decode(data) {
        const sl = this.scLen(data, 0);
        if (data.length < sl + 1 || !this.decoder) return;     // 至少需要起始码 + 1 字节 NAL 头
        const t = data[sl] & 0x1F;                             // 读取 NAL 类型
        try {
            const avcc = this.annexbToAvcc(data);              // Annex B → AVCC 格式转换
            // timestamp 递增 33333μs（≈33.3ms，约 30fps），使解码器按正确时序输出帧
            this.decoder.decode(new EncodedVideoChunk({
                type: t === 5 ? 'key' : 'delta',
                timestamp: this.ts++ * 33333,
                data: avcc
            }));
        } catch (_) { /* 解码器已关闭或状态异常，静默忽略 */ }
    },

    // ── 数据入口 ──
    // 由 onMessage 在收到 WebSocket 二进制消息时调用。
    // 每次调用将新数据追加到缓冲区，然后尝试初始化解码器或提取 NAL 单元解码。

    /** 接收 WebSocket 二进制数据并送入 H.264 解码流水线 */
    feed(raw) {
        // 追加原始数据到 Annex B 缓冲区
        const chunk = new Uint8Array(raw);
        const t = new Uint8Array(this.buf.length + chunk.length);
        t.set(this.buf);
        t.set(chunk, this.buf.length);
        this.buf = t;

        // 解码器未就绪：尝试扫描 SPS/PPS 并完成 configure()
        if (!this.ready) {
            if (!this.init()) return;
        }

        // 解码器就绪：循环提取并解码 NAL 单元
        while (this.ready && this.buf.length > 3) {
            const sl = this.scLen(this.buf, 0);                // 缓冲区首部起始码长度

            // ── 首帧保护：configure() 后第一个 decode() 必须是 keyframe (IDR type=5) ──
            // 如果缓冲区首部不是 IDR（可能是 SPS/PPS/SEI/AUD），跳过该 NAL 继续等待。
            if (this.firstDecode) {
                const t = this.buf[sl] & 0x1F;
                if (t !== 5) {
                    const sc = this.findSC(this.buf, sl);      // 找到当前 NAL 末尾
                    if (sc < 0) {
                        this.buf = new Uint8Array(0);
                        break;                                 // 已是最后一个 NAL，清空等待新数据
                    }
                    this.buf = this.buf.slice(sc);             // 跳过当前非 IDR NAL
                    continue;
                }
                this.firstDecode = false;                      // 找到 IDR，解除首帧保护
            }

            // ── 正常解码 ──
            const sc = this.findSC(this.buf, sl);              // 查找下一个起始码（当前 NAL 结束位置）
            if (sc < 0) {
                // 缓冲区只有一个 NAL 单元：整体作为一帧解码，清空缓冲区
                const c = this.buf;
                this.buf = new Uint8Array(0);
                if (c.length > sl) this.decode(c);
                break;
            }
            // 切出第一个 NAL 单元，剩余数据留在缓冲区供下次循环
            const c = this.buf.slice(0, sc);
            this.buf = this.buf.slice(sc);
            if (c.length > sl) this.decode(c);
        }
    }
};


// ═══════════════════════════════════════════
// JPEG 解码器（独立模块）
//   仅在 streamFormat === 'jpeg' 时激活
//   数据格式：[ox(4B)] [oy(4B)] [pw(4B)] [ph(4B)] [zoom(8B)] [JPEG数据]
// ═══════════════════════════════════════════
const JPEG = {
    reset() { /* JPEG 无状态，无需重置 */ },
    close() { /* JPEG 无资源，无需释放 */ },

    /** 解码并渲染 JPEG 帧（从 24 字节元数据头后开始） */
    feed(buf) {
        const dv = new DataView(buf);
        const pw = dv.getInt32(8, true),
              ph = dv.getInt32(12, true);
        // 长宽校验，防止 H.264 数据被误解析为 JPEG
        if (pw < 100 || pw > 10000 || ph < 100 || ph > 10000) return;

        meta = {
            ox: dv.getInt32(0, true),
            oy: dv.getInt32(4, true),
            pw, ph,
            zoom: dv.getFloat64(16, true)
        };
        updateOrigRes(pw, ph);

        const bp = basePw(pw),
              bh = basePh(ph),
              k = `${bp}x${bh}`;
        if (isMobile) {
            if (k !== lastResKey) {
                lastResKey = k;
                mobileUIBuilt = false;
                updateMobileResolutions(bp, bh);
            }
        } else {
            if (k !== lastResKey) {
                lastResKey = k;
                buildDesktopResolutions(bp, bh);
            }
        }

        // JPEG 数据从偏移 24 开始（跳过 24 字节元数据头）
        const jpg = new Uint8Array(buf, 24);
        createImageBitmap(new Blob([jpg], { type: 'image/jpeg' })).then(bmp => {
            if (cw !== bmp.width || ch !== bmp.height) {
                canvas.width = bmp.width;
                canvas.height = bmp.height;
                cw = bmp.width;
                ch = bmp.height;
            }
            ctx.drawImage(bmp, 0, 0);
            bmp.close();
            // 首帧渲染后恢复状态（从 H.264 切回 MJPEG 时触发）
            if (statusEl.textContent === '切换中...') {
                statusEl.textContent = '已连接';
                statusEl.style.color = '#27ae60';
            }
        }).catch(() => { /* 位图解码失败，静默忽略 */ });
    }
};


// ═══════════════════════════════════════════
// 解码器路由
// ═══════════════════════════════════════════

/** 重置所有解码器状态（切换编码格式或重连时调用） */
function resetDecoders() {
    H264.reset();
    JPEG.reset();
}


// ═══════════════════════════════════════════
// 分辨率选项（桌面 & 移动共用）
// ═══════════════════════════════════════════

/** 更新远程桌面原始分辨率。仅在以下情况更新：
 *  1. 尚未记录原始分辨率（首帧）
 *  2. 当前为原始分辨率模式（currentMW === 0，无缩放）
 *  3. 帧分辨率大于已记录的原始值（切换到了更大的显示器）
 */
function updateOrigRes(pw, ph) {
    if (origPw === 0 || currentMW === 0 || pw > origPw || ph > origPh) {
        if (origPw !== pw || origPh !== ph) {
            origPw = pw;
            origPh = ph;
            return true;
        }
    }
    return false;
}

/** 获取用于生成分辨率选项列表的基准宽度（优先使用原始分辨率） */
function basePw(pw) { return origPw || pw; }

/** 获取用于生成分辨率选项列表的基准高度（优先使用原始分辨率） */
function basePh(ph) { return origPh || ph; }

/** 根据基准分辨率生成分辨率选项列表。
 *  桌面端：原始 + [1080, 720, 480] 等比缩放
 *  手机端：适配屏幕 + [720, 480] 等比缩放（去重）
 */
function buildResolutions(pw, ph) {
    if (isMobile) {
        const bp = basePw(pw),
              bh = basePh(ph),
              m = Math.min(bp, Math.round(innerWidth * (devicePixelRatio || 1))),
              o = [{ label: '适配', w: m }];
        for (const t of [720, 480]) {
            if (t >= bh) continue;
            o.push({ label: `${t}p`, w: Math.round(bp * t / bh) });
        }
        return o.filter((o, i) => o.findIndex(x => x.w === o.w) === i);
    }
    const bp = basePw(pw),
          bh = basePh(ph),
          o = [{ label: `原始 (${bp}×${bh})`, value: 0 }];
    for (const t of [1080, 720, 480]) {
        if (t >= bh) continue;
        o.push({
            label: `${Math.round(bp * t / bh)}×${t}`,
            value: Math.round(bp * t / bh)
        });
    }
    return o;
}

/** 更新分辨率下拉菜单 UI，保持当前选中值不变 */
function applyResolutions(pw, ph) {
    updateOrigRes(pw, ph);
    const bp = basePw(pw),
          bh = basePh(ph),
          k = `${bp}x${bh}`;
    if (k === lastResKey) return;
    lastResKey = k;

    const o = buildResolutions(bp, bh),
          c = maxwSelect.value;
    maxwSelect.innerHTML = '';
    o.forEach(o => {
        const e = document.createElement('option');
        e.value = o.value || o.w;
        e.textContent = o.label;
        maxwSelect.appendChild(e);
    });
    maxwSelect.value = o.find(o => String(o.value || o.w) === c)
        ? c
        : String(o[0].value || o[0].w);
    currentMW = parseInt(maxwSelect.value);
    sendSettings();
}


// ═══════════════════════════════════════════
// WebSocket 连接管理
// ═══════════════════════════════════════════

/** 建立 WebSocket 连接到后端 */
function connect() {
    if (ws) {
        ws.onclose = null;
        ws.close();
        ws = null;
    }
    resetDecoders();
    clearReconnectTimer();
    reconnectHint.style.display = 'none';
    lastResKey = '';

    statusEl.textContent = '连接中...';
    statusEl.style.color = '#f1c40f';

    ws = new WebSocket(`ws://${serverAddr}/ws`);
    ws.binaryType = 'arraybuffer';

    // 连接成功：发送当前设置（画质/分辨率/H.264 开关），后端据此启动对应编码器
    ws.onopen = () => {
        wasConnected = true;
        statusEl.textContent = '已连接';
        statusEl.style.color = '#27ae60';
        reconnectDelay = 5;
        sendSettings();
    };

    ws.onmessage = onMessage;

    ws.onclose = () => {
        statusEl.textContent = '连接断开';
        statusEl.style.color = '#e74c3c';
        statsEl.innerHTML = '离线';
        resetDecoders();
        if (wasConnected) startReconnect();
    };

    ws.onerror = () => {
        if (!wasConnected) {
            statusEl.textContent = '连接失败';
            statusEl.style.color = '#e74c3c';
        }
    };
}

/** WebSocket 消息处理器。
 *  文本消息：JSON 格式（用户名、编码格式、性能统计、控制权信息）
 *  二进制消息：帧数据，路由到 H264 或 JPEG 解码器
 */
function onMessage(event) {
    if (typeof event.data === 'string') {
        try {
            const s = JSON.parse(event.data);

            // 用户名（仅首次连接时发送，用于标识当前用户）
            if (s.user) {
                statsEl.setAttribute('data-user', s.user);
            }

            // 编码格式通知（初始连接或切换编码器时发送）。
            // 独立于 user 检测：切换 H.264/MJPEG 时只发 format 不带 user。
            if (s.format) {
                if (streamFormat !== s.format) {
                    streamFormat = s.format;
                    statusEl.textContent = '切换中...';
                    statusEl.style.color = '#f1c40f';
                }
                return; // 格式消息不包含 stats 字段
            }

            // 性能统计（每秒由后端推送一次）
            if (isMobile) {
                const u = statsEl.getAttribute('data-user') || '',
                      p = matchMedia('(orientation:portrait)').matches;
                statsEl.innerHTML = p
                    ? `${u}&emsp;${s.fps}fps&emsp;${s.kb}KB`
                    : `${u}<br>${s.fps}fps<br>${s.kb}KB`;
            } else {
                statsEl.innerHTML =
                    `${s.w}×${s.h} Q${s.q} │ ${s.fps}fps │ ${s.enc_ms}ms │ ${s.kb}KB/f │ ${(s.kb * s.fps / 1024).toFixed(1)}MB/s`;
            }

            // 控制权信息
            if (s.owner !== undefined) {
                const me = statsEl.getAttribute('data-user') || '',
                      im = s.owner === me;
                controlCheck.disabled = !im && s.owner !== '';
                controlCheck.checked = im;
                controlCheck.parentElement.title = s.owner
                    ? `控制权:${s.owner}`
                    : '点击获取控制权';
                canvas.style.cursor = 'crosshair';
            }

            // 从 stats 同步坐标元数据（H.264 帧不含此信息）
            if (s.ox !== undefined) {
                meta.ox = s.ox;
                meta.oy = s.oy;
                meta.zoom = s.zoom;
            }

            // 屏幕数量变化
            if (s.screens > 0 && s.screens !== screenCount) {
                screenCount = s.screens;
                if (isMobile) {
                    mobileUIBuilt = false;
                    updateMobileUI();
                } else {
                    updateDesktopScreens();
                }
            }
        } catch (_) { /* JSON 解析失败，静默忽略 */ }
        return;
    }

    // 二进制帧：按当前 streamFormat 路由到对应解码器
    if (streamFormat === 'h264' && typeof VideoDecoder !== 'undefined') {
        H264.feed(event.data);
    } else {
        JPEG.feed(event.data);
    }
}

// 初始连接
connect();


// ═══════════════════════════════════════════
// 自动重连（指数退避：5s → 10s → 20s → 30s）
// ═══════════════════════════════════════════

/** 开始自动重连倒计时 */
function startReconnect() {
    scheduleReconnect();
}

/** 调度重连计时器，每秒更新 UI 倒计时 */
function scheduleReconnect() {
    clearReconnectTimer();
    reconnectCountdown = reconnectDelay;
    updateReconnectUI();
    reconnectHint.style.display = 'flex';

    reconnectTimer = setInterval(() => {
        reconnectCountdown--;
        if (reconnectCountdown <= 0) {
            clearReconnectTimer();
            reconnectHint.style.display = 'none';
            reconnectDelay = Math.min(reconnectDelay * 2, 30);
            connect();
            return;
        }
        updateReconnectUI();
    }, 1000);
}

/** 更新重连提示文本 */
function updateReconnectUI() {
    reconnectMsg.textContent = `${reconnectCountdown} 秒后自动重连...`;
}

/** 用户手动触发立即重连 */
function manualReconnect() {
    clearReconnectTimer();
    reconnectHint.style.display = 'none';
    connect();
}

/** 清除重连计时器 */
function clearReconnectTimer() {
    if (reconnectTimer) {
        clearInterval(reconnectTimer);
        reconnectTimer = null;
    }
}

// 点击状态文本也可触发重连
statusEl.onclick = () => {
    if (!ws || ws.readyState !== WebSocket.OPEN) {
        clearReconnectTimer();
        reconnectHint.style.display = 'none';
        connect();
    }
};


// ═══════════════════════════════════════════
// 桌面端控制（鼠标 + 键盘 + H.264 开关）
// ═══════════════════════════════════════════
if (!isMobile) {

    /** 根据屏幕数量更新显示器下拉选择器 */
    function updateDesktopScreens() {
        const c = select.value;
        select.innerHTML = '';
        for (let i = 0; i < screenCount; i++) {
            const e = document.createElement('option');
            e.value = i;
            e.textContent = i === 0 ? '主屏 (0)' : `副屏 (${i})`;
            select.appendChild(e);
        }
        select.value = c < screenCount ? c : '0';
    }

    /** 桌面端分辨率选项更新（仅刷新 UI，不触发 sendSettings） */
    function buildDesktopResolutions(pw, ph) {
        const o = buildResolutions(pw, ph),
              c = maxwSelect.value;
        maxwSelect.innerHTML = '';
        o.forEach(o => {
            const e = document.createElement('option');
            e.value = o.value;
            e.textContent = o.label;
            maxwSelect.appendChild(e);
        });
        const match = o.find(o => String(o.value) === c);
        maxwSelect.value = match ? c : String(o[0].value || o[0].w || '0');
    }

    // ── 控件事件绑定 ──
    qualitySlider.oninput = () => {
        qualityVal.textContent = qualitySlider.value;
    };
    qualitySlider.onchange = () => {
        currentQ = parseInt(qualitySlider.value);
        sendSettings();
    };

    maxwSelect.onchange = () => {
        currentMW = parseInt(maxwSelect.value);
        sendSettings();
    };

    select.onchange = () => {
        currentScreen = parseInt(select.value);
        currentMW = 0;
        send({ screen: currentScreen, maxw: 0 });
        lastResKey = '';
    };

    controlCheck.onchange = () => {
        send({ control: controlCheck.checked });
        canvas.style.cursor = 'crosshair';
    };

    // H.264 切换：重置解码器状态，显示"切换中..."提示，发送设置到后端
    const h264Toggle = document.getElementById('use-h264');
    if (h264Toggle) {
        h264Toggle.checked = useH264;
        h264Toggle.onchange = () => {
            useH264 = h264Toggle.checked;
            resetDecoders();
            statusEl.textContent = '切换中...';
            statusEl.style.color = '#f1c40f';
            sendSettings();
        };
    }

    // ── Canvas 鼠标事件 ──
    let active = false,
        dragStart = null,
        dragging = false,
        lastMoveSent = 0;

    /** mousedown：开始拖拽或单击 */
    canvas.onmousedown = e => {
        if (!controlCheck.checked || e.target !== canvas) return;
        e.preventDefault();
        if (e.button === 0) {
            active = true;
            const c = screenCoords(e);
            if (c.fx == null) return;
            dragStart = c;
            dragging = false;
        }
    };

    /** mousemove：拖拽中或光标位置同步（30ms 节流） */
    canvas.onmousemove = e => {
        if (!controlCheck.checked) return;
        if (!active || !dragStart) {
            const n = Date.now();
            if (n - lastMoveSent < 30) return; // 30ms 节流
            lastMoveSent = n;
            const c = screenCoords(e);
            if (c.fx != null) send({ mx: c.fx, my: c.fy });
            return;
        }
        const c = screenCoords(e);
        if (c.fx == null) return;
        if (Math.abs(c.fx - dragStart.fx) > 3 || Math.abs(c.fy - dragStart.fy) > 3) {
            dragging = true;
        }
    };

    /** mouseup：拖拽结束（发送拖拽事件）或单击（发送 HTTP 点击） */
    canvas.onmouseup = e => {
        if (!active) return;
        active = false;
        e.preventDefault();
        const c = screenCoords(e);
        if (c.fx == null) return;
        if (dragging) {
            send({
                dx1: dragStart.fx, dy1: dragStart.fy,
                dx2: c.fx, dy2: c.fy
            });
        } else {
            // 单击走 HTTP 端点降低延迟
            fetch(`http://${serverAddr}/click?x=${c.fx}&y=${c.fy}`).catch(() => {});
        }
        dragging = false;
        dragStart = null;
    };

    // 右键菜单：发送右键点击事件
    view.addEventListener('contextmenu', e => {
        if (!controlCheck.checked || e.target !== canvas) return;
        e.preventDefault();
        const c = screenCoords(e);
        if (c.fx == null) return;
        send({ rx: c.fx, ry: c.fy });
    });

    // ── 键盘事件（全局 capture 拦截） ──
    // 需要 preventDefault 的功能键列表（防止浏览器默认行为）
    const pc = new Set([
        'F1', 'F3', 'F5', 'F11', 'F12',
        'Tab', 'Escape',
        'AltLeft', 'AltRight',
        'ControlLeft', 'ControlRight',
        'MetaLeft', 'MetaRight'
    ]);

    window.addEventListener('keydown', e => {
        if (!controlCheck.checked) return;
        if (pc.has(e.code) || e.ctrlKey || e.altKey || e.metaKey) {
            e.preventDefault();
            e.stopPropagation();
        }
        sendKey(e.code, true);
    }, { capture: true });

    window.addEventListener('keyup', e => {
        if (!controlCheck.checked) return;
        e.preventDefault();
        e.stopPropagation();
        sendKey(e.code, false);
    }, { capture: true });
}


// ═══════════════════════════════════════════
// 手机端控件（自适应分辨率 + 画质选择）
// ═══════════════════════════════════════════
if (isMobile) {

    /** 更新手机端分辨率选项并刷新 UI */
    function updateMobileResolutions(pw, ph) {
        mobileResOpts = buildResolutions(pw, ph);
        if (!currentMW || !mobileResOpts.find(o => o.w === currentMW)) {
            currentMW = mobileResOpts[0].w;
        }
        mobileUIBuilt = false;
        updateMobileUI();
    }

    /** 构建/刷新手机端底栏 UI（画质 + 分辨率 + 屏幕切换） */
    function updateMobileUI() {
        // 仅需要刷新选中状态（UI 已构建）
        if (mobileUIBuilt) {
            bar.querySelectorAll('.ctrl-btn').forEach(b => b.classList.remove('active'));
            bar.querySelector(`[data-q="${currentQ}"]`)?.classList.add('active');
            bar.querySelector(`[data-mw="${currentMW}"]`)?.classList.add('active');
            const sb = bar.querySelector('.scr-btn');
            if (sb) sb.textContent = screenCount > 1 ? `屏${currentScreen}` : '主屏';
            return;
        }

        // 首次构建 UI
        mobileUIBuilt = true;

        // 清除旧控件
        bar.querySelectorAll('.ctrl-rows, .ctrl-row, .ctrl-btn, .ctrl-group')
           .forEach(e => e.remove());

        // 确保存在控件容器
        let rows = bar.querySelector('.ctrl-rows');
        if (!rows) {
            rows = document.createElement('div');
            rows.className = 'ctrl-rows';
            bar.appendChild(rows);
        }

        /** 创建新行容器 */
        const row = () => {
            const d = document.createElement('div');
            d.className = 'ctrl-row';
            rows.appendChild(d);
            return d;
        };

        /** 创建单个按钮 */
        const addBtn = (r, t, cl, click) => {
            const b = document.createElement('span');
            b.className = 'ctrl-btn ' + cl;
            b.textContent = t;
            b.onclick = click;
            r.appendChild(b);
        };

        /** 创建按钮组 */
        const addGroup = (r, btns) => {
            const g = document.createElement('span');
            g.className = 'ctrl-group';
            btns.forEach(b => {
                const e = document.createElement('span');
                e.className = 'ctrl-btn';
                if (b.active) e.classList.add('active');
                e.textContent = b.label;
                e.onclick = b.click;
                if (b.data) {
                    Object.entries(b.data).forEach(([k, v]) =>
                        e.setAttribute('data-' + k, v));
                }
                g.appendChild(e);
            });
            r.appendChild(g);
        };

        // 构建第一行：屏幕切换 + 画质选择
        const r = row();
        addBtn(r,
            screenCount > 1 ? `屏${currentScreen}` : '主屏',
            'scr-btn',
            () => {
                if (screenCount > 1) {
                    currentScreen = (currentScreen + 1) % screenCount;
                    currentMW = 0;
                    send({ screen: currentScreen, maxw: 0 });
                    lastResKey = '';
                    mobileUIBuilt = false;
                    updateMobileUI();
                }
            }
        );

        // 画质按钮组（低/中/高三档）
        addGroup(r, [
            {
                label: '低', active: currentQ === 40,
                data: { q: 40 },
                click: () => { currentQ = 40; sendSettings(); updateMobileUI(); }
            },
            {
                label: '中', active: currentQ === 60,
                data: { q: 60 },
                click: () => { currentQ = 60; sendSettings(); updateMobileUI(); }
            },
            {
                label: '高', active: currentQ === 80,
                data: { q: 80 },
                click: () => { currentQ = 80; sendSettings(); updateMobileUI(); }
            }
        ]);

        // 分辨率按钮组
        addGroup(r, mobileResOpts.map(o => ({
            label: o.label,
            active: currentMW === o.w,
            data: { mw: o.w },
            click: () => { currentMW = o.w; sendSettings(); updateMobileUI(); }
        })));
    }
}


// ═══════════════════════════════════════════
// 坐标转换（桌面 & 移动共用）
//   将屏幕像素坐标映射到远程桌面实际坐标（考虑缩放比和偏移量）
// ═══════════════════════════════════════════

/** 获取事件的客户端坐标（兼容鼠标和触摸事件） */
function eventPos(e) {
    const t = (e.touches && e.touches[0]) || (e.changedTouches && e.changedTouches[0]);
    if (t) return { clientX: t.clientX, clientY: t.clientY, target: e.target };
    return { clientX: e.clientX, clientY: e.clientY, target: e.target };
}

/** 将浏览器像素坐标转换为远程桌面实际坐标。
 *  计算逻辑：
 *  1. 获取 canvas 在视口中的位置和尺寸
 *  2. 计算 canvas 内的等比例显示区域（letterbox/pillarbox）
 *  3. 将鼠标在 canvas 内的相对位置映射到远程桌面的实际像素坐标
 *  返回 { fx, fy } 或空对象（鼠标在 canvas 显示区域外）
 */
function screenCoords(e) {
    const p = eventPos(e),
          r = canvas.getBoundingClientRect(),
          ra = meta.pw / meta.ph,        // 远程桌面的宽高比
          cr = r.width / r.height;       // canvas 元素的宽高比

    // 计算 letterbox/pillarbox 后的实际显示区域
    let aw, ah, ox = 0, oy = 0;
    if (ra > cr) {
        // 远程更宽 → 上下留黑边 (letterbox)
        aw = r.width;
        ah = r.width / ra;
        oy = (r.height - ah) / 2;
    } else {
        // 远程更高 → 左右留黑边 (pillarbox)
        ah = r.height;
        aw = r.height * ra;
        ox = (r.width - aw) / 2;
    }

    // 鼠标在显示区域内的相对位置 [0, 1]
    const cx = (p.clientX - r.left - ox) / aw,
          cy = (p.clientY - r.top - oy) / ah;

    // 鼠标在显示区域外 → 返回空对象
    if (cx < 0 || cx > 1 || cy < 0 || cy > 1) return {};

    // 映射到远程桌面实际坐标（考虑 DPI 缩放和偏移量）
    return {
        fx: Math.round((meta.ox * meta.zoom) + (cx * meta.pw * meta.zoom)),
        fy: Math.round((meta.oy * meta.zoom) + (cy * meta.ph * meta.zoom))
    };
}
