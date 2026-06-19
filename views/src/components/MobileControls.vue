<!-- 移动端底栏/侧栏：动态按钮组 -->
<template>
  <div id="bar" class="mobile-bar">
    <StatsDisplay />

    <div class="ctrl-rows">
      <div class="ctrl-row">
        <!-- 屏幕切换按钮 -->
        <span
          class="ctrl-btn scr-btn"
          @click="cycleScreen"
        >{{ store.screenCount > 1 ? `屏${store.currentScreen}` : '主屏' }}</span>

        <!-- 画质按钮组 -->
        <span class="ctrl-group">
          <span
            v-for="q in qualityOptions"
            :key="q.value"
            class="ctrl-btn"
            :class="{ active: store.currentQ === q.value }"
            @click="setQuality(q.value)"
          >{{ q.label }}</span>
        </span>

        <!-- 分辨率按钮组 -->
        <span class="ctrl-group">
          <span
            v-for="r in currentResOpts"
            :key="r.w"
            class="ctrl-btn"
            :class="{ active: store.currentMW === r.w }"
            @click="setResolution(r.w)"
          >{{ r.label }}</span>
        </span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue';
import { useAppStore } from '@/stores/app';
import { buildResolutions } from '@/composables/useResolutionOptions';
import StatsDisplay from './StatsDisplay.vue';
import type { ResolutionOption } from '@/types';

const store = useAppStore();

// ── 画质选项 ──
const qualityOptions = [
  { label: '低', value: 40 },
  { label: '中', value: 60 },
  { label: '高', value: 80 },
];

// ── 分辨率选项（响应式更新） ──
const currentResOpts = ref<ResolutionOption[]>([]);

function updateResOptions() {
  currentResOpts.value = buildResolutions(
    store.basePw,
    store.basePh,
    true,
  );
}

watch([() => store.basePw, () => store.basePh], updateResOptions, { immediate: true });

// ── 操作 ──
function cycleScreen() {
  if (store.screenCount > 1) {
    store.currentScreen = (store.currentScreen + 1) % store.screenCount;
    store.currentMW = 0;
    store.lastResKey = '';
    store.send({ screen: store.currentScreen, maxw: 0 });
  }
}

function setQuality(v: number) {
  store.currentQ = v;
  store.sendSettings();
}

function setResolution(w: number) {
  store.currentMW = w;
  store.sendSettings();
}
</script>

<style scoped>
/* 基础移动端顶栏 */
.mobile-bar {
  background: rgba(26, 26, 26, 0.92);
  backdrop-filter: blur(4px);
  font-size: 11px;
  gap: 12px;
  align-items: center;
  flex-shrink: 0;
  display: flex;
  padding: 5px 10px;
  border-bottom: 1px solid #333;
  user-select: none;
}

/* 控件按钮 */
.ctrl-btn {
  background: #333;
  color: #ccc;
  border: 1px solid #555;
  border-radius: 3px;
  padding: 4px 8px;
  font-size: 12px;
  cursor: pointer;
  white-space: nowrap;
}

.ctrl-btn.active {
  background: #f1c40f;
  color: #000;
  border-color: #f1c40f;
}

.ctrl-group {
  display: flex;
  gap: 3px;
}

/* ── 竖屏布局 ── */
@media (orientation: portrait) {
  .mobile-bar {
    flex-direction: column;
    padding: 5px 10px;
    border-bottom: 1px solid #333;
    gap: 4px;
  }

  .ctrl-rows {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .ctrl-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .ctrl-btn {
    padding: 4px 10px;
    font-size: 12px;
  }

  .ctrl-group {
    display: flex;
    gap: 4px;
  }
}

/* ── 横屏布局 ── */
@media (orientation: landscape) {
  .mobile-bar {
    flex-direction: column;
    padding: 8px 3px;
    width: 54px;
    border-right: 1px solid #333;
    align-items: center;
    border-bottom: none;
  }

  .ctrl-rows {
    flex: 1;
    display: flex;
    flex-direction: column;
    justify-content: space-evenly;
    align-items: center;
    width: 100%;
    padding: 6px 0;
  }

  .ctrl-row {
    height: 100%;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 4px;
    justify-content: space-between;
  }

  .ctrl-group {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .ctrl-btn {
    padding: 5px 3px;
    font-size: 10px;
    text-align: center;
    min-width: 44px;
  }
}
</style>
