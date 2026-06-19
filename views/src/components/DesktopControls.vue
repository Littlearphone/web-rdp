<template>
  <div id="bar">
    <!-- 屏幕选择 -->
    <span class="label">屏幕</span>
    <n-select
      v-model:value="store.currentScreen"
      :options="screenOptions"
      size="tiny"
      style="width:100px"
      @update:value="onScreenChange"
    />

    <span class="sep">|</span>

    <!-- 画质滑块 -->
    <span class="label">画质</span>
    <n-slider
      v-model:value="store.currentQ"
      :min="30"
      :max="100"
      :step="5"
      style="width:70px"
      @update:value="store.sendSettings"
    />
    <span class="label">{{ store.currentQ }}</span>

    <span class="sep">|</span>

    <!-- 帧率选择（ddagrab 模式） -->
    <template v-if="store.statsMaxRate > 0">
      <span class="label">帧率</span>
      <n-select
        v-model:value="store.currentFPS"
        :options="fpsOptions"
        size="tiny"
        style="width:130px"
        @update:value="onFPSChange"
      />
      <span class="sep">|</span>
    </template>

    <!-- 分辨率选择 -->
    <span class="label">分辨率</span>
    <n-select
      v-model:value="store.currentMW"
      :options="resOptions"
      size="tiny"
      style="width:130px"
      @update:value="store.sendSettings"
    />

    <span class="sep">|</span>

    <!-- 控制开关 -->
    <n-switch
      v-model:value="controlOn"
      :disabled="controlDisabled"
      size="small"
      @update:value="onControlToggle"
    />
    <span
      class="label"
      :title="controlTitle"
      style="cursor:pointer"
    >控制</span>

    <span class="sep">|</span>

    <!-- H.264 开关 -->
    <n-switch
      v-model:value="store.useH264"
      :disabled="!store.canH264"
      size="small"
      @update:value="onH264Toggle"
    />
    <span class="label" title="勾选=H.264低流量模式，取消=MJPEG兼容模式">H.264 编码</span>

    <span class="sep">|</span>

    <!-- 统计信息 -->
    <span id="stats">{{ statsText }}</span>

    <!-- 状态文本（可点击重连） -->
    <span style="margin-left:auto">
      <span
        class="status-text"
        :style="{ color: statusColor }"
        @click="onStatusClick"
      >{{ statusText }}</span>
    </span>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue';
import { NSelect, NSlider, NSwitch } from 'naive-ui';
import { useAppStore } from '@/stores/app';
import { buildResolutions, buildFPSOptions } from '@/composables/useResolutionOptions';
import { useWebSocket } from '@/composables/useWebSocket';

const store = useAppStore();
const { connect } = useWebSocket();

// ── 屏幕选项 ──
const screenOptions = computed(() => {
  const opts = [];
  for (let i = 0; i < store.screenCount; i++) {
    opts.push({
      label: i === 0 ? `主屏 (0)` : `副屏 (${i})`,
      value: i,
    });
  }
  return opts;
});

function onScreenChange(v: number) {
  store.currentScreen = v;
  store.currentMW = 0;
  store.lastResKey = '';
  store.send({ screen: v, maxw: 0 });
}

// ── 分辨率选项 ──
const resOptions = computed(() => {
  const opts = buildResolutions(
    store.basePw,
    store.basePh,
    false,
  );
  return opts.map(o => ({ label: o.label, value: o.value ?? o.w }));
});

// ── FPS 选项 ──
const fpsOptions = computed(() => buildFPSOptions(store.statsMaxRate));

function onFPSChange(v: number) {
  store.currentFPS = v;
  store.sendSettings();
}

// ── 控制权 ──
const controlOn = ref(false);
const controlDisabled = computed(() => {
  if (!store.statsOwner) return false;
  return store.statsOwner !== store.statsUser;
});
const controlTitle = computed(() => {
  return store.statsOwner
    ? `控制权: ${store.statsOwner}`
    : '点击获取控制权';
});

watch(() => store.statsOwner, (owner) => {
  const me = store.statsUser;
  controlOn.value = owner === me;
});

function onControlToggle(v: boolean) {
  controlOn.value = v;
  store.send({ control: v });
}

// ── H.264 切换 ──
function onH264Toggle(v: boolean) {
  store.useH264 = v;
  store.sendSettings();
}

// ── 统计文本 ──
const statsText = computed(() => {
  return `${store.statsW}×${store.statsH} Q${store.statsQ} │ ${store.statsFps}fps │ ${store.statsEncMs}ms │ ${store.statsKb}KB/f │ ${(store.statsKb * store.statsFps / 1024).toFixed(1)}MB/s`;
});

// ── 状态文本 ──
const statusText = computed(() => {
  switch (store.connectionStatus) {
    case 'connecting': return '连接中...';
    case 'connected': return '已连接';
    case 'switching': return '切换中...';
    case 'disconnected': return '连接断开';
    case 'failed': return '连接失败';
  }
});

const statusColor = computed(() => {
  switch (store.connectionStatus) {
    case 'connecting':
    case 'switching': return '#f1c40f';
    case 'connected': return '#27ae60';
    default: return '#e74c3c';
  }
});

function onStatusClick() {
  if (!store.ws || store.ws.readyState !== WebSocket.OPEN) {
    connect();
  }
}
</script>

<style scoped>
#bar {
  background: #1a1a1a;
  display: flex;
  align-items: center;
  padding: 4px 10px;
  border-bottom: 1px solid #333;
  font-size: 12px;
  gap: 8px;
  user-select: none;
  min-height: 36px;
  flex-shrink: 0;
}

.label {
  color: #ddd;
  white-space: nowrap;
}

.sep {
  color: #444;
}

#stats {
  font-size: 12px;
  color: #4ec9b0;
  text-align: right;
}

.status-text {
  font-size: 12px;
  cursor: pointer;
  white-space: nowrap;
}
</style>
