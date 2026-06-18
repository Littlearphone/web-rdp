// ═══════════════════════════════════════════
// DOM 引用 & 全局状态
// ═══════════════════════════════════════════
const canvas = document.getElementById('screen'), ctx = canvas.getContext('2d');
let cw = 0, ch = 0;
const select = document.getElementById('screen-id'), statusEl = document.getElementById('status-text');
const controlCheck = document.getElementById('enable-control'), qualitySlider = document.getElementById('quality');
const qualityVal = document.getElementById('quality-val'), maxwSelect = document.getElementById('maxw');
const statsEl = document.getElementById('stats'), reconnectHint = document.getElementById('reconnect-hint');
const reconnectMsg = document.getElementById('reconnect-msg'), bar = document.getElementById('bar'), view = document.getElementById('view');
const isMobile = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) && window.innerWidth <= 900;

let meta = { ox: 0, oy: 0, pw: 1, ph: 1, zoom: 1.0 }, serverAddr = window.location.host;
let ws = null, reconnectTimer = null, reconnectDelay = 5, reconnectCountdown = 0, wasConnected = false, lastResKey = '';
let currentQ = 75, currentMW = 0, currentScreen = 0, screenCount = 1, mobileResOpts = [], mobileUIBuilt = false;
let streamFormat = 'jpeg', useH264 = false;

function send(o) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(o)); }
function sendSettings() { send({ quality: currentQ, maxw: currentMW, webcodecs: useH264 }); }
function sendKey(code, down) { send({ key: code, down: down }); }

