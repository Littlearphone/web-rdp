<template>
  <canvas ref="canvasRef" id="screen" />
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch } from 'vue';
import { useAppStore } from '@/stores/app';
import { registerBinaryHandler } from '@/composables/useWebSocket';
import { screenCoords } from '@/composables/useCoordinateMapping';

import { createH264Decoder, type H264Decoder } from '@/decoders/h264';
import { createJpegDecoder, type JpegDecoder } from '@/decoders/jpeg';

const store = useAppStore();
const canvasRef = ref<HTMLCanvasElement | null>(null);

let h264Decoder: ReturnType<typeof createH264Decoder> | null = null;
let jpegDecoder: ReturnType<typeof createJpegDecoder> | null = null;

// ── 桌面端输入状态 ──
let active = false;
let dragStart: { fx: number; fy: number } | null = null;
let dragging = false;
let lastMoveSent = 0;

// ═══════════════════════════════════════════
// 解码器管理
// ═══════════════════════════════════════════

function initDecoders() {
  const canvas = canvasRef.value;
  if (!canvas) return;

  h264Decoder = createH264Decoder(canvas, () => {
    if (store.connectionStatus === 'switching') {
      store.connectionStatus = 'connected';
    }
  });

  jpegDecoder = createJpegDecoder(
    canvas,
    (meta) => {
      store.meta = meta;
      store.updateOrigRes(meta.pw, meta.ph);
    },
    () => {
      if (store.connectionStatus === 'switching') {
        store.connectionStatus = 'connected';
      }
    },
  );
}

function resetDecoders() {
  h264Decoder?.reset();
  jpegDecoder?.reset();
}

function closeDecoders() {
  h264Decoder?.close();
  jpegDecoder?.close();
  h264Decoder = null;
  jpegDecoder = null;
}

/** 二进制帧路由 */
function handleBinary(data: ArrayBuffer, format: 'h264' | 'jpeg') {
  if (format === 'h264' && h264Decoder) {
    h264Decoder.feed(data);
  } else if (format === 'jpeg' && jpegDecoder) {
    jpegDecoder.feed(data);
  }
}

// ═══════════════════════════════════════════
// 桌面端鼠标事件
//
// 流式拖拽策略：
//   mousedown  → 发送 LEFTDOWN (mb/md) + 光标位置 (mx/my)
//                远端窗口收到 WM_LBUTTONDOWN 后捕获鼠标
//   mousemove  → 持续发送光标位置 (mx/my)，限频 30ms
//                远端 SetCursorPos 产生 WM_MOUSEMOVE，因左键已按下，
//                系统将其识别为拖拽，窗口实时跟随
//   mouseup    → 发送 LEFTUP (mb/md)，远端释放鼠标捕获
//                单击时 LEFTDOWN→LEFTUP 间隔即用户自然点击时长，
//                无需额外处理即可被远端识别为完整点击
// ═══════════════════════════════════════════

function onMouseDown(e: MouseEvent) {
  if (!store.statsOwner || e.target !== canvasRef.value) return;
  e.preventDefault();
  if (e.button === 0) {
    active = true;
    const c = screenCoords(e, canvasRef.value!);
    if (!('fx' in c)) return;
    dragStart = c as { fx: number; fy: number };
    dragging = false;
    // 发送 LEFTDOWN + 光标位置 → 远端按下左键，窗口捕获鼠标
    store.send({ mb: 'left', md: true, mx: dragStart.fx, my: dragStart.fy });
  }
}

function onMouseMove(e: MouseEvent) {
  if (!store.statsOwner) return;
  if (!active || !dragStart) {
    // 非拖拽状态：发送光标位置给远端（限频 30ms）
    const n = Date.now();
    if (n - lastMoveSent < 30) return;
    lastMoveSent = n;
    const c = screenCoords(e, canvasRef.value!);
    if ('fx' in c) {
      store.send({ mx: (c as { fx: number; fy: number }).fx, my: (c as { fx: number; fy: number }).fy });
    }
    return;
  }
  // 拖拽状态：检测移动阈值 + 持续发送光标位置
  // 远端左键已按下，SetCursorPos 产生的 WM_MOUSEMOVE 会被识别为拖拽
  const c = screenCoords(e, canvasRef.value!);
  if (!('fx' in c)) return;
  const coords = c as { fx: number; fy: number };
  if (!dragging && (Math.abs(coords.fx - dragStart!.fx) > 3 ||
      Math.abs(coords.fy - dragStart!.fy) > 3)) {
    dragging = true;
  }
  // 拖拽期间持续发送光标位置，实现远端窗口实时跟随
  const n = Date.now();
  if (n - lastMoveSent >= 30) {
    lastMoveSent = n;
    store.send({ mx: coords.fx, my: coords.fy });
  }
}

