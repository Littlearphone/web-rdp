<!-- 统计信息文本（桌面端与移动端复用） -->
<template>
  <span v-if="store.isMobile" class="mobile-stats">
    <span v-if="portrait">{{ store.statsUser }}&emsp;{{ store.statsFps }}fps&emsp;{{ store.statsKb }}KB</span>
    <span v-else>{{ store.statsUser }}<br>{{ store.statsFps }}fps<br>{{ store.statsKb }}KB</span>
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useAppStore } from '@/stores/app';

const store = useAppStore();

const portrait = computed(() => matchMedia('(orientation:portrait)').matches);
</script>

<style scoped>
.mobile-stats {
  font-size: 10px;
  color: #4ec9b0;
  text-align: center;
}
</style>