// ═══════════════════════════════════════════
// H.264 解码器（独立模块）
//   仅在 streamFormat === 'h264' 时激活
//   依赖：canvas, cw, ch, meta（全局）
// ═══════════════════════════════════════════
const H264 = {
  decoder: null, ready: false, buf: new Uint8Array(0), ts: 0,
  reset() { this.close(); this.buf = new Uint8Array(0); this.ready = false; this.ts = 0; this.firstDecode = false; },
  close() { if (this.decoder) { try { this.decoder.close() } catch (_) { } this.decoder = null; } },

  // 查找起始码（3字节 00 00 01 或 4字节 00 00 00 01），返回位置
  findSC(d, o) {
    for (let i = o; i < d.length - 2; i++) {
      if (d[i] === 0 && d[i + 1] === 0) {
        if (d[i + 2] === 1) return i;                       // 3字节
        if (i < d.length - 3 && d[i + 2] === 0 && d[i + 3] === 1) return i; // 4字节
      }
    }
    return -1;
  },
  // 返回位置 pos 处起始码的长度（3 或 4）
  scLen(d, pos) {
    if (pos + 3 < d.length && d[pos] === 0 && d[pos + 1] === 0 && d[pos + 2] === 0 && d[pos + 3] === 1) return 4;
    return 3;
  },

  buildCodecString(sps) {
    // sps[1]=profile_idc, sps[2]=constraint flags, sps[3]=level_idc
    const p = sps[1].toString(16).toUpperCase().padStart(2, '0');
    const c = sps[2].toString(16).toUpperCase().padStart(2, '0');
    const l = sps[3].toString(16).toUpperCase().padStart(2, '0');
    return 'avc1.' + p + c + l;
  },

  buildAvcC(sps, pps) {
    const d = new Uint8Array(11 + sps.length + pps.length);
    d[0] = 1; d[1] = sps[1]; d[2] = sps[2]; d[3] = sps[3]; d[4] = 0xFF; d[5] = 0xE1;
    d[6] = sps.length >> 8; d[7] = sps.length & 0xFF; d.set(sps, 8); d[8 + sps.length] = 1;
    const p = 9 + sps.length; d[p] = pps.length >> 8; d[p + 1] = pps.length & 0xFF; d.set(pps, p + 2);
    return d;
  },

  annexbToAvcc(data) {
    const parts = []; let pos = 0;
    while (pos < data.length - 2) {
      const sc = this.findSC(data, pos); if (sc < 0) break;
      const sl = this.scLen(data, sc); pos = sc + sl;
      const n = this.findSC(data, pos); const end = n >= 0 ? n : data.length;
      const nal = data.slice(pos, end);
      const h = new Uint8Array(4); h[0] = (nal.length >> 24) & 0xFF; h[1] = (nal.length >> 16) & 0xFF; h[2] = (nal.length >> 8) & 0xFF; h[3] = nal.length & 0xFF;
      parts.push(h); parts.push(nal); pos = end;
    }
    if (parts.length === 0) return data;
    const total = parts.reduce((s, a) => s + a.length, 0), r = new Uint8Array(total); let off = 0;
    for (const a of parts) { r.set(a, off); off += a.length; }
    return r;
  },

  init() {
    let pos = 0, sps = null, pps = null, spsSC = -1, ppsSC = -1, firstIDR = -1, nalCnt = 0;
    while (pos < this.buf.length - 3) {
      const sc = this.findSC(this.buf, pos); if (sc < 0) break;
      const sl = this.scLen(this.buf, sc);
      const n = this.findSC(this.buf, sc + sl); const end = n >= 0 ? n : this.buf.length;
      const nal = this.buf.slice(sc + sl, end); const t = nal[0] & 0x1F;
      nalCnt++; console.log('H264 init: NAL#' + nalCnt + ' type=' + t + ' len=' + nal.length + ' sc=' + sc + ' sl=' + sl);
      if (t === 7) { sps = nal; spsSC = sc; }
      if (t === 8) { pps = nal; ppsSC = sc; }
      if (t === 5 && firstIDR < 0) firstIDR = sc;
      if (n < 0) break; pos = n;
    }
    if (!sps || !pps) { console.log('H264 init: 等待 SPS/PPS, buf=' + this.buf.length + 'B'); return false; }
    console.log('H264 init: SPS=' + sps.length + 'B PPS=' + pps.length + 'B firstIDR@' + firstIDR + ' codec=' + this.buildCodecString(sps));
    this.close();
    this.decoder = new VideoDecoder({
      output: frame => {
        if (cw !== frame.displayWidth || ch !== frame.displayHeight) { canvas.width = frame.displayWidth; canvas.height = frame.displayHeight; cw = frame.displayWidth; ch = frame.displayHeight; }
        ctx.drawImage(frame, 0, 0); frame.close();
        if (statusEl.textContent === '切换中...') { statusEl.textContent = '已连接'; statusEl.style.color = '#27ae60'; }
      },
      error: e => console.error('H264 解码错误:', e.message, e)
    });
    try {
      this.decoder.configure({ codec: this.buildCodecString(sps), description: this.buildAvcC(sps, pps) });
      console.log('H264 init: VideoDecoder 已配置 ready=true');
      this.ready = true; this.ts = 0; this.firstDecode = true;
      // 裁剪到第一个 IDR（如有），否则保留缓冲区等 IDR 到来后在 feed() 中跳过非关键帧
      if (firstIDR >= 0) { this.buf = this.buf.slice(firstIDR); this.firstDecode = false; }
      return true;
    } catch (e) {
      console.error('H264 configure 失败:', e.message);
      this.close();
      return false;
    }
  },

  decode(data) {
    const sl = this.scLen(data, 0);
    if (data.length < sl + 1 || !this.decoder) return;
    const t = data[sl] & 0x1F;
    try {
      const avcc = this.annexbToAvcc(data);
      this.decoder.decode(new EncodedVideoChunk({ type: t === 5 ? 'key' : 'delta', timestamp: this.ts++ * 33333, data: avcc }));
      console.log('H264 decode: type=' + t + ' ts=' + (this.ts-1) + ' len=' + data.length);
    } catch (e) { console.error('H264 decode异常:', e.message); }
  },

  feed(raw) {
    const chunk = new Uint8Array(raw);
    const t = new Uint8Array(this.buf.length + chunk.length); t.set(this.buf); t.set(chunk, this.buf.length); this.buf = t;
    if (!this.ready) { if (!this.init()) return; }
    while (this.ready && this.buf.length > 3) {
      const sl = this.scLen(this.buf, 0);
      // 等待首个 IDR 关键帧：configure() 后第一个 decode() 必须是 keyframe
      if (this.firstDecode) {
        const t = this.buf[sl] & 0x1F;
        if (t !== 5) {
          const sc = this.findSC(this.buf, sl);
          if (sc < 0) { this.buf = new Uint8Array(0); break; }
          this.buf = this.buf.slice(sc); // 跳过此非 IDR NAL
          console.log('H264: 跳过非关键帧 type=' + t + ', 等待 IDR...');
          continue;
        }
        this.firstDecode = false;
      }
      const sc = this.findSC(this.buf, sl);
      if (sc < 0) { const c = this.buf; this.buf = new Uint8Array(0); if (c.length > sl) this.decode(c); break; }
      const c = this.buf.slice(0, sc); this.buf = this.buf.slice(sc);
      if (c.length > sl) this.decode(c);
    }
  }
};

