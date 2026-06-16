// ── DOM 引用 ──
const img = document.getElementById('screen');
const select = document.getElementById('screen-id');
const statusEl = document.getElementById('status-text');
const controlCheck = document.getElementById('enable-control');
const qualitySlider = document.getElementById('quality');
const qualityVal = document.getElementById('quality-val');
const maxwSelect = document.getElementById('maxw');
const statsEl = document.getElementById('stats');
const reconnectHint = document.getElementById('reconnect-hint');
const reconnectMsg = document.getElementById('reconnect-msg');
const bar = document.getElementById('bar');
const view = document.getElementById('view');

const isMobile = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) && window.innerWidth <= 900;

// ── 状态 ──
let meta = { ox: 0, oy: 0, pw: 1, ph: 1, zoom: 1.0 };
let serverAddr = window.location.host;
let ws = null;
let reconnectTimer = null;
let reconnectDelay = 5;
let reconnectCountdown = 0;
let wasConnected = false;
let lastResKey = '';
let currentQ = 75;
let currentMW = 0;
let currentScreen = 0;
let screenCount = 1;
let mobileResOpts = [];
let mobileUIBuilt = false;

// ── 工具 ──
function send(o) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(o));
}

function sendSettings() {
  send({ quality: currentQ, maxw: currentMW });
}

function sendKey(code, down) {
  send({ key: code, down: down });
}

// ── 分辨率选项 ──
function buildResolutions(pw, ph) {
  if (isMobile) {
    const maxW = Math.min(pw, Math.round(window.innerWidth * (window.devicePixelRatio || 1)));
    const opts = [{ label: '适配', w: maxW }];
    for (const th of [720, 480]) {
      if (th >= ph) continue;
      opts.push({ label: `${th}p`, w: Math.round(pw * th / ph) });
    }
    return opts.filter((o, i) => opts.findIndex(x => x.w === o.w) === i);
  }
  const opts = [{ label: `原始 (${pw}×${ph})`, value: 0 }];
  for (const th of [1080, 720, 480]) {
    if (th >= ph) continue;
    opts.push({
      label: `${Math.round(pw * th / ph)}×${th}`,
      value: Math.round(pw * th / ph)
    });
  }
  return opts;
}

function applyResolutions(pw, ph) {
  const key = `${pw}x${ph}`;
  if (key === lastResKey) return;
  lastResKey = key;

  const opts = buildResolutions(pw, ph);
  const cur = maxwSelect.value;
  maxwSelect.innerHTML = '';
  opts.forEach(o => {
    const el = document.createElement('option');
    el.value = o.value || o.w;
    el.textContent = o.label;
    maxwSelect.appendChild(el);
  });
  maxwSelect.value = opts.find(o => String(o.value || o.w) === cur) ? cur : String(opts[0].value || opts[0].w);
  if (isMobile) sendSettings();
}

// ── WebSocket 连接 ──
function connect() {
  if (ws) { ws.onclose = null; ws.close(); ws = null; }
  clearReconnectTimer();
  reconnectHint.style.display = 'none';
  lastResKey = '';

  statusEl.textContent = '连接中...';
  statusEl.style.color = '#f1c40f';

  ws = new WebSocket(`ws://${serverAddr}/ws`);
  ws.binaryType = 'arraybuffer';

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
    if (wasConnected) startReconnect();
  };

  ws.onerror = () => {
    if (!wasConnected) {
      statusEl.textContent = '连接失败';
      statusEl.style.color = '#e74c3c';
    }
  };
}

