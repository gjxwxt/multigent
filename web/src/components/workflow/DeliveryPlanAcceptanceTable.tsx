import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useApiJson } from '../../lib/use-api'
import { apiPost } from '../../lib/api'
import { cn } from '../../lib/cn'

// Acceptance table for a frozen delivery plan. Read-only by default; branch
// rows carrying a failed/deferred branch instance also surface the operator
// rulings (retry / defer) so a stalled stage has a visible way out instead of
// an orange badge and silence.
//
// The 404 answers of the endpoint are states, not errors: "task has no
// workflow run" and "no delivery plan is frozen for this run" both render an
// explicit not-frozen panel (the latter still shows run provenance when the
// run exists but no plan has been approved yet — that payload never arrives
// with a 404, so the distinction is made purely on the error message).

type AcceptanceRow = {
  wpId: string
  branchId: string
  waveIndex: number
  taskId?: string
  childRunId?: string
  childRunStatus?: string
  branchInstanceId?: string
  branchStatus?: string
  definitionId?: string
  definitionDigest?: string
  baseCommit?: string
  branchName?: string
  worktreeDir?: string
  deliveryCommit?: string
  qaBaselineKey?: string
  qaBaselinePresent?: boolean
  materializedAt?: string
  taskVars?: Record<string, string>
}

type AcceptanceSignoff = {
  stepId: string
  instanceId: string
  actorType?: string
  actorId?: string
  status: string
  decision?: string
  comments?: string
  finishedAt?: string
}

type DeliveryPlanView = {
  ok: boolean
  runId: string
  runStatus: string
  activeStepId: string
  approval?: {
    planId?: string
    runId?: string
    version?: number
    digest?: string
    status?: string
    approvedBy?: string
    approvedAt?: string
    versions?: number
  }
  materializations?: AcceptanceRow[]
  signoffs?: AcceptanceSignoff[]
  progress?: {
    ready?: string[]
    waiting?: string[]
    inProgress?: string[]
    completed?: string[]
    blocked?: string[]
    deferred?: string[]
  }
}

const NOT_FROZEN_MESSAGE = 'no delivery plan is frozen for this run'
const NO_RUN_MESSAGE = 'task has no workflow run'

// Failure categories mirror the backend enum (plan_branch_actions.go). They
// seed the defer dialog and drive the default suggested action per class.
const FAILURE_CATEGORIES = ['compile_test', 'business_assertion', 'environment', 'unknown'] as const
type FailureCategory = (typeof FAILURE_CATEGORIES)[number]

function suggestedActionFor(category: FailureCategory): 'retry' | 'defer' {
  // Environment flakiness is worth one more run; a failed business assertion
  // usually means the work itself is out of reach this round.
  return category === 'environment' || category === 'compile_test' ? 'retry' : 'defer'
}

function shortSha(value?: string, head = 7): string {
  const v = (value ?? '').trim()
  if (!v) return ''
  return v.length > head ? v.slice(0, head) : v
}

function branchStatusBadgeClass(status?: string): string {
  switch ((status ?? '').trim()) {
    case 'failed':
      return 'bg-red-100 text-red-700 dark:bg-red-950/60 dark:text-red-400'
    case 'deferred':
      return 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-400'
    case 'blocked':
      return 'bg-orange-100 text-orange-700 dark:bg-orange-950/60 dark:text-orange-400'
    case 'skipped':
      return 'bg-neutral-100 text-neutral-500 dark:bg-zinc-900 dark:text-zinc-500'
    case 'completed':
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-400'
    case 'running':
      return 'bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-400'
    default:
      return 'bg-neutral-100 text-neutral-500 dark:bg-zinc-900 dark:text-zinc-500'
  }
}

function Cell({ children, mono = false }: { children: React.ReactNode; mono?: boolean }) {
  const { t } = useTranslation()
  const empty = children === undefined || children === null || String(children) === ''
  return (
    <td className={cn('px-2 py-1.5 align-top text-xs', mono && 'font-mono', empty ? 'text-neutral-300 dark:text-zinc-600' : 'text-neutral-700 dark:text-zinc-300')}>
      {empty ? t('workflows.acceptance.emptyCell', { defaultValue: '—' }) : children}
    </td>
  )
}