// ═══════════════════════════════════════════
// JPEG 解码器（独立模块）
//   仅在 streamFormat === 'jpeg' 时激活
//   依赖：canvas, cw, ch, meta（全局）
// ═══════════════════════════════════════════
const JPEG = {
  reset() { },
  close() { },
  feed(buf) {
    const dv = new DataView(buf);
    const pw = dv.getInt32(8, true), ph = dv.getInt32(12, true);
    if (pw < 100 || pw > 10000 || ph < 100 || ph > 10000) return; // 校验防止误解析
    meta = { ox: dv.getInt32(0, true), oy: dv.getInt32(4, true), pw, ph, zoom: dv.getFloat64(16, true) };
    if (isMobile) { const k = `${pw}x${ph}`; if (k !== lastResKey) { lastResKey = k; mobileUIBuilt = false; updateMobileResolutions(pw, ph); } }
    else { const k = `${pw}x${ph}`; if (k !== lastResKey) { lastResKey = k; buildDesktopResolutions(pw, ph); } }
    const jpg = new Uint8Array(buf, 24);
    createImageBitmap(new Blob([jpg], { type: 'image/jpeg' })).then(bmp => {
      if (cw !== bmp.width || ch !== bmp.height) { canvas.width = bmp.width; canvas.height = bmp.height; cw = bmp.width; ch = bmp.height; }
      ctx.drawImage(bmp, 0, 0); bmp.close();
    }).catch(() => { });
  }
};

// ═══════════════════════════════════════════
// 解码器路由
// ═══════════════════════════════════════════
function resetDecoders() { H264.reset(); JPEG.reset(); }

// ═══════════════════════════════════════════
// 分辨率选项（桌面 & 移动共用）
// ═══════════════════════════════════════════
function buildResolutions(pw, ph) {
  if (isMobile) { const m = Math.min(pw, Math.round(innerWidth * (devicePixelRatio || 1))), o = [{ label: '适配', w: m }]; for (const t of [720, 480]) { if (t >= ph) continue; o.push({ label: `${t}p`, w: Math.round(pw * t / ph) }); } return o.filter((o, i) => o.findIndex(x => x.w === o.w) === i); }
  const o = [{ label: `原始 (${pw}×${ph})`, value: 0 }]; for (const t of [1080, 720, 480]) { if (t >= ph) continue; o.push({ label: `${Math.round(pw * t / ph)}×${t}`, value: Math.round(pw * t / ph) }); } return o;
}
function applyResolutions(pw, ph) { const k = `${pw}x${ph}`; if (k === lastResKey) return; lastResKey = k; const o = buildResolutions(pw, ph), c = maxwSelect.value; maxwSelect.innerHTML = ''; o.forEach(o => { const e = document.createElement('option'); e.value = o.value || o.w; e.textContent = o.label; maxwSelect.appendChild(e); }); maxwSelect.value = o.find(o => String(o.value || o.w) === c) ? c : String(o[0].value || o[0].w); currentMW = parseInt(maxwSelect.value); sendSettings(); }

