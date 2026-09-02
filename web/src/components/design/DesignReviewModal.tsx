import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ExternalLink, Loader2, RefreshCw, RotateCcw, X } from 'lucide-react'
import { apiFetch } from '../../lib/api'
import { cn } from '../../lib/cn'
import { overlayDismissProps } from '../ui/overlay'
import { DESIGN_EMBED_MODE } from './mode'

// OD's real run status vocabulary (od_client LatestRunStatus): pending /
// running / succeeded / failed / cancelled; "none" means no run yet. "done"
// and "error" are kept as legacy aliases.
export type DesignRunStatus = 'none' | 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled' | string

/**
 * Stage-two modal of the design gate (docs §5.2/§8.2): the OD Studio iframe
 * (Plan A) or a new-tab guide card (Plan B), with a real state machine around
 * it — staged skeleton loading while the OD run warms up, a continue/rework
 * pair once the run is ready, and full session recovery so closing the modal
 * or refreshing the page never strands the flow (the OD project persists
 * server-side; design/status re-mints signed URLs on every open).
 *
 * Props: proxyUrl/launchUrl come from the start response; the modal polls
 * design/status itself and mints fresh URLs when they are missing.
 */
export function DesignReviewModal({
  project,
  taskID,
  taskTitle,
  proxyUrl,
  launchUrl,
  onConfirm,
  onRework,
  onClose,
}: {
  project: string
  taskID: string
  taskTitle?: string
  proxyUrl: string
  launchUrl: string
  onConfirm: () => void
  onRework: () => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [runStatus, setRunStatus] = useState<DesignRunStatus>('generating')
  const [frameReady, setFrameReady] = useState(false)
  const [frameNonce, setFrameNonce] = useState(0)
  const [frameErr, setFrameErr] = useState(false)
  // Status endpoint is the session source of truth: it re-mints signed
  // proxy/launch URLs (4h TTL tokens) every poll, so a refresh or reopen
  // recovers the session without another start call.
  const [liveUrls, setLiveUrls] = useState<{ proxyUrl?: string; launchUrl?: string }>({})
  // Staged loading copy rotates while the OD run warms up; the skeleton is
  // real state (frameReady), not a timed fake.
  const [stageIdx, setStageIdx] = useState(0)
  const stageTimer = useRef<number | null>(null)

  const effectiveProxy = proxyUrl || liveUrls.proxyUrl || ''
  const effectiveLaunch = launchUrl || liveUrls.launchUrl || ''
  const ready = runStatus === 'succeeded' || runStatus === 'done'
  const failed = runStatus === 'failed' || runStatus === 'error'

  const loadingStages = [
    t('designGate.loading.project', { defaultValue: '正在创建设计项目…' }),
    t('designGate.loading.assistant', { defaultValue: '正在启动设计助手…' }),
    t('designGate.loading.canvas', { defaultValue: '正在加载设计画布…' }),
  ]

  // Poll design/status: drives the run badge, recovers signed URLs after a
  // reopen/refresh, and flags upstream failures. 10s cadence; failures leave
  // the last state (best-effort, silent statuses).
  useEffect(() => {
    let alive = true
    const poll = () => {
      apiFetch<{ projectId?: string; runStatus?: DesignRunStatus; proxyUrl?: string; launchUrl?: string }>(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/status`,
        { silentStatuses: [401, 403, 404, 500, 502, 503, 504] },
      )
        .then((data) => {
          if (!alive || !data) return
          if (data.runStatus) setRunStatus(data.runStatus)
          if (data.proxyUrl || data.launchUrl) setLiveUrls({ proxyUrl: data.proxyUrl, launchUrl: data.launchUrl })
        })
        .catch(() => { /* best-effort */ })
    }
    poll()
    const timer = window.setInterval(poll, 10000)
    return () => {
      alive = false
      window.clearInterval(timer)
    }
  }, [project, taskID])

  // Rotate the staged copy every 3s while the iframe has not painted.
  useEffect(() => {
    if (frameReady || ready) return
    stageTimer.current = window.setInterval(() => {
      setStageIdx((i) => Math.min(i + 1, loadingStages.length - 1))
    }, 3000)
    return () => {
      if (stageTimer.current) window.clearInterval(stageTimer.current)
    }
  }, [frameReady, ready, loadingStages.length])

  function reloadFrame() {
    setFrameErr(false)
    setFrameReady(false)
    setStageIdx(0)
    setFrameNonce((n) => n + 1)
  }

  const statusBadge = (() => {
    if (ready) {
      return { cls: 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-950/40 dark:text-emerald-300 dark:border-emerald-800', label: t('designGate.status.done', { defaultValue: '设计已就绪' }), pulse: false }
    }
    if (failed) {
      return { cls: 'bg-red-50 text-red-700 border-red-200 dark:bg-red-950/40 dark:text-red-300 dark:border-red-800', label: t('designGate.status.error', { defaultValue: '生成失败' }), pulse: false }
    }
    if (runStatus === 'none') {
      return { cls: 'bg-neutral-100 text-neutral-600 border-neutral-200 dark:bg-zinc-800 dark:text-zinc-400 dark:border-zinc-700', label: t('designGate.status.idle', { defaultValue: '未开始' }), pulse: false }
    }
    return { cls: 'bg-sky-50 text-sky-700 border-sky-200 dark:bg-sky-950/40 dark:text-sky-300 dark:border-sky-800', label: t('designGate.status.generating', { defaultValue: '设计中…' }), pulse: true }
  })()

  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/45 p-4 backdrop-blur-[1px]" role="presentation" {...overlayDismissProps(onClose)}>
      <div
        className="flex h-[min(85vh,860px)] w-[min(1080px,92vw)] animate-scale-in flex-col overflow-hidden rounded-xl border border-neutral-200 bg-white shadow-2xl dark:border-zinc-700 dark:bg-zinc-900"
        role="dialog"
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Top bar: title, run badge, external-tab shortcut, close */}
        <div className="flex shrink-0 items-center justify-between gap-3 border-b border-neutral-100 px-5 py-3 dark:border-zinc-800">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
              {t('designGate.title', { defaultValue: '设计确认' })}
              {taskTitle ? <span className="ml-2 truncate font-normal text-xs text-neutral-500 dark:text-zinc-400">{taskTitle}</span> : null}
            </h2>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <span className={cn('inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-[11px] font-medium', statusBadge.cls)}>
              {statusBadge.pulse && <span className="size-1.5 animate-pulse rounded-full bg-sky-500" />}
              {statusBadge.label}
            </span>
            {effectiveLaunch && (
              <button
                type="button"
                onClick={() => window.open(effectiveLaunch, '_blank', 'noopener')}
                title={t('designGate.openExternal', { defaultValue: '在新标签页中打开 OpenDesign' })}
                className="rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-sky-600 dark:hover:bg-zinc-800 dark:hover:text-sky-400"
              >
                <ExternalLink className="size-4" />
              </button>
            )}
            <button
              type="button"
              onClick={onClose}
              className="rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
              aria-label="close"
            >
              <X className="size-4" />
            </button>
          </div>
        </div>

        {/* Middle: iframe (Plan A) or external-tab guide card (Plan B) */}
        {DESIGN_EMBED_MODE === 'iframe' ? (
          <div className="relative min-h-0 flex-1 bg-neutral-100 dark:bg-zinc-950">
            {/* Skeleton overlay: visible until the iframe paints. Real state,
                staged copy, spinner + shimmer. */}
            {!frameReady && (
              <div className="absolute inset-0 z-10 flex flex-col items-center justify-center gap-4 bg-neutral-100 dark:bg-zinc-950">
                <div className="w-full max-w-md space-y-3 px-8">
                  <div className="h-24 animate-pulse rounded-xl bg-neutral-200/80 dark:bg-zinc-800/80" />
                  <div className="grid grid-cols-3 gap-3">
                    <div className="h-16 animate-pulse rounded-lg bg-neutral-200/70 dark:bg-zinc-800/70" />
                    <div className="h-16 animate-pulse rounded-lg bg-neutral-200/60 dark:bg-zinc-800/60" />
                    <div className="h-16 animate-pulse rounded-lg bg-neutral-200/50 dark:bg-zinc-800/50" />
                  </div>
                  <div className="h-2 w-3/4 animate-pulse rounded-full bg-neutral-200/70 dark:bg-zinc-800/70" />
                </div>
                <p className="flex items-center gap-2 text-sm text-neutral-500 dark:text-zinc-400">
                  <Loader2 className="size-4 animate-spin text-sky-500" />
                  {failed
                    ? t('designGate.loading.failed', { defaultValue: '生成失败，可点击下方「重新设计」重试。' })
                    : ready
                      ? t('designGate.loading.canvas', { defaultValue: '正在加载设计画布…' })
                      : loadingStages[stageIdx]}
                </p>
                {frameErr && (
                  <div className="flex items-center gap-2">
                    <p className="text-xs text-red-600 dark:text-red-400">{t('designGate.frameError', { defaultValue: '画布加载失败' })}</p>
                    <button
                      type="button"
                      onClick={reloadFrame}
                      className="text-xs font-medium text-sky-600 hover:underline dark:text-sky-400"
                    >
                      {t('designGate.retry', { defaultValue: '重试' })}
                    </button>
                  </div>
                )}
                {effectiveLaunch && (
                  <p className="text-xs text-neutral-400 dark:text-zinc-500">
                    {t('designGate.loading.externalHint', { defaultValue: '等待太久？点击右上角 ↗ 在新标签页打开' })}
                  </p>
                )}
              </div>
            )}
            {effectiveProxy ? (
              <iframe
                key={frameNonce}
                src={effectiveProxy}
                title={t('designGate.title', { defaultValue: '设计确认' })}
                className={cn('size-full border-0 transition-opacity duration-300', frameReady ? 'opacity-100' : 'opacity-0')}
                allow="clipboard-write"
                onLoad={() => setFrameReady(true)}
                onError={() => { setFrameErr(true); setFrameReady(true) }}
              />
            ) : (
              <div className="flex size-full items-center justify-center">
                <p className="text-sm text-red-600 dark:text-red-400">
                  {t('designGate.sessionMissing', { defaultValue: '设计会话缺失，请关闭后重新打开。' })}
                </p>
              </div>
            )}
          </div>
        ) : (
          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-8">
            <div className="mx-auto max-w-lg rounded-xl border border-neutral-200 bg-neutral-50 p-6 text-center dark:border-zinc-700 dark:bg-zinc-900">
              <p className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
                {t('designGate.planB.title', { defaultValue: '在新标签页中完成设计' })}
              </p>
              <p className="mt-2 text-xs leading-relaxed text-neutral-500 dark:text-zinc-400">
                {t('designGate.planB.desc', { defaultValue: '点击下方按钮在新标签页打开 OpenDesign Studio；完成设计后回到本弹窗点击「确认流转」。' })}
              </p>
              <button
                type="button"
                onClick={() => window.open(effectiveLaunch, '_blank', 'noopener')}
                className="mt-4 inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white hover:bg-sky-700"
              >
                <ExternalLink className="size-4" />
                {t('designGate.planB.open', { defaultValue: '打开 OpenDesign ↗' })}
              </button>
              <p className="mt-3 text-[11px] text-neutral-400 dark:text-zinc-500">
                {t('designGate.planB.returnHint', { defaultValue: '完成后回到此处确认流转' })}
              </p>
            </div>
          </div>
        )}

        {/* Bottom action bar: rework left; ready-state aware confirm right */}
        <div className="flex shrink-0 items-center justify-between gap-3 border-t border-neutral-100 px-5 py-3 dark:border-zinc-800">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onRework}
              className="rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm font-medium text-neutral-600 hover:bg-neutral-50 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
            >
              {t('designGate.back', { defaultValue: '打回需求' })}
            </button>
            <button
              type="button"
              onClick={reloadFrame}
              title={t('designGate.chat.reload', { defaultValue: '重新加载设计画布' })}
              className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm font-medium text-neutral-600 hover:bg-neutral-50 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
            >
              <RefreshCw className="size-3.5" />
              {t('designGate.reloadCanvas', { defaultValue: '刷新画布' })}
            </button>
          </div>
          <div className="flex items-center gap-3">
            {!ready && (
              <span className="hidden items-center gap-1.5 text-xs text-neutral-400 sm:inline-flex dark:text-zinc-500">
                <RotateCcw className="size-3" />
                {t('designGate.confirmHint', { defaultValue: '设计还在生成中；确认前可在画布内继续修改' })}
              </span>
            )}
            <button
              type="button"
              onClick={onConfirm}
              disabled={failed}
              title={ready
                ? t('designGate.confirmReady', { defaultValue: '设计已就绪，确认后进入实现编码' })
                : t('designGate.confirmNotReady', { defaultValue: '设计尚未标记完成；确认前请先在画布中检查设计' })}
              className="rounded-lg bg-sky-600 px-4 py-2 text-sm font-semibold text-white hover:bg-sky-700 disabled:opacity-50"
            >
              {ready
                ? t('designGate.confirm', { defaultValue: '确认并流转' })
                : t('designGate.confirmAnyway', { defaultValue: '设计未完成？仍要确认流转' })}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
