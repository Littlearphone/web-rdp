/**
 * 分辨率选项构建
 *
 * 桌面端：原始 + [1080, 720, 480] 等比缩放
 * 移动端：适配屏幕 + [720, 480] 等比缩放（去重）
 */

import { useAppStore } from '@/stores/app';
import type { ResolutionOption } from '@/types';

/** 根据基准分辨率生成选项列表 */
export function buildResolutions(pw: number, ph: number, isMobile: boolean): ResolutionOption[] {
  const bp = basePw(pw);
  const bh = basePh(ph);

  if (isMobile) {
    const m = Math.min(bp, Math.round(window.innerWidth * (window.devicePixelRatio || 1)));
    const opts: ResolutionOption[] = [{ label: '适配', w: m }];
    for (const t of [720, 480]) {
      if (t >= bh) continue;
      opts.push({ label: `${t}p`, w: Math.round(bp * t / bh) });
    }
    // 去重
    return opts.filter((o, i) => opts.findIndex(x => x.w === o.w) === i);
  }

  const opts: ResolutionOption[] = [
    { label: '原始', value: 0, w: 0 },
  ];
  for (const t of [1080, 720, 480]) {
    if (t >= bh) continue;
    opts.push({
      label: `${t}p`,
      value: Math.round(bp * t / bh),
      w: Math.round(bp * t / bh),
    });
  }
  return opts;
}

export function basePw(pw: number): number {
  const store = useAppStore();
  return store.origPw || pw;
}

export function basePh(ph: number): number {
  const store = useAppStore();
  return store.origPh || ph;
}

/** 生成 FPS 选项（仅 ddagrab 模式可见） */
export function buildFPSOptions(maxRate: number): { label: string; value: number }[] {
  const opts: { label: string; value: number }[] = [
    { label: '自动帧率', value: 0 },
  ];
  for (const r of [maxRate, 120, 90, 60, 30, 15]) {
    if (r < maxRate && r >= 15 && !opts.find(o => o.value === r)) {
      opts.push({ label: `${r} fps`, value: r });
    }
  }
  return opts;
}
