<template>
  <div id="bar">
    <!-- 屏幕选择：始终可见 -->
    <n-select
      v-model:value="store.currentScreen"
      :options="screenOptions"
      size="tiny"
      style="width:80px"
      @update:value="onScreenChange"
    />

    <!-- 控制权开关：始终可见，紧跟屏幕选项 -->
    <span class="sep">|</span>

    <n-switch
      v-model:value="controlOn"
      :disabled="controlDisabled"
      size="small"
      @update:value="onControlToggle"
    />
    <n-tooltip trigger="hover">
      <template #trigger>
        <span class="label">{{ controlTip }}</span>
      </template>
      {{ controlTitle }}
    </n-tooltip>

    <!-- 画质 / 分辨率 / 编码 / 帧率：仅控制者可见 -->
    <template v-if="isController">
      <span class="sep">|</span>

      <span class="label">画质</span>
      <n-slider
        v-model:value="store.currentQ"
        :min="30"
        :max="100"
        :step="5"
        style="width:60px"
        @update:value="store.sendSettings"
      />

      <span class="sep">|</span>

      <span class="label">分辨率</span>
      <n-select
        v-model:value="store.currentMW"
        :options="resOptions"
        size="tiny"
        style="width:90px"
        @update:value="store.sendSettings"
      />

      <template v-if="store.canH264">
        <span class="sep">|</span>

        <n-switch
          v-model:value="store.useH264"
          size="small"
          @update:value="onH264Toggle"
        />
        <n-tooltip trigger="hover">
          <template #trigger>
            <span class="label">节流模式</span>
          </template>
          开启：GPU 硬件编码，流量低延迟小<br>关闭：兼容模式，纯软件编码
        </n-tooltip>
      </template>

      <template v-if="store.streamFormat === 'h264' && store.statsMaxRate > 0">
        <span class="sep">|</span>

        <n-select
          v-model:value="store.currentFPS"
          :options="fpsOptions"
          size="tiny"
          style="width:100px"
          @update:value="onFPSChange"
        />
      </template>
    </template>

    <!-- 动态数据 -->
    <span class="sep">|</span>
    <span class="stat">{{ store.statsFps }}fps | {{ store.statsEncMs }}ms | {{ (store.statsKb * store.statsFps / 1024).toFixed(1) }}MB/s</span>

    <!-- 连接状态 -->
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
import { NSelect, NSlider, NSwitch, NTooltip, useNotification } from 'naive-ui';
import { useAppStore } from '@/stores/app';
import { buildResolutions, buildFPSOptions } from '@/composables/useResolutionOptions';
import { useWebSocket } from '@/composables/useWebSocket';

const store = useAppStore();
const { connect } = useWebSocket();
const notification = useNotification();

/** 当前用户是否为控制者（仅控制者可见流配置项） */
const isController = computed(() => store.statsOwner === store.statsUser);

const screenOptions = computed(() => {
  const opts = [];
  for (let i = 0; i < store.screenCount; i++) {
    opts.push({
      label: i === 0 ? '主屏' : `副屏${i}`,
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

const resOptions = computed(() => {
  const opts = buildResolutions(store.basePw, store.basePh, false);
  return opts.map(o => ({ label: o.label, value: o.value ?? o.w }));
});

const fpsOptions = computed(() => buildFPSOptions(store.statsMaxRate));

function onFPSChange(v: number) {
  store.currentFPS = v;
  store.sendSettings();
}

// ── 控制权 ──
const controlOn = ref(false);
const controlDisabled = computed(() => {
  // 正在等待审批时禁用开关
  if (store.controlStatus === 'pending') return true;
  // 他人控制时禁用
  if (!store.statsOwner) return false;
  return store.statsOwner !== store.statsUser;
});
const controlTitle = computed(() => {
  if (store.controlStatus === 'pending') return '等待宿主确认...';
  return store.statsOwner
    ? `${store.statsOwner} 正在控制`
    : '打开开关获取控制权';
});
const controlTip = computed(() => {
  if (store.controlStatus === 'pending') return '请求中...';
  if (store.controlStatus === 'denied' || store.controlStatus === 'busy')
    return store.controlMsg || '控制';
  return store.statsOwner
    ? `${store.statsOwner} 正在控制`
    : '控制';
});

// 监听服务器推送的 owner 更新，同步开关状态
watch(() => store.statsOwner, (owner) => {
  controlOn.value = owner === store.statsUser;
  if (owner === store.statsUser) {
    store.controlStatus = 'idle';
    store.controlMsg = '';
  }
});

// 监听控制状态变化，同步开关 + 弹 toast 通知
watch(() => store.controlStatus, (status) => {
  if (status === 'granted') {
    controlOn.value = true;
    notification.success({
      title: '控制权已获取',
      description: '您现在可以控制远程桌面',
      duration: 3000,
    });
  } else if (status === 'denied') {
    controlOn.value = false;
    notification.warning({
      title: '控制请求被拒绝',
      description: store.controlMsg || '宿主拒绝了您的控制请求',
      duration: 5000,
    });
  } else if (status === 'busy') {
    controlOn.value = false;
    notification.info({
      title: '控制权不可用',
      description: store.controlMsg || '其他用户正在控制此电脑',
      duration: 4000,
    });
  } else if (status === 'pending') {
    notification.info({
      title: '请求已发送',
      description: '等待宿主确认控制权限...',
      duration: 2000,
    });
  }
});

function onControlToggle(v: boolean) {
  if (v) {
    // 用户开启控制 → 发送请求，等待服务端审批
    controlOn.value = false; // 先复位，等服务器确认后再打开
    store.controlStatus = 'pending';
    store.controlMsg = '等待宿主确认...';
    store.send({ control: true });
  } else {
    // 用户关闭控制 → 直接释放
    controlOn.value = false;
    store.controlStatus = 'idle';
    store.controlMsg = '';
    store.send({ control: false });
  }
}

function onH264Toggle(v: boolean) {
  store.useH264 = v;
  store.sendSettings();
}

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
  gap: 6px;
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

.stat {
  font-size: 12px;
  color: #4ec9b0;
  white-space: nowrap;
}

.status-text {
  font-size: 12px;
  cursor: pointer;
  white-space: nowrap;
}
</style>