function onMouseUp(e: MouseEvent) {
  if (!active) return;
  active = false;
  e.preventDefault();
  const c = screenCoords(e, canvasRef.value!);
  if (!('fx' in c)) {
    // 鼠标在显示区域外释放 → 仍需发送 LEFTUP 防止按钮粘滞
    store.send({ mb: 'left', md: false });
    dragging = false;
    dragStart = null;
    return;
  }
  const coords = c as { fx: number; fy: number };
  // 统一使用流式消息完成释放：LEFTUP 释放 mousedown 时按下的左键。
  // 单击和拖拽都走同一路径 — 单击时 LEFTDOWN→LEFTUP 的间隔即用户自然点击时长。
  store.send({ mx: coords.fx, my: coords.fy, mb: 'left', md: false });
  dragging = false;
  dragStart = null;
}

function onContextMenu(e: MouseEvent) {
  if (!store.statsOwner || e.target !== canvasRef.value) return;
  e.preventDefault();
  const c = screenCoords(e, canvasRef.value!);
  if (!('fx' in c)) return;
  const coords = c as { fx: number; fy: number };
  store.send({ rx: coords.fx, ry: coords.fy });
}

/** 粘贴事件：将浏览器剪贴板内容（文本或图像）发送到远程 */
async function onPaste(e: ClipboardEvent) {
  let sent = false;
  // 先检查图像（优先于文本，因为截图粘贴通常包含图像）
  const items = e.clipboardData?.items;
  if (items) {
    for (let i = 0; i < items.length; i++) {
      if (items[i].type.startsWith('image/')) {
        const blob = items[i].getAsFile();
        if (blob) {
          try {
            const buf = await blob.arrayBuffer();
            const b64 = btoa(String.fromCharCode(...new Uint8Array(buf)));
            store.send({ clipboard_image: b64 });
            sent = true;
          } catch (_) { /* 读取失败 */ }
        }
      }
    }
  }
  // 回退：检查纯文本
  if (!sent) {
    const text = e.clipboardData?.getData('text/plain');
    if (text) {
      store.send({ clipboard: text });
      sent = true;
    }
  }

  // ── Ctrl+V 延迟按键：剪贴板内容先于 V 键到达远程 ──
  if (sent && store.pendingClipboardPaste) {
    store.pendingClipboardPaste = false;
    // 150ms 延迟确保剪贴板消息先被远程处理
    setTimeout(() => {
      store.sendKey('KeyV', true);
      setTimeout(() => store.sendKey('KeyV', false), 50);
    }, 150);
  }
}

/** copy 事件：浏览器复制后同步到远程剪贴板 */
function onCopy(e: ClipboardEvent) {
  // 优先使用同步 API（copy 事件中 clipboardData 一定可用）
  const text = e.clipboardData?.getData('text/plain');
  if (text && text !== store.remoteClipboard) {
    store.send({ clipboard: text });
  }
}

function bindDesktopEvents() {
  const canvas = canvasRef.value;
  if (!canvas) return;
  canvas.addEventListener('mousedown', onMouseDown);
  canvas.addEventListener('contextmenu', onContextMenu);
  // mousemove / mouseup 绑定在 window 上，确保拖拽时鼠标移出 canvas
  // 也能正常完成拖拽（释放时发送 dx1/dy1/dx2/dy2）
  window.addEventListener('mousemove', onMouseMove);
  window.addEventListener('mouseup', onMouseUp);
  // 剪贴板事件
  window.addEventListener('paste', onPaste);
  window.addEventListener('copy', onCopy);
}

function unbindDesktopEvents() {
  const canvas = canvasRef.value;
  if (!canvas) return;
  canvas.removeEventListener('mousedown', onMouseDown);
  canvas.removeEventListener('contextmenu', onContextMenu);
  window.removeEventListener('mousemove', onMouseMove);
  window.removeEventListener('mouseup', onMouseUp);
  window.removeEventListener('paste', onPaste);
  window.removeEventListener('copy', onCopy);
}

// ═══════════════════════════════════════════
// 生命周期
// ═══════════════════════════════════════════

onMounted(() => {
  initDecoders();
  registerBinaryHandler(handleBinary);

  if (!store.isMobile) {
    bindDesktopEvents();
  }
});

// 格式变化时重置解码器
watch(() => store.streamFormat, () => {
  resetDecoders();
});

onUnmounted(() => {
  unbindDesktopEvents();
  closeDecoders();
  registerBinaryHandler(() => {});
});
</script>

<style scoped>
#screen {
  width: 100%;
  height: 100%;
  object-fit: contain;
  cursor: crosshair;
  image-rendering: auto;
}
</style>
