/** 远程桌面帧元数据 */
export interface Meta {
  ox: number;
  oy: number;
  pw: number;
  ph: number;
  zoom: number;
}

/** 分辨率选项 */
export interface ResolutionOption {
  label: string;
  w: number;
  value?: number;
}

export type StreamFormat = 'jpeg' | 'h264';
export type ConnectionStatus = 'connecting' | 'connected' | 'switching' | 'disconnected' | 'failed';

/** WebSocket 控制消息（发送到后端） */
export interface CtrlMsg {
  control?: boolean;
  screen?: number;
  quality?: number;
  maxw?: number;
  key?: string;
  down?: boolean;
  text?: string;
  rx?: number;
  ry?: number;
  dx1?: number;
  dy1?: number;
  dx2?: number;
  dy2?: number;
  mx?: number;
  my?: number;
  webcodecs?: boolean;
  fps?: number;
}

/** 性能统计消息（后端每秒推送） */
export interface StatsMsg {
  owner: string;
  fps: number;
  enc_ms: number;
  kb: number;
  q: number;
  w: number;
  h: number;
  ox: number;
  oy: number;
  zoom: number;
  screens: number;
  maxrate: number;
}

/** 初始化消息（用户名 + 编码格式） */
export interface InitMsg {
  user?: string;
  format?: string;
}