function onMessage(event) {
  // 文本消息 → stats / 用户名
  if (typeof event.data === 'string') {
    try {
      const s = JSON.parse(event.data);

      // 用户名
      if (s.user) {
        statsEl.setAttribute('data-user', s.user);
        return;
      }

      // 性能统计
      if (isMobile) {
        const u = statsEl.getAttribute('data-user') || '';
        const portrait = matchMedia('(orientation: portrait)').matches;
        statsEl.innerHTML = portrait
          ? `${u}&emsp;${s.fps}fps&emsp;${s.kb}KB`
          : `${u}<br>${s.fps}fps<br>${s.kb}KB`;
      } else {
        statsEl.innerHTML = `${s.w}×${s.h} Q${s.q} │ ${s.fps}fps │ ${s.enc_ms}ms │ ${s.kb}KB/f │ ${(s.kb * s.fps / 1024).toFixed(1)}MB/s`;
      }

      // 控制权同步
      if (s.owner !== undefined) {
        const me = statsEl.getAttribute('data-user') || '';
        const isMe = s.owner === me;
        controlCheck.disabled = !isMe && s.owner !== '';
        controlCheck.checked = isMe;
        controlCheck.parentElement.title = s.owner ? `控制权: ${s.owner}` : '点击获取控制权';
        img.style.cursor = 'crosshair';
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
    } catch (_) {}
    return;
  }

  // 二进制消息 → 帧数据
  const buf = event.data;
  const dv = new DataView(buf);
  meta = {
    ox: dv.getInt32(0, true),
    oy: dv.getInt32(4, true),
    pw: dv.getInt32(8, true),
    ph: dv.getInt32(12, true),
    zoom: dv.getFloat64(16, true)
  };

  // 更新分辨率选项
  if (isMobile) {
    const k = `${meta.pw}x${meta.ph}`;
    if (k !== lastResKey) {
      lastResKey = k;
      mobileUIBuilt = false;
      updateMobileResolutions(meta.pw, meta.ph);
    }
  } else {
    const key = `${meta.pw}x${meta.ph}`;
    if (key !== lastResKey) {
      lastResKey = key;
      buildDesktopResolutions(meta.pw, meta.ph);
    }
  }

  // 渲染 JPEG
  const jpg = new Uint8Array(buf, 24);
  const blob = new Blob([jpg], { type: 'image/jpeg' });
  const url = URL.createObjectURL(blob);
  const old = img.src;
  img.src = url;
  if (old && old.startsWith('blob:')) URL.revokeObjectURL(old);
}

connect();

// ── 自动重连 ──
function startReconnect() {
  scheduleReconnect();
}

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

function updateReconnectUI() {
  reconnectMsg.textContent = `${reconnectCountdown} 秒后自动重连...`;
}

function manualReconnect() {
  clearReconnectTimer();
  reconnectHint.style.display = 'none';
  connect();
}

function clearReconnectTimer() {
  if (reconnectTimer) { clearInterval(reconnectTimer); reconnectTimer = null; }
}

statusEl.onclick = () => {
  if (!ws || ws.readyState !== WebSocket.OPEN) {
    clearReconnectTimer();
    reconnectHint.style.display = 'none';
    connect();
  }
};

// ── 桌面端控制 ──
if (!isMobile) {

  function updateDesktopScreens() {
    const cur = select.value;
    select.innerHTML = '';
    for (let i = 0; i < screenCount; i++) {
      const el = document.createElement('option');
      el.value = i;
      el.textContent = i === 0 ? '主屏 (0)' : `副屏 (${i})`;
      select.appendChild(el);
    }
    select.value = cur < screenCount ? cur : '0';
  }

  function buildDesktopResolutions(pw, ph) {
    const opts = buildResolutions(pw, ph);
    const cur = maxwSelect.value;
    maxwSelect.innerHTML = '';
    opts.forEach(o => {
      const el = document.createElement('option');
      el.value = o.value;
      el.textContent = o.label;
      maxwSelect.appendChild(el);
    });
    maxwSelect.value = opts.find(o => String(o.value) === cur) ? cur : '0';
  }

  qualitySlider.oninput = () => { qualityVal.textContent = qualitySlider.value; };
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
    send({ screen: currentScreen });
    lastResKey = '';
  };

  // 控制权切换
  controlCheck.onchange = () => {
    send({ control: controlCheck.checked });
    img.style.cursor = 'crosshair';
  };

  // 鼠标点击 / 拖拽
  let active = false, dragStart = null, dragging = false;

  img.onmousedown = e => {
    if (!controlCheck.checked || e.target !== img) return;
    e.preventDefault();
    if (e.button === 0) {
      active = true;
      const c = screenCoords(e);
      if (c.fx == null) return;
      dragStart = c;
      dragging = false;
    }
  };

  img.onmousemove = e => {
    if (!active || !dragStart) return;
    const c = screenCoords(e);
    if (c.fx == null) return;
    if (Math.abs(c.fx - dragStart.fx) > 3 || Math.abs(c.fy - dragStart.fy) > 3) {
      dragging = true;
    }
  };

  img.onmouseup = e => {
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
      fetch(`http://${serverAddr}/click?x=${c.fx}&y=${c.fy}`).catch(() => {});
    }
    dragging = false;
    dragStart = null;
  };

  // 右键
  view.addEventListener('contextmenu', e => {
    if (!controlCheck.checked || e.target !== img) return;
    e.preventDefault();
    const c = screenCoords(e);
    if (c.fx == null) return;
    send({ rx: c.fx, ry: c.fy });
  });

  // 键盘
  const preventCodes = new Set([
    'F1', 'F3', 'F5', 'F11', 'F12', 'Tab', 'Escape',
    'AltLeft', 'AltRight', 'ControlLeft', 'ControlRight',
    'MetaLeft', 'MetaRight'
  ]);

  window.addEventListener('keydown', e => {
    if (!controlCheck.checked) return;
    if (preventCodes.has(e.code) || e.ctrlKey || e.altKey || e.metaKey) {
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

// ── 手机端控件 ──
if (isMobile) {

  function updateMobileResolutions(pw, ph) {
    mobileResOpts = buildResolutions(pw, ph);
    if (!currentMW || !mobileResOpts.find(o => o.w === currentMW)) {
      currentMW = mobileResOpts[0].w;
    }
    mobileUIBuilt = false;
    updateMobileUI();
  }

  function updateMobileUI() {
    // 轻量更新：只切换 active class
    if (mobileUIBuilt) {
      bar.querySelectorAll('.ctrl-btn').forEach(b => b.classList.remove('active'));
      bar.querySelector(`[data-q="${currentQ}"]`)?.classList.add('active');
      bar.querySelector(`[data-mw="${currentMW}"]`)?.classList.add('active');
      const sb = bar.querySelector('.scr-btn');
      if (sb) sb.textContent = screenCount > 1 ? `屏${currentScreen}` : '主屏';
      return;
    }

    // 首次构建 DOM
    mobileUIBuilt = true;
    bar.querySelectorAll('.ctrl-rows,.ctrl-row,.ctrl-btn,.ctrl-group').forEach(e => e.remove());

    let rows = bar.querySelector('.ctrl-rows');
    if (!rows) {
      rows = document.createElement('div');
      rows.className = 'ctrl-rows';
      bar.appendChild(rows);
    }

    const row = () => {
      const d = document.createElement('div');
      d.className = 'ctrl-row';
      rows.appendChild(d);
      return d;
    };

    const addBtn = (r, text, cls, click) => {
      const b = document.createElement('span');
      b.className = 'ctrl-btn ' + cls;
      b.textContent = text;
      b.onclick = click;
      r.appendChild(b);
    };

    const addGroup = (r, btns) => {
      const g = document.createElement('span');
      g.className = 'ctrl-group';
      btns.forEach(b => {
        const el = document.createElement('span');
        el.className = 'ctrl-btn';
        if (b.active) el.classList.add('active');
        el.textContent = b.label;
        el.onclick = b.click;
        if (b.data) {
          Object.entries(b.data).forEach(([k, v]) => el.setAttribute('data-' + k, v));
        }
        g.appendChild(el);
      });
      r.appendChild(g);
    };

    const r = row();

    // 屏幕切换
    addBtn(r, screenCount > 1 ? `屏${currentScreen}` : '主屏', 'scr-btn', () => {
      if (screenCount > 1) {
        currentScreen = (currentScreen + 1) % screenCount;
        send({ screen: currentScreen });
        mobileUIBuilt = false;
        updateMobileUI();
      }
    });

    // 画质
    addGroup(r, [
      { label: '低', active: currentQ === 40, data: { q: 40 }, click: () => { currentQ = 40; sendSettings(); updateMobileUI(); } },
      { label: '中', active: currentQ === 60, data: { q: 60 }, click: () => { currentQ = 60; sendSettings(); updateMobileUI(); } },
      { label: '高', active: currentQ === 80, data: { q: 80 }, click: () => { currentQ = 80; sendSettings(); updateMobileUI(); } },
    ]);

    // 分辨率
    addGroup(r, mobileResOpts.map(o => ({
      label: o.label,
      active: currentMW === o.w,
      data: { mw: o.w },
      click: () => { currentMW = o.w; sendSettings(); updateMobileUI(); }
    })));
  }
}

// ── 坐标转换 ──
function eventPos(e) {
  const t = (e.touches && e.touches[0]) || (e.changedTouches && e.changedTouches[0]);
  if (t) return { clientX: t.clientX, clientY: t.clientY, target: e.target };
  return { clientX: e.clientX, clientY: e.clientY, target: e.target };
}

function screenCoords(e) {
  const p = eventPos(e);
  const rect = img.getBoundingClientRect();
  const ratio = meta.pw / meta.ph;
  const cRatio = rect.width / rect.height;

  let aw, ah, ox = 0, oy = 0;
  if (ratio > cRatio) {
    aw = rect.width;
    ah = rect.width / ratio;
    oy = (rect.height - ah) / 2;
  } else {
    ah = rect.height;
    aw = rect.height * ratio;
    ox = (rect.width - aw) / 2;
  }

  const cx = (p.clientX - rect.left - ox) / aw;
  const cy = (p.clientY - rect.top - oy) / ah;
  if (cx < 0 || cx > 1 || cy < 0 || cy > 1) return {};

  return {
    fx: Math.round((meta.ox * meta.zoom) + (cx * meta.pw * meta.zoom)),
    fy: Math.round((meta.oy * meta.zoom) + (cy * meta.ph * meta.zoom))
  };
}