// ═══════════════════════════════════════════
// WebSocket 连接
// ═══════════════════════════════════════════
function connect() {
  if (ws) { ws.onclose = null; ws.close(); ws = null; }
  resetDecoders(); clearReconnectTimer(); reconnectHint.style.display = 'none'; lastResKey = '';
  statusEl.textContent = '连接中...'; statusEl.style.color = '#f1c40f';
  ws = new WebSocket(`ws://${serverAddr}/ws`); ws.binaryType = 'arraybuffer';
  ws.onopen = () => { wasConnected = true; statusEl.textContent = '已连接'; statusEl.style.color = '#27ae60'; reconnectDelay = 5; sendSettings(); };
  ws.onmessage = onMessage;
  ws.onclose = () => { statusEl.textContent = '连接断开'; statusEl.style.color = '#e74c3c'; statsEl.innerHTML = '离线'; resetDecoders(); if (wasConnected) startReconnect(); };
  ws.onerror = () => { if (!wasConnected) { statusEl.textContent = '连接失败'; statusEl.style.color = '#e74c3c'; } };
}

function onMessage(event) {
  if (typeof event.data === 'string') {
    try {
      const s = JSON.parse(event.data);
      // 用户名
      if (s.user) { statsEl.setAttribute('data-user', s.user); }
      if (s.format) { if (streamFormat !== s.format) { console.log('格式切换: ' + streamFormat + ' → ' + s.format); streamFormat = s.format; statusEl.textContent = '切换中...'; statusEl.style.color = '#f1c40f'; } return; }
      // 性能统计
      if (isMobile) { const u = statsEl.getAttribute('data-user') || '', p = matchMedia('(orientation:portrait)').matches; statsEl.innerHTML = p ? `${u}&emsp;${s.fps}fps&emsp;${s.kb}KB` : `${u}<br>${s.fps}fps<br>${s.kb}KB`; }
      else { statsEl.innerHTML = `${s.w}×${s.h} Q${s.q} │ ${s.fps}fps │ ${s.enc_ms}ms │ ${s.kb}KB/f │ ${(s.kb * s.fps / 1024).toFixed(1)}MB/s`; }
      if (s.owner !== undefined) { const me = statsEl.getAttribute('data-user') || '', im = s.owner === me; controlCheck.disabled = !im && s.owner !== ''; controlCheck.checked = im; controlCheck.parentElement.title = s.owner ? `控制权:${s.owner}` : '点击获取控制权'; canvas.style.cursor = 'crosshair'; }
      if (s.screens > 0 && s.screens !== screenCount) { screenCount = s.screens; isMobile ? (mobileUIBuilt = false, updateMobileUI()) : updateDesktopScreens(); }
    } catch (_) { }
    return;
  }
  // 二进制帧 → 路由到对应解码器
  if (streamFormat === 'h264' && typeof VideoDecoder !== 'undefined') { H264.feed(event.data); }
  else { if (streamFormat === 'h264') { console.log('H264: VideoDecoder不可用!'); } JPEG.feed(event.data); }
}
connect();

// ═══════════════════════════════════════════
// 自动重连
// ═══════════════════════════════════════════
function startReconnect() { scheduleReconnect(); }
function scheduleReconnect() { clearReconnectTimer(); reconnectCountdown = reconnectDelay; updateReconnectUI(); reconnectHint.style.display = 'flex'; reconnectTimer = setInterval(() => { reconnectCountdown--; if (reconnectCountdown <= 0) { clearReconnectTimer(); reconnectHint.style.display = 'none'; reconnectDelay = Math.min(reconnectDelay * 2, 30); connect(); return; } updateReconnectUI(); }, 1000); }
function updateReconnectUI() { reconnectMsg.textContent = `${reconnectCountdown} 秒后自动重连...`; }
function manualReconnect() { clearReconnectTimer(); reconnectHint.style.display = 'none'; connect(); }
function clearReconnectTimer() { if (reconnectTimer) { clearInterval(reconnectTimer); reconnectTimer = null; } }
statusEl.onclick = () => { if (!ws || ws.readyState !== WebSocket.OPEN) { clearReconnectTimer(); reconnectHint.style.display = 'none'; connect(); } };

