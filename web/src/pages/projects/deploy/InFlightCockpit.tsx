// InFlightCockpit：置顶部署中驾驶舱。
// 规范 v2 §1.3：计时基准 = inflight.createdAt（startedAt 恒空）；进度区渲染真实 pipeline jobs
// （lint/test/build/package/deploy 五 stage 八 job），不虚构"四阶段"。
import { useEffect, useState } from 'react'
import { Loader2, ShieldCheck, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '../../../lib/cn'
import {
  formatElapsed,
  pipelineStatusTextCls,
  shortSha,
  statusCls,
  statusText,
  type DeployAction,
  type DeployAggregate,
  type DeployRequest,
} from './deploy-shared'

const inflightStates = ['pending_approval', 'approved', 'deploying']

export function InFlightCockpit({
  inflight,
  agg,
  actionBusy,
  onAction,
}: {
  inflight: DeployRequest | null
  agg: DeployAggregate | null
  actionBusy: string | null
  onAction: (id: string, action: DeployAction) => Promise<void>
}) {
  const { t } = useTranslation()
  const [now, setNow] = useState(() => Date.now())

  // 秒表走针；数据轮询（5s）由页面层负责，这里只管显示。
  useEffect(() => {
    if (!inflight) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [inflight])

  if (!inflight || !inflightStates.includes(inflight.status)) return null

  const pipeline = inflight.pipelineId ? (agg?.pipelines ?? []).find((p) => p.id === inflight.pipelineId) ?? null : null
  const waitingApproval = inflight.status === 'pending_approval'
  // startedAt 恒空（后端只写内存不落库），计时基准只能用 createdAt。
  const base = inflight.createdAt

  return (
    <section className="rounded-lg border border-sky-300 bg-sky-50/50 p-4 dark:border-sky-700 dark:bg-sky-950/30">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2.5">
          <Loader2 className="size-4 animate-spin text-sky-600 dark:text-sky-400" />
          <span className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
            {waitingApproval
              ? t('projectDeploy.cockpitWaiting', { defaultValue: '等待审批' })
              : inflight.status === 'approved'
                ? t('projectDeploy.cockpitApproved', { defaultValue: '已批准 · 待触发' })
                : t('projectDeploy.cockpitRunning', { defaultValue: '正在执行部署流水线' })}
          </span>
          <span className="rounded-full bg-white px-2 py-0.5 font-mono text-[11px] font-semibold text-neutral-700 shadow-sm dark:bg-zinc-800 dark:text-zinc-200">
            {inflight.id}
          </span>
          <span className={cn('rounded-full px-2 py-0.5 text-[11px] font-medium', statusCls(inflight.status))}>
            {statusText(inflight.status)}
          </span>
          <span className="font-mono text-sm font-semibold text-sky-700 tabular-nums dark:text-sky-300">
            {formatElapsed(base, now)}
          </span>
        </div>
        <div className="flex items-center gap-2">
          {waitingApproval && (
            <>
              <button type="button" onClick={() => void onAction(inflight.id, 'approve')} disabled={actionBusy !== null} className="inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-sky-700 disabled:cursor-not-allowed disabled:opacity-60">
                <ShieldCheck className="size-3.5" />
                {t('projectDeploy.approve', { defaultValue: '批准' })}
              </button>
              <button type="button" onClick={() => void onAction(inflight.id, 'reject')} disabled={actionBusy !== null} className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-xs font-medium text-neutral-700 shadow-sm hover:bg-neutral-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200">
                {t('projectDeploy.reject', { defaultValue: '驳回' })}
              </button>
            </>
          )}
          {inflight.status === 'approved' && (
            <button type="button" onClick={() => void onAction(inflight.id, 'trigger')} disabled={actionBusy !== null} className="inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-sky-700 disabled:cursor-not-allowed disabled:opacity-60">
              {t('projectDeploy.trigger', { defaultValue: '触发流水线' })}
            </button>
          )}
          <button type="button" onClick={() => void onAction(inflight.id, 'cancel')} disabled={actionBusy !== null} className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-xs font-medium text-neutral-700 shadow-sm hover:bg-neutral-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200">
            <X className="size-3.5" />
            {t('projectDeploy.cancel', { defaultValue: '取消' })}
          </button>
        </div>
      </div>

      {/* 元数据条 */}
      <div className="mt-3 grid grid-cols-1 gap-2 text-[11px] text-neutral-600 sm:grid-cols-3 dark:text-zinc-400">
        <div>
          <span className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.metaTarget', { defaultValue: '目标' })}</span>
          <span className="ml-1.5 font-mono font-semibold text-neutral-800 dark:text-zinc-200">{inflight.branch}@{shortSha(inflight.sha)}</span>
        </div>
        <div>
          <span className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.colActor', { defaultValue: '发起人' })}</span>
          <span className="ml-1.5 font-semibold text-neutral-800 dark:text-zinc-200">{inflight.createdBy}</span>
        </div>
        <div>
          <span className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.metaApproval', { defaultValue: '审批' })}</span>
          <span className="ml-1.5 font-semibold text-neutral-800 dark:text-zinc-200">
            {inflight.approval?.required ? t('projectDeploy.approvalRequired', { defaultValue: '需审批' }) : t('projectDeploy.approvalFree', { defaultValue: '免审批' })}
          </span>
        </div>
      </div>

      {/* 进度区：真实 pipeline jobs（无阶段级状态源，不虚构四阶段） */}
      <div className="mt-3 rounded-lg border border-sky-100 bg-white/70 p-3 dark:border-sky-950/60 dark:bg-zinc-900/50">
        {!inflight.pipelineId ? (
          <p className="text-[11px] text-neutral-500 dark:text-zinc-400">{t('projectDeploy.pipelineCreating', { defaultValue: '流水线创建中…' })}</p>
        ) : !pipeline ? (
          <p className="text-[11px] text-neutral-500 dark:text-zinc-400">{t('projectDeploy.pipelinePending', { defaultValue: '流水线 #{id} 状态获取中…', id: inflight.pipelineId })}</p>
        ) : (
          <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 sm:grid-cols-4">
            {(pipeline.jobs ?? []).map((j) => (
              <div key={`${j.stage}-${j.name}`} className="flex items-center gap-1.5 text-[11px]">
                {j.status === 'success' ? (
                  <span className="size-1.5 rounded-full bg-emerald-500" />
                ) : j.status === 'running' ? (
                  <Loader2 className="size-3 animate-spin text-sky-500" />
                ) : j.status === 'failed' ? (
                  <span className="size-1.5 rounded-full bg-red-500" />
                ) : (
                  <span className="size-1.5 rounded-full bg-neutral-300 dark:bg-zinc-600" />
                )}
                <span className="truncate font-mono text-neutral-700 dark:text-zinc-300">{j.name}</span>
                <span className={cn('font-mono text-[10px]', pipelineStatusTextCls(j.status))}>{j.status}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </section>
  )
}
