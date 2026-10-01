// Observability center — static demo data.
// 讲故事用死数据页：数据全部硬编码，等日志中心/监控栈就位后只换数据层。
// 口径与 docs/observability-alerting-proposal.md 一致（97/95/99 为接口成功率 SLO 档位）。

export type SliTone = 'good' | 'warn' | 'bad'

export type SloCard = {
  api: string
  target: number
  current: number
  window: string
}

export type ServiceRow = {
  name: string
  url: string
  online: boolean
  latencyMs: number
  lastProbe: string
  uptime30d: number
}

export type FrontendErrorRow = {
  message: string
  count: number
  lastSeen: string
  release: string
  trend: number[]
}

export type AlertEvent = {
  id: string
  at: string
  severity: 'P1' | 'P2' | 'P3'
  service: string
  summary: string
  status: 'firing' | 'acknowledged' | 'resolved'
  claimedBy?: string
}

export const sloCards: SloCard[] = [
  { api: '核心接口（登录/工作流/任务）', target: 99.9, current: 99.96, window: '近 24h' },
  { api: 'Web 控制台接口', target: 99, current: 99.4, window: '近 24h' },
  { api: 'IM 通知回调接口', target: 99, current: 98.2, window: '近 24h' },
]

export function sliTone(current: number, target: number): SliTone {
  if (current >= target) return 'good'
  if (current >= target - 1) return 'warn'
  return 'bad'
}

export const serviceRows: ServiceRow[] = [
  { name: 'multigent-console', url: 'http://192.168.139.231:27892', online: true, latencyMs: 38, lastProbe: '10s 前', uptime30d: 99.98 },
  { name: 'mattermost', url: 'http://192.168.139.231:8065', online: true, latencyMs: 51, lastProbe: '10s 前', uptime30d: 99.71 },
  { name: 'gitlab-ce（宿主）', url: 'http://192.168.139.231:8083', online: true, latencyMs: 112, lastProbe: '10s 前', uptime30d: 99.90 },
  { name: 'app-preview（batch-test1）', url: 'http://192.168.139.231:28012', online: false, latencyMs: 0, lastProbe: '2min 前', uptime30d: 97.42 },
]

export const frontendErrorRows: FrontendErrorRow[] = [
  {
    message: 'TypeError: Cannot read properties of undefined (reading "map")',
    count: 42,
    lastSeen: '5min 前',
    release: 'console@06233336',
    trend: [3, 5, 4, 9, 7, 6, 8],
  },
  {
    message: 'WebSocket closed unexpectedly (code 1006)',
    count: 18,
    lastSeen: '12min 前',
    release: 'console@06233336',
    trend: [1, 0, 2, 3, 2, 6, 4],
  },
  {
    message: 'ChunkLoadError: Loading chunk 7 failed (timeout)',
    count: 6,
    lastSeen: '1h 前',
    release: 'console@05187741',
    trend: [0, 1, 0, 0, 2, 1, 2],
  },
]

export const alertEvents: AlertEvent[] = [
  {
    id: 'alt-0930-011',
    at: '09-30 23:41',
    severity: 'P2',
    service: 'multigent-console',
    summary: 'dockerd liveness probe failed — engine SIGKILLed and restarted (recovered in 34s)',
    status: 'resolved',
  },
  {
    id: 'alt-0930-010',
    at: '09-30 15:47',
    severity: 'P2',
    service: 'agent-sandbox',
    summary: 'sandbox spawn failures spike: 117 wakeup tasks failed while docker daemon hung',
    status: 'resolved',
    claimedBy: 'agent:oncall-dev',
  },
  {
    id: 'alt-0930-009',
    at: '09-30 15:30',
    severity: 'P1',
    service: 'dockerd',
    summary: 'docker daemon unresponsive: /_ping timeout > 10s (futex hang signature)',
    status: 'resolved',
    claimedBy: 'agent:oncall-dev',
  },
  {
    id: 'alt-0930-008',
    at: '09-30 12:02',
    severity: 'P3',
    service: 'mattermost',
    summary: 'health endpoint degraded (health: starting) for 51s after host restart',
    status: 'resolved',
  },
  {
    id: '0930-firing-demo',
    at: '09-30 23:58',
    severity: 'P3',
    service: 'app-preview（batch-test1）',
    summary: 'probe down: connection refused on :28012 for 2m (demo row)',
    status: 'firing',
  },
]

// 24h error-rate sparkline per key API (percentage). Pure display data.
export const apiErrorTrend: Record<string, number[]> = {
  '核心接口': [0.02, 0.03, 0.04, 0.02, 0.05, 0.03, 0.04, 0.06, 0.04, 0.03, 0.05, 0.04],
  'Web 控制台': [0.4, 0.5, 0.6, 0.4, 0.8, 0.6, 0.5, 0.7, 0.6, 0.5, 0.6, 0.6],
  'IM 回调': [0.6, 0.8, 1.1, 0.9, 1.8, 1.2, 1.0, 0.9, 1.1, 1.0, 1.2, 1.8],
}