// ═══════════════════════════════════════════
// 桌面端控制
// ═══════════════════════════════════════════
if (!isMobile) {
  function updateDesktopScreens() { const c = select.value; select.innerHTML = ''; for (let i = 0; i < screenCount; i++) { const e = document.createElement('option'); e.value = i; e.textContent = i === 0 ? '主屏 (0)' : `副屏 (${i})`; select.appendChild(e); } select.value = c < screenCount ? c : '0'; }
  function buildDesktopResolutions(pw, ph) { const o = buildResolutions(pw, ph), c = maxwSelect.value; maxwSelect.innerHTML = ''; o.forEach(o => { const e = document.createElement('option'); e.value = o.value; e.textContent = o.label; maxwSelect.appendChild(e); }); maxwSelect.value = o.find(o => String(o.value) === c) ? c : '0'; }
  qualitySlider.oninput = () => { qualityVal.textContent = qualitySlider.value; }; qualitySlider.onchange = () => { currentQ = parseInt(qualitySlider.value); sendSettings(); }; maxwSelect.onchange = () => { currentMW = parseInt(maxwSelect.value); sendSettings(); }; select.onchange = () => { currentScreen = parseInt(select.value); currentMW = 0; send({ screen: currentScreen, maxw: 0 }); lastResKey = ''; };
  controlCheck.onchange = () => { send({ control: controlCheck.checked }); canvas.style.cursor = 'crosshair'; };
  const h264Toggle = document.getElementById('use-h264'); if (h264Toggle) { h264Toggle.checked = useH264; h264Toggle.onchange = () => { useH264 = h264Toggle.checked; resetDecoders(); statusEl.textContent = '切换中...'; statusEl.style.color = '#f1c40f'; sendSettings(); }; }
  let active = false, dragStart = null, dragging = false, lastMoveSent = 0;
  canvas.onmousedown = e => { if (!controlCheck.checked || e.target !== canvas) return; e.preventDefault(); if (e.button === 0) { active = true; const c = screenCoords(e); if (c.fx == null) return; dragStart = c; dragging = false; } };
  canvas.onmousemove = e => { if (!controlCheck.checked) return; if (!active || !dragStart) { const n = Date.now(); if (n - lastMoveSent < 30) return; lastMoveSent = n; const c = screenCoords(e); if (c.fx != null) send({ mx: c.fx, my: c.fy }); return; } const c = screenCoords(e); if (c.fx == null) return; if (Math.abs(c.fx - dragStart.fx) > 3 || Math.abs(c.fy - dragStart.fy) > 3) dragging = true; };
  canvas.onmouseup = e => { if (!active) return; active = false; e.preventDefault(); const c = screenCoords(e); if (c.fx == null) return; if (dragging) { send({ dx1: dragStart.fx, dy1: dragStart.fy, dx2: c.fx, dy2: c.fy }); } else { fetch(`http://${serverAddr}/click?x=${c.fx}&y=${c.fy}`).catch(() => { }); } dragging = false; dragStart = null; };
  view.addEventListener('contextmenu', e => { if (!controlCheck.checked || e.target !== canvas) return; e.preventDefault(); const c = screenCoords(e); if (c.fx == null) return; send({ rx: c.fx, ry: c.fy }); });
  const pc = new Set(['F1', 'F3', 'F5', 'F11', 'F12', 'Tab', 'Escape', 'AltLeft', 'AltRight', 'ControlLeft', 'ControlRight', 'MetaLeft', 'MetaRight']);
  window.addEventListener('keydown', e => { if (!controlCheck.checked) return; if (pc.has(e.code) || e.ctrlKey || e.altKey || e.metaKey) { e.preventDefault(); e.stopPropagation(); } sendKey(e.code, true); }, { capture: true });
  window.addEventListener('keyup', e => { if (!controlCheck.checked) return; e.preventDefault(); e.stopPropagation(); sendKey(e.code, false); }, { capture: true });
}

