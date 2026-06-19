<!-- 重连倒计时覆盖层 -->
<template>
  <div v-show="store.showReconnectHint" id="reconnect-hint">
    <span>{{ store.reconnectCountdown }} 秒后自动重连...</span>
    <button @click="manualReconnect">立即重连</button>
  </div>
</template>

<script setup lang="ts">
import { useAppStore } from '@/stores/app';
import { useWebSocket } from '@/composables/useWebSocket';

const store = useAppStore();
const { manualReconnect } = useWebSocket();
</script>

<style scoped>
#reconnect-hint {
  display: flex;
  position: fixed;
  bottom: 20px;
  left: 50%;
  transform: translateX(-50%);
  background: #1a1a1a;
  border: 1px solid #f1c40f;
  border-radius: 6px;
  padding: 10px 20px;
  color: #f1c40f;
  font-size: 13px;
  z-index: 100;
  white-space: nowrap;
  gap: 12px;
  align-items: center;
}

#reconnect-hint button {
  background: #f1c40f;
  color: #000;
  border: none;
  border-radius: 3px;
  padding: 4px 14px;
  cursor: pointer;
  font-weight: 600;
  font-size: 12px;
}
</style>
