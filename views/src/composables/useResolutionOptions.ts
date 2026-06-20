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

/** 生成 FPS 选项（仅 ddagrab 模式可见）。
 *  上限取屏幕刷新率或 144 的较低值，向下排列多档帧率。 */
export function buildFPSOptions(maxRate: number): { label: string; value: number }[] {
  const upper = Math.min(maxRate, 144);
  const tiers = [upper, 120, 90, 60, 30, 15];
  const seen = new Set<number>();
  const opts: { label: string; value: number }[] = [];
  for (const r of tiers) {
    if (r <= upper && r >= 15 && !seen.has(r)) {
      seen.add(r);
      opts.push({ label: `${r} fps`, value: r });
    }
  }
  return opts;
}
