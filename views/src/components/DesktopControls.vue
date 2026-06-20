<template>
  <div id="bar">
    <!-- 用户名：点击可修改 -->
    <n-tooltip trigger="hover">
      <template #trigger>
        <span class="user-tag" @click="onEditUser">{{ store.statsUser }}</span>
      </template>
      点击修改用户名
    </n-tooltip>

    <span class="sep">|</span>

    <!-- 屏幕选择：始终可见 -->
    <n-select
      v-model:value="store.currentScreen"
      :options="screenOptions"
      size="tiny"
      style="width:80px"
      @update:value="onScreenChange"
    />

    <!-- 控制权开关：他人控制时完全隐藏（避免通过浏览器 DevTools 篡改 disabled 属性） -->
    <template v-if="controlSwitchVisible">
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
    </template>
    <!-- 他人控制时仅展示文字提示，无开关 -->
    <template v-else>
      <span class="sep">|</span>
      <span class="label" style="color:#888">{{ controlTip }}</span>
    </template>

    <!-- 画质 / 分辨率 / 编码 / 帧率：仅控制者可见 -->
    <template v-if="isController">
      <!-- WebRTC 模式下隐藏画质/分辨率/FPS，自动拉满 -->
      <template v-if="!isWebRTCSimplified">
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
      </template>

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

      <template v-if="(store.streamFormat === 'h264' && store.statsMaxRate > 0) && !isWebRTCSimplified">
        <span class="sep">|</span>

        <n-select
          v-model:value="store.currentFPS"
          :options="fpsOptions"
          size="tiny"
          style="width:100px"
          @update:value="onFPSChange"
        />
      </template>

      <!-- 自适应偏好：仅 H.264 模式生效 -->
      <template v-if="store.streamFormat === 'h264'">
        <span class="sep">|</span>
        <n-tooltip trigger="hover">
          <template #trigger>
            <span
              class="adapt-mode-btn"
              @click="toggleAdaptMode"
            >当前为 {{ store.adaptMode === 'smooth' ? '⚡流畅' : '🎨画质' }} 模式</span>
          </template>
          单击切换：<br>🎨画质 → 优先降帧率保画质<br>⚡流畅 → 优先降画质保帧率
        </n-tooltip>
      </template>
    </template>

    <!-- 动态数据 -->
    <span class="sep">|</span>
    <span class="stat">{{ store.statsFps }}fps | max {{ store.statsEncMs }}ms | {{ bwText }}</span>
    <!-- 自适应状态指示（仅 H.264 模式） -->
    <span v-if="store.adaptActive && store.streamFormat === 'h264'" class="adapt-badge" :title="`自适应降级: 画质${store.adaptQ} FPS${store.adaptFPS}`">
      ↓{{ store.adaptQ }}q {{ store.adaptFPS }}fps
    </span>
    <span class="sep">|</span>
    <span class="stat">{{ store.statsUsers }} 人在线</span>

    <!-- 连接状态 -->
    <span style="margin-left:auto">
      <span
        class="status-text"
        :style="{ color: statusColor }"
        @click="onStatusClick"
      >{{ statusText }}</span>
    </span>
  </div>

  <!-- 修改用户名弹窗（风格与进入页面的弹窗一致） -->
  <n-modal
    :show="showEditNameDialog"
    :mask-closable="false"
    @update:show="(v: boolean) => { if (!v) showEditNameDialog = false; }"
    transform-origin="center"
  >
    <div class="name-dialog">
      <h3 class="name-dialog-title">修改用户名</h3>
      <p class="name-dialog-body">在远程桌面上显示的名称，其他用户可见</p>
      <n-input
        ref="editNameInputRef"
        v-model:value="editTempName"
        size="large"
        placeholder="输入用户名"
        @keyup.enter="onConfirmEditUser"
      />
      <div class="name-dialog-actions">
        <n-button size="medium" quaternary @click="showEditNameDialog = false">取消</n-button>
        <n-button type="primary" size="medium" @click="onConfirmEditUser">确认</n-button>
      </div>
    </div>
  </n-modal>
</template>

