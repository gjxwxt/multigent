// deploy-center 子组件共享类型与工具。
// 契约事实基准：docs/deploy-center-frontend-refactor-spec-v2.md §2（与 HTML 原型冲突时以 §2 为准）。
// 两条硬事实：startedAt/finishedAt 恒空（勿用于展示）；commitSpan 恒两元素（基线/目标），非逐 commit 清单。
import type { ReactNode } from 'react'
import { cn } from '../../../lib/cn'

export type CommitSpanEntry = { sha: string; shortSha: string; title: string; author: string; committedAt: string }

export type DeployHealth = { status?: string; latencyMs?: number; reason?: string; httpStatus?: number }

export type DeployRequest = {
  id: string
  project?: string
  branch: string
  sha: string
  env: string
  status: string
  pipelineId?: number
  commitSpan?: CommitSpanEntry[]
  approval?: { required?: boolean; state?: string }
  vars?: Record<string, string>
  health?: DeployHealth
  createdBy: string
  createdAt: string
  finishedAt?: string
}

export type PipelineJobSummary = { name: string; stage: string; status: string; runnerDescription?: string }

export type PipelineSummary = {
  id: number
  ref: string
  sha: string
  status: string
  webUrl?: string
  jobs?: PipelineJobSummary[]
}

export type DeployAggregate = {
  deployPort: number
  deployHostConfigured: boolean
  lastDeployed: DeployRequest | null
  inflight: DeployRequest | null
  health: DeployHealth | null
  pipelines: PipelineSummary[]
}

export type BranchInfo = { name: string; isDefault: boolean; commitId: string; commitTitle: string }

export type DeployAction = 'approve' | 'reject' | 'cancel' | 'trigger'

export const cardCls = 'rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40'
export const cardHeadCls = 'border-b border-neutral-100 px-5 py-3 dark:border-zinc-800'
export const cardTitleCls = 'text-sm font-semibold text-neutral-800 dark:text-zinc-100'
export const neutralButton =
  'inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-sm font-medium text-neutral-700 shadow-sm hover:bg-neutral-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200 dark:hover:bg-zinc-700'
export const primaryButton =
  'inline-flex items-center gap-2 rounded-lg bg-sky-600 px-4 py-2 text-sm font-medium text-white hover:bg-sky-700 active:bg-sky-800 disabled:cursor-not-allowed disabled:opacity-60'

export function statusText(status: string): string {
  // i18n key：deployStatus.pending_approval 等，全量双语落库（规范 v2 §1.7）。
  const map: Record<string, string> = {
    pending_approval: '待审批',
    approved: '已批准',
    deploying: '部署中',
    success: '成功',
    failed: '失败',
    cancelled: '已取消',
    rejected: '已驳回',
  }
  if (map[status]) return useT(`deployStatus.${status}`, map[status])
  return status
}

// statusText 被普通工具函数与组件共用，无法处处走 useTranslation hook；
// 主组件挂载时 bindDeployT(t) 注入一次，未注入时回退中文 defaultValue。
let tFn: ((k: string, dv: string) => string) | null = null
export function bindDeployT(fn: (k: string, dv: string) => string) {
  tFn = fn
}
function useT(key: string, fallback: string): string {
  return tFn ? tFn(key, fallback) : fallback
}

export function statusCls(status: string): string {
  switch (status) {
    case 'success':
      return 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-300'
    case 'failed':
    case 'rejected':
      return 'bg-red-100 text-red-800 dark:bg-red-950/60 dark:text-red-300'
    case 'deploying':
    case 'approved':
      return 'bg-sky-100 text-sky-800 dark:bg-sky-950/60 dark:text-sky-300'
    case 'pending_approval':
      return 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-300'
    default:
      return 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400'
  }
}

export function pipelineStatusTextCls(status: string): string {
  switch (status) {
    case 'success':
      return 'text-emerald-700 dark:text-emerald-400'
    case 'failed':
      return 'text-red-600 dark:text-red-400'
    case 'running':
      return 'text-sky-700 dark:text-sky-400'
    default:
      return 'text-neutral-500 dark:text-zinc-400'
  }
}

export function shortSha(sha: string): string {
  return sha ? sha.slice(0, 8) : ''
}

export function formatTime(iso?: string): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(undefined, { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  } catch {
    return iso
  }
}

// 相对时间（驾驶舱/版本卡用）：勿用 startedAt/finishedAt（恒空），基准只有 createdAt。
export function formatRelativeTime(iso?: string): string {
  if (!iso) return '—'
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return iso
  const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000))
  if (seconds < 60) return `${seconds}s 前`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m 前`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h 前`
  const days = Math.floor(hours / 24)
  return `${days}d 前`
}

// mm:ss 计时（驾驶舱秒表）。createdAt 是唯一可靠起点。
export function formatElapsed(iso: string, now: number): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return '--:--'
  const total = Math.max(0, Math.floor((now - then) / 1000))
  const mm = String(Math.floor(total / 60)).padStart(2, '0')
  const ss = String(total % 60).padStart(2, '0')
  return `${mm}:${ss}`
}

// 后端 deploySensitiveVar 的前端对齐版：key 大写化后含 TOKEN/SECRET/PASSWORD/KEY 即视为敏感。
// 敏感值在平台侧只存 "***" 掩码（凭据不落盘红线）——掩码回显的唯一真相，禁止切换明文。
export function isSensitiveVarKey(key: string): boolean {
  const k = key.toUpperCase()
  return ['TOKEN', 'SECRET', 'PASSWORD', 'KEY'].some((m) => k.includes(m))
}

// 台账"探针结果"列：失败/取消单没有 health 快照，显示 — 而非伪造 200 OK。
export function DeployHealthCell({ health }: { health?: DeployHealth }): ReactNode {
  if (!health || !health.status || health.status === 'unknown') {
    return <span className="text-neutral-400 dark:text-zinc-500">—</span>
  }
  if (health.status === 'up') {
    return (
      <span className="inline-flex items-center gap-1 font-mono text-[11px] text-emerald-600 dark:text-emerald-400">
        <span className="size-1.5 rounded-full bg-emerald-500" />
        200 OK{health.latencyMs != null ? ` (${health.latencyMs}ms)` : ''}
      </span>
    )
  }
  return (
    <span className="inline-flex items-center gap-1 font-mono text-[11px] text-red-600 dark:text-red-400">
      <span className="size-1.5 rounded-full bg-red-500" />
      {health.httpStatus ? `HTTP ${health.httpStatus}` : '探活失败'}
    </span>
  )
}

export function StatusBadge({ status, className }: { status: string; className?: string }): ReactNode {
  return (
    <span className={cn('inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium', statusCls(status), className)}>
      {statusText(status)}
    </span>
  )
}