function BranchStatusBadge({ status }: { status?: string }) {
  const { t } = useTranslation()
  const s = (status ?? '').trim()
  if (!s) return null
  const label = t(`workflows.acceptance.branchStatus.${s}`, { defaultValue: s })
  return (
    <span className={cn('ml-1 inline-block rounded px-1 py-px font-sans text-[10px] font-medium', branchStatusBadgeClass(s))} title={label}>
      {label}
    </span>
  )
}

function BranchActionButtons({ project, taskId, row, onDone }: { project: string; taskId: string; row: AcceptanceRow; onDone: () => void }) {
  const { t } = useTranslation()
  const [busy, setBusy] = useState<'retry' | 'defer' | null>(null)
  const [category, setCategory] = useState<FailureCategory>('environment')
  const [createFollowUp, setCreateFollowUp] = useState(true)
  const [deferOpen, setDeferOpen] = useState(false)
  const status = (row.branchStatus ?? '').trim()
  const canRetry = status === 'failed'
  const canDefer = status === 'failed' || status === 'deferred'
  if (!canRetry && !canDefer) return null

  const run = async (action: 'retry' | 'defer') => {
    setBusy(action)
    try {
      await apiPost(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/workflow/branch/${encodeURIComponent(row.branchId)}/${action}`,
        action === 'defer' ? { failureCategory: category, createFollowUp } : {},
      )
      setDeferOpen(false)
      onDone()
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="mt-1 flex flex-wrap items-center gap-1">
      {canRetry && (
        <button
          type="button"
          disabled={busy !== null}
          onClick={() => run('retry')}
          className="rounded border border-sky-300 bg-sky-50 px-1.5 py-0.5 text-[10px] font-medium text-sky-700 hover:bg-sky-100 disabled:opacity-50 dark:border-sky-800 dark:bg-sky-950/40 dark:text-sky-400 dark:hover:bg-sky-950/70"
        >
          {busy === 'retry' ? t('workflows.acceptance.actions.retrying', { defaultValue: '重试中…' }) : t('workflows.acceptance.actions.retry', { defaultValue: '重试' })}
        </button>
      )}
      {canDefer && !deferOpen && (
        <button
          type="button"
          disabled={busy !== null}
          onClick={() => setDeferOpen(true)}
          className="rounded border border-amber-300 bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-800 hover:bg-amber-100 disabled:opacity-50 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-400 dark:hover:bg-amber-950/70"
        >
          {t('workflows.acceptance.actions.defer', { defaultValue: '缓办' })}
        </button>
      )}
      {deferOpen && (
        <span className="inline-flex flex-wrap items-center gap-1 rounded border border-amber-200 bg-amber-50/60 px-1.5 py-0.5 dark:border-amber-900 dark:bg-amber-950/30">
          <select
            value={category}
            onChange={(e) => setCategory(e.target.value as FailureCategory)}
            className="rounded border border-amber-300 bg-white px-1 py-px text-[10px] text-amber-900 dark:border-amber-800 dark:bg-zinc-900 dark:text-amber-300"
          >
            {FAILURE_CATEGORIES.map((c) => (
              <option key={c} value={c}>
                {t(`workflows.acceptance.failureCategory.${c}`, { defaultValue: c })}
                {suggestedActionFor(c) === 'retry' ? ' ↻' : ' ⏸'}
              </option>
            ))}
          </select>
          <label className="inline-flex items-center gap-0.5 text-[10px] text-amber-800 dark:text-amber-400">
            <input type="checkbox" checked={createFollowUp} onChange={(e) => setCreateFollowUp(e.target.checked)} className="h-2.5 w-2.5" />
            {t('workflows.acceptance.actions.createFollowUp', { defaultValue: '建待办' })}
          </label>
          <button
            type="button"
            disabled={busy !== null}
            onClick={() => run('defer')}
            className="rounded bg-amber-600 px-1.5 py-px text-[10px] font-medium text-white hover:bg-amber-700 disabled:opacity-50"
          >
            {busy === 'defer' ? t('workflows.acceptance.actions.deferring', { defaultValue: '缓办中…' }) : t('workflows.acceptance.actions.confirmDefer', { defaultValue: '确认' })}
          </button>
          <button type="button" onClick={() => setDeferOpen(false)} className="text-[10px] text-neutral-400 hover:text-neutral-600 dark:text-zinc-500">
            ✕
          </button>
        </span>
      )}
    </div>
  )
}

export function DeliveryPlanAcceptanceTable({ project, taskId, reloadKey = 0 }: { project: string; taskId: string; reloadKey?: number }) {
  const { t } = useTranslation()
  const [actionNonce, setActionNonce] = useState(0)
  const state = useApiJson<DeliveryPlanView & { errorMessage?: string }>(
    `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/workflow/delivery-plan`,
    reloadKey + actionNonce,
    { silentStatuses: [404] },
  )

  if (state.status === 'loading') {
    return (
      <section className="mx-4 mb-4 rounded-xl border border-neutral-200 bg-white px-4 py-3 dark:border-zinc-700 dark:bg-zinc-950">
        <p className="text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.acceptance.loading', { defaultValue: '正在读取验收表…' })}</p>
      </section>
    )
  }

  if (state.status === 'error') {
    const message = state.error instanceof Error ? state.error.message : String(state.error)
    const noRun = message.includes(NO_RUN_MESSAGE)
    const notFrozen = message.includes(NOT_FROZEN_MESSAGE)
    if (!noRun && !notFrozen) {
      return (
        <section className="mx-4 mb-4 rounded-xl border border-neutral-200 bg-white px-4 py-3 dark:border-zinc-700 dark:bg-zinc-950">
          <p className="text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.acceptance.loadFailed', { defaultValue: '验收表读取失败' })}: {message}</p>
        </section>
      )
    }
    return (
      <section className="mx-4 mb-4 rounded-xl border border-amber-200 bg-amber-50/60 px-4 py-3 dark:border-amber-900/60 dark:bg-amber-950/30">
        <p className="text-sm font-semibold text-amber-800 dark:text-amber-300">{t('workflows.acceptance.notFrozenTitle', { defaultValue: '交付计划尚未冻结' })}</p>
        <p className="mt-1 text-xs text-amber-700 dark:text-amber-400">
          {noRun
            ? t('workflows.acceptance.noRunBody', { defaultValue: '该任务尚未启动工作流运行，因此还没有交付计划。' })
            : t('workflows.acceptance.notFrozenBody', { defaultValue: '运行已存在，但契约评审尚未批准冻结交付计划；批准冻结后此处显示验收表。' })}
        </p>
      </section>
    )
  }

  const view = state.data
  const approval = view.approval
  const rows = view.materializations ?? []
  const signoffs = view.signoffs ?? []
  const progress = view.progress
  const deferredSet = new Set(progress?.deferred ?? [])
  const blockedSet = new Set(progress?.blocked ?? [])
  const frozen = Boolean(approval && approval.version)

  return (
    <section className="mx-4 mb-4 rounded-xl border border-neutral-200 bg-white px-4 py-3 dark:border-zinc-700 dark:bg-zinc-950">
      {frozen && approval ? (
        <div className="mb-2">
          <p className="text-sm font-semibold text-neutral-800 dark:text-zinc-200">{t('workflows.acceptance.frozenTitle', { defaultValue: '交付计划已冻结' })}</p>
          <p className="mt-1 font-mono text-[11px] leading-relaxed text-neutral-500 dark:text-zinc-500">
            {approval.planId || '—'} · v{approval.version} · {t('workflows.acceptance.digest', { defaultValue: '摘要' })} {shortSha(approval.digest, 12)}
            {approval.approvedBy ? ` · ${t('workflows.acceptance.approvedBy', { defaultValue: '批准人' })} ${approval.approvedBy}` : ''}
            {approval.versions ? ` · v×${approval.versions}` : ''}
          </p>
          {blockedSet.size > 0 && (
            <p className="mt-1 text-[11px] font-medium text-orange-700 dark:text-orange-400">
              {t('workflows.acceptance.blockedNote', { defaultValue: '阻塞中的工作包' })}: {[...blockedSet].join(', ')}
            </p>
          )}
          {deferredSet.size > 0 && (
            <p className="mt-0.5 text-[11px] text-amber-700 dark:text-amber-400">
              {t('workflows.acceptance.deferredNote', { defaultValue: '本轮缓办（人工）' })}: {[...deferredSet].join(', ')}
            </p>
          )}
        </div>
      ) : (
        <div className="mb-2">
          <p className="text-sm font-semibold text-amber-800 dark:text-amber-300">{t('workflows.acceptance.notFrozenTitle', { defaultValue: '交付计划尚未冻结' })}</p>
          <p className="mt-1 text-xs text-amber-700 dark:text-amber-400">{t('workflows.acceptance.notFrozenBody', { defaultValue: '运行已存在，但契约评审尚未批准冻结交付计划；批准冻结后此处显示验收表。' })}</p>
        </div>
      )}

      {rows.length === 0 ? (
        <p className="text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.acceptance.noRows', { defaultValue: '冻结计划中没有工作包物化记录。' })}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[46rem] border-collapse">
            <thead>
              <tr className="border-b border-neutral-200 text-left text-[10px] uppercase tracking-wider text-neutral-400 dark:border-zinc-800 dark:text-zinc-500">
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colWp', { defaultValue: '工作包 / 波次' })}</th>
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colRun', { defaultValue: '子运行' })}</th>
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colBranch', { defaultValue: '分支' })}</th>
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colDelivery', { defaultValue: '交付 SHA' })}</th>
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colIntegration', { defaultValue: '集成候选' })}</th>
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colQa', { defaultValue: 'QA' })}</th>
                <th className="px-2 py-1 font-medium">{t('workflows.acceptance.colTask', { defaultValue: '任务' })}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={`${row.wpId}:${row.branchId}`} className="border-b border-neutral-100 last:border-b-0 dark:border-zinc-900">
                  <Cell>
                    <span className="font-semibold">{row.wpId}</span>
                    <span className="ml-1 text-neutral-400 dark:text-zinc-500">w{row.waveIndex}</span>
                  </Cell>
                  <Cell mono>
                    {row.childRunId ? `${shortSha(row.childRunId, 10)}${row.childRunStatus ? ` (${row.childRunStatus})` : ''}` : ''}
                  </Cell>
                  <Cell mono>
                    {row.branchName || row.branchId}
                    <BranchStatusBadge status={row.branchStatus} />
                    {row.worktreeDir ? <span className="block truncate font-mono text-[10px] text-neutral-400 dark:text-zinc-500" title={row.worktreeDir}>{row.worktreeDir}</span> : null}
                    <BranchActionButtons project={project} taskId={taskId} row={row} onDone={() => setActionNonce((n) => n + 1)} />
                  </Cell>
                  <Cell mono>{shortSha(row.deliveryCommit)}</Cell>
                  <Cell mono>{shortSha(row.baseCommit)}</Cell>
                  <Cell>
                    {row.qaBaselinePresent
                      ? <span className="text-emerald-700 dark:text-emerald-400">{t('workflows.acceptance.qaBaselineOk', { defaultValue: '基线在档' })}</span>
                      : ''}
                  </Cell>
                  <Cell mono>{row.taskId}</Cell>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {signoffs.length > 0 && (
        <div className="mt-3 border-t border-neutral-100 pt-2 dark:border-zinc-900">
          <p className="text-[10px] font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('workflows.acceptance.signoffs', { defaultValue: '人工签核' })}</p>
          <ul className="mt-1 space-y-1">
            {signoffs.map((s) => (
              <li key={s.instanceId} className="text-xs text-neutral-600 dark:text-zinc-400">
                <span className="font-medium">{s.stepId}</span>
                {s.actorId ? ` · ${s.actorId}` : ''}
                {s.decision ? ` · ${s.decision}` : ''}
                {s.finishedAt ? ` · ${s.finishedAt}` : ''}
                {s.comments ? <span className="block text-[11px] text-neutral-400 dark:text-zinc-500">{s.comments}</span> : null}
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}