<script setup lang="ts">
import { ref, computed, watch, nextTick } from 'vue';
import { NSelect, NSlider, NSwitch, NTooltip, NModal, NInput, NButton, useNotification } from 'naive-ui';
import { useAppStore } from '@/stores/app';
import { buildResolutions, buildFPSOptions } from '@/composables/useResolutionOptions';
import { useWebSocket } from '@/composables/useWebSocket';

const store = useAppStore();
const { connect } = useWebSocket();
const notification = useNotification();

/** 当前用户是否为控制者（仅控制者可见流配置项） */
const isController = computed(() => store.statsOwner === store.statsUser);

/** H.264 模式下简化 UI：隐藏画质/分辨率/FPS，参数拉满靠自适应 + GCC */
const isWebRTCSimplified = computed(() =>
  store.streamFormat === 'h264' && isController.value,
);

/** 带宽显示：自动选择 KB/s 或 MB/s */
const bwText = computed(() => {
  const kbps = store.statsKb * store.statsFps;
  if (kbps >= 1000) return (kbps / 1024).toFixed(1) + 'MB/s';
  return kbps.toFixed(0) + 'KB/s';
});

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
/** 开关是否可见：无人控制或自己控制时显示，他人控制时隐藏防止 DevTools 篡改 */
const controlSwitchVisible = computed(() =>
  !store.statsOwner || store.statsOwner === store.statsUser,
);
const controlDisabled = computed(() => store.controlStatus === 'pending');
const controlTitle = computed(() => {
  if (store.controlStatus === 'pending') return '等待宿主确认...';
  if (store.statsOwner === store.statsUser) return '你正在控制此电脑';
  return store.statsOwner
    ? `${store.statsOwner} 正在控制此电脑`
    : '打开开关获取控制权';
});
const controlTip = computed(() => {
  if (store.controlStatus === 'pending') return '请求中...';
  if (store.controlStatus === 'denied' || store.controlStatus === 'busy')
    return store.controlMsg || '控制';
  if (store.statsOwner === store.statsUser) return '你正在控制';
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

function toggleAdaptMode() {
  store.adaptMode = store.adaptMode === 'smooth' ? 'quality' : 'smooth';
  store.send({ adapt_mode: store.adaptMode });
}

// H.264 模式下自动拉满参数，让 GCC + 自适应全权接管
watch(() => store.streamFormat, (fmt) => {
  if (fmt === 'h264' && isController.value) {
    const maxFPS = store.statsMaxRate > 0 ? store.statsMaxRate : 60;
    let changed = false;
    if (store.currentQ !== 100) { store.currentQ = 100; changed = true; }
    if (store.currentMW !== 0) { store.currentMW = 0; changed = true; }
    if (store.currentFPS !== maxFPS) { store.currentFPS = maxFPS; changed = true; }
    if (changed) store.sendSettings();
  }
});

const showEditNameDialog = ref(false);
const editTempName = ref('');
const editNameInputRef = ref<InstanceType<typeof NInput> | null>(null);

function onEditUser() {
  editTempName.value = store.statsUser;
  showEditNameDialog.value = true;
  nextTick(() => editNameInputRef.value?.focus());
}

function onConfirmEditUser() {
  const name = editTempName.value.trim();
  if (!name || name === store.statsUser) {
    showEditNameDialog.value = false;
    return;
  }
  store.send({ user: name });
  showEditNameDialog.value = false;
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

.user-tag {
  font-size: 12px;
  color: #f0c040;
  cursor: pointer;
  white-space: nowrap;
  border-bottom: 1px dashed #666;
}
.user-tag:hover {
  color: #f8d860;
  border-bottom-color: #f0c040;
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

.adapt-mode-btn {
  font-size: 12px;
  cursor: pointer;
  white-space: nowrap;
  opacity: 0.8;
  transition: opacity 0.2s;
}
.adapt-mode-btn:hover {
  opacity: 1;
}

.adapt-badge {
  font-size: 11px;
  color: #e67e22;
  background: rgba(230, 126, 34, 0.15);
  border-radius: 3px;
  padding: 1px 5px;
  white-space: nowrap;
  cursor: default;
}
</style>
