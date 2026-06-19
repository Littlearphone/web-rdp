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
  }
}

function onMouseMove(e: MouseEvent) {
  if (!store.statsOwner) return;
  if (!active || !dragStart) {
    const n = Date.now();
    if (n - lastMoveSent < 30) return;
    lastMoveSent = n;
    const c = screenCoords(e, canvasRef.value!);
    if ('fx' in c) {
      store.send({ mx: (c as { fx: number; fy: number }).fx, my: (c as { fx: number; fy: number }).fy });
    }
    return;
  }
  const c = screenCoords(e, canvasRef.value!);
  if (!('fx' in c)) return;
  if (Math.abs((c as { fx: number; fy: number }).fx - dragStart!.fx) > 3 ||
      Math.abs((c as { fx: number; fy: number }).fy - dragStart!.fy) > 3) {
    dragging = true;
  }
}

function onMouseUp(e: MouseEvent) {
  if (!active) return;
  active = false;
  e.preventDefault();
  const c = screenCoords(e, canvasRef.value!);
  if (!('fx' in c)) return;
  const coords = c as { fx: number; fy: number };
  if (dragging) {
    store.send({
      dx1: dragStart!.fx, dy1: dragStart!.fy,
      dx2: coords.fx, dy2: coords.fy,
    });
  } else {
    // 单击走 HTTP 端点降低延迟
    fetch(`http://${store.serverAddr}/click?x=${coords.fx}&y=${coords.fy}`).catch(() => {});
  }
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

function bindDesktopEvents() {
  const canvas = canvasRef.value;
  if (!canvas) return;
  canvas.addEventListener('mousedown', onMouseDown);
  canvas.addEventListener('mousemove', onMouseMove);
  canvas.addEventListener('mouseup', onMouseUp);
  canvas.addEventListener('contextmenu', onContextMenu);
}

function unbindDesktopEvents() {
  const canvas = canvasRef.value;
  if (!canvas) return;
  canvas.removeEventListener('mousedown', onMouseDown);
  canvas.removeEventListener('mousemove', onMouseMove);
  canvas.removeEventListener('mouseup', onMouseUp);
  canvas.removeEventListener('contextmenu', onContextMenu);
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
  max-width: 100%;
  max-height: 100%;
  object-fit: contain;
  cursor: crosshair;
  image-rendering: auto;
}
</style>
