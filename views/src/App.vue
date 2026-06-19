<template>
  <n-config-provider :theme="darkTheme" :locale="zhCN" style="height: 100%">
    <n-notification-provider>
    <div id="main">
      <!-- 桌面端顶栏 -->
      <DesktopControls v-if="!store.isMobile && !showNameDialog" />

      <!-- 移动端控件（竖屏底部 / 横屏侧边） -->
      <template v-if="store.isMobile && !showNameDialog">
        <div id="view">
          <ScreenCanvas />
        </div>
        <MobileControls />
      </template>

      <!-- 桌面端布局：控件在上，canvas 在下 -->
      <template v-else-if="!showNameDialog">
        <div id="view">
          <ScreenCanvas />
        </div>
      </template>
    </div>

    <!-- 重连覆盖层（固定定位） -->
    <ConnectionOverlay v-if="!showNameDialog" />

    <!-- 用户名设置弹窗（连接前必填）—— 风格与 Win32 深色弹窗一致 -->
    <n-modal
      :show="showNameDialog"
      :mask-closable="false"
      :closable="false"
      transform-origin="center"
    >
      <div class="name-dialog">
        <h3 class="name-dialog-title">设置用户名</h3>
        <p class="name-dialog-body">在远程桌面上显示的名称，其他用户可见</p>
        <n-input
          ref="nameInputRef"
          v-model:value="tempName"
          size="large"
          placeholder="输入用户名"
          @keyup.enter="onConfirmName"
        />
        <p class="name-dialog-body" style="margin-top:16px; margin-bottom:4px;">
          访问密码（可选，留空则请求宿主审批）
        </p>
        <n-input
          v-model:value="tempPassword"
          size="large"
          placeholder="输入密码（留空=匿名访问）"
          @keyup.enter="onConfirmName"
        />
        <div class="name-dialog-actions">
          <n-button type="primary" size="medium" @click="onConfirmName">
            进入
          </n-button>
        </div>
      </div>
    </n-modal>
    </n-notification-provider>
  </n-config-provider>
</template>

<script setup lang="ts">
import { ref, onMounted, nextTick, watch } from 'vue';
import { darkTheme, zhCN, NConfigProvider, NNotificationProvider, NModal, NInput, NButton } from 'naive-ui';
import { useAppStore } from '@/stores/app';
import { useWebSocket } from '@/composables/useWebSocket';
import { useKeyboardCapture } from '@/composables/useKeyboardCapture';
import DesktopControls from '@/components/DesktopControls.vue';
import MobileControls from '@/components/MobileControls.vue';
import ScreenCanvas from '@/components/ScreenCanvas.vue';
import ConnectionOverlay from '@/components/ConnectionOverlay.vue';

const store = useAppStore();
const { init } = useWebSocket();

// 桌面端键盘拦截
if (!store.isMobile) {
  useKeyboardCapture();
}

// ── 用户名弹窗 ──
const showNameDialog = ref(true);
const tempName = ref('用户' + Math.floor(1000 + Math.random() * 9000));
const tempPassword = ref('');
const nameInputRef = ref<InstanceType<typeof NInput> | null>(null);

function onConfirmName() {
  const name = tempName.value.trim();
  if (!name) return;
  showNameDialog.value = false;
  const pwd = tempPassword.value.trim();
  init(name, pwd || undefined);
}

onMounted(async () => {
  await nextTick();
  nameInputRef.value?.focus();
});

// 连接断开后回到初始弹窗，让用户可修改名字/密码后重新进入
watch(() => store.connectionStatus, (status) => {
  if ((status === 'disconnected' || status === 'failed') && store.wasConnected) {
    store.wasConnected = false;
    store.clearReconnectTimer();
    store.showReconnectHint = false;
    showNameDialog.value = true;
  }
});
</script>

<style>
/* ═══════════════════════════════════════════
   全局重置 & 基础样式
   ═══════════════════════════════════════════ */
* {
  margin: 0;
  padding: 0;
  box-sizing: border-box;
}

body {
  width: 100vw;
  height: 100vh;
  background: #000;
  color: #ddd;
  font-family: sans-serif;
  overflow: hidden;
}

#app {
  height: 100%;
}

#main {
  display: flex;
  flex-direction: column;
  height: 100%;
}

#view {
  flex: 1;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
}

.name-dialog {
  background: #202020;
  padding: 28px 36px 24px;
  border-radius: 4px;
  width: 420px;
  max-width: 90vw;
  font-family: "Microsoft YaHei UI", "Microsoft YaHei", sans-serif;
}

.name-dialog-title {
  margin: 0 0 4px 0;
  color: #e0e0e0;
  font-size: 22px;
  font-weight: 700;
}

.name-dialog-body {
  margin: 0 0 20px 0;
  color: #999;
  font-size: 16px;
  font-weight: 400;
}

.name-dialog-actions {
  margin-top: 20px;
  text-align: right;
}
</style>
