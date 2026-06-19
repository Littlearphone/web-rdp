<template>
  <n-config-provider :theme="darkTheme" :locale="zhCN" style="height: 100%">
    <n-notification-provider>
    <div id="main">
      <!-- 桌面端顶栏 -->
      <DesktopControls v-if="!store.isMobile" />

      <!-- 移动端控件（竖屏底部 / 横屏侧边） -->
      <template v-if="store.isMobile">
        <div id="view">
          <ScreenCanvas />
        </div>
        <MobileControls />
      </template>

      <!-- 桌面端布局：控件在上，canvas 在下 -->
      <template v-else>
        <div id="view">
          <ScreenCanvas />
        </div>
      </template>
    </div>

    <!-- 重连覆盖层（固定定位） -->
    <ConnectionOverlay />
    </n-notification-provider>
  </n-config-provider>
</template>

<script setup lang="ts">
import { onMounted } from 'vue';
import { darkTheme, zhCN, NConfigProvider, NNotificationProvider } from 'naive-ui';
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

onMounted(() => {
  init();
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
</style>