// ═══════════════════════════════════════════
// 手机端控件
// ═══════════════════════════════════════════
if (isMobile) {
  function updateMobileResolutions(pw, ph) { mobileResOpts = buildResolutions(pw, ph); if (!currentMW || !mobileResOpts.find(o => o.w === currentMW)) currentMW = mobileResOpts[0].w; mobileUIBuilt = false; updateMobileUI(); }
  function updateMobileUI() { if (mobileUIBuilt) { bar.querySelectorAll('.ctrl-btn').forEach(b => b.classList.remove('active')); bar.querySelector(`[data-q="${currentQ}"]`)?.classList.add('active'); bar.querySelector(`[data-mw="${currentMW}"]`)?.classList.add('active'); const sb = bar.querySelector('.scr-btn'); if (sb) sb.textContent = screenCount > 1 ? `屏${currentScreen}` : '主屏'; return; } mobileUIBuilt = true; bar.querySelectorAll('.ctrl-rows,.ctrl-row,.ctrl-btn,.ctrl-group').forEach(e => e.remove()); let rows = bar.querySelector('.ctrl-rows'); if (!rows) { rows = document.createElement('div'); rows.className = 'ctrl-rows'; bar.appendChild(rows); } const row = () => { const d = document.createElement('div'); d.className = 'ctrl-row'; rows.appendChild(d); return d; }; const addBtn = (r, t, cl, click) => { const b = document.createElement('span'); b.className = 'ctrl-btn ' + cl; b.textContent = t; b.onclick = click; r.appendChild(b); }; const addGroup = (r, btns) => { const g = document.createElement('span'); g.className = 'ctrl-group'; btns.forEach(b => { const e = document.createElement('span'); e.className = 'ctrl-btn'; if (b.active) e.classList.add('active'); e.textContent = b.label; e.onclick = b.click; if (b.data) Object.entries(b.data).forEach(([k, v]) => e.setAttribute('data-' + k, v)); g.appendChild(e); }); r.appendChild(g); }; const r = row(); addBtn(r, screenCount > 1 ? `屏${currentScreen}` : '主屏', 'scr-btn', () => { if (screenCount > 1) { currentScreen = (currentScreen + 1) % screenCount; currentMW = 0; send({ screen: currentScreen, maxw: 0 }); lastResKey = ''; mobileUIBuilt = false; updateMobileUI(); } }); addGroup(r, [{ label: '低', active: currentQ === 40, data: { q: 40 }, click: () => { currentQ = 40; sendSettings(); updateMobileUI(); } }, { label: '中', active: currentQ === 60, data: { q: 60 }, click: () => { currentQ = 60; sendSettings(); updateMobileUI(); } }, { label: '高', active: currentQ === 80, data: { q: 80 }, click: () => { currentQ = 80; sendSettings(); updateMobileUI(); } }]); addGroup(r, mobileResOpts.map(o => ({ label: o.label, active: currentMW === o.w, data: { mw: o.w }, click: () => { currentMW = o.w; sendSettings(); updateMobileUI(); } }))); }
}

// ═══════════════════════════════════════════
// 坐标转换（桌面 & H.264 共用）
// ═══════════════════════════════════════════
function eventPos(e) { const t = (e.touches && e.touches[0]) || (e.changedTouches && e.changedTouches[0]); if (t) return { clientX: t.clientX, clientY: t.clientY, target: e.target }; return { clientX: e.clientX, clientY: e.clientY, target: e.target }; }
function screenCoords(e) { const p = eventPos(e), r = canvas.getBoundingClientRect(), ra = meta.pw / meta.ph, cr = r.width / r.height; let aw, ah, ox = 0, oy = 0; if (ra > cr) { aw = r.width; ah = r.width / ra; oy = (r.height - ah) / 2; } else { ah = r.height; aw = r.height * ra; ox = (r.width - aw) / 2; } const cx = (p.clientX - r.left - ox) / aw, cy = (p.clientY - r.top - oy) / ah; if (cx < 0 || cx > 1 || cy < 0 || cy > 1) return {}; return { fx: Math.round((meta.ox * meta.zoom) + (cx * meta.pw * meta.zoom)), fy: Math.round((meta.oy * meta.zoom) + (cy * meta.ph * meta.zoom)) }; }
