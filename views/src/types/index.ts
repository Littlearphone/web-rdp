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
  user?: string;
  clipboard?: string;
  clipboard_image?: string;
  auth?: string;
  /** WebRTC 信令 */
  rtc_webrtc?: boolean;
  rtc_sdp?: string;
  rtc_ice?: string;
  /** 自适应码率：偏好模式 */
  adapt_mode?: string;
  /** 自适应码率：前端实际接收帧率 */
  net_fps?: number;
  /** 自适应码率：前端解码队列深度 */
  net_queue?: number;
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
  users?: number;
  /** 自适应是否正在降级 */
  adapt_active?: boolean;
  /** 自适应目标画质 */
  adapt_q?: number;
  /** 自适应目标帧率 */
  adapt_fps?: number;
}

/** 初始化 / 格式切换消息（用户名 + 编码格式 + 会话实际参数） */
export interface InitMsg {
  user?: string;
  format?: string;
  quality?: number;
  maxw?: number;
  fps?: number;
  challenge?: string;
  clipboard?: string;
  clipboard_image?: string;
  rtc_sdp?: string;
  rtc_ice?: Record<string, unknown>;
  rtc_restart?: boolean;
  h264_sps?: string;
  h264_pps?: string;
}

/** 控制权限状态 */
export type ControlStatus = 'idle' | 'pending' | 'granted' | 'denied' | 'busy';

/** 控制状态消息（服务器推送的权限请求结果） */
export interface ControlStatusMsg {
  control_status?: ControlStatus;
  control_msg?: string;
}
