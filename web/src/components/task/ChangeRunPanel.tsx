// Change Run panel (Task 3.1 UI slice): surfaces the task's active proposal
// inside the task detail modal — diff view, state, and the operator actions
// (apply / reject / rollback). Read-only for non-operators: the backend gates
// every state change on project OPERATOR, the panel only hides disabled
// buttons and surfaces the error the API returns.
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { apiPost } from '../../lib/api'
import { useApiJson } from '../../lib/use-api'
import { cn } from '../../lib/cn'

export type ChangeRunProposal = {
  id: string
  project: string
  taskId: string
  state: string
  actor: string
  request?: string
  patch?: string
  diff?: string
  paths: string[]
  verification?: Record<string, string>
  postimage?: Record<string, string>
  appliedSha?: string
  rollbackState?: string
  createdAt: string
  updatedAt: string
}

const stateChip: Record<string, string> = {
  awaiting_approval: 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-300',
  applying: 'bg-sky-100 text-sky-800 dark:bg-sky-950/60 dark:text-sky-300',
  applied: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-300',
  verification_failed: 'bg-red-100 text-red-800 dark:bg-red-950/60 dark:text-red-300',
  rejected: 'bg-neutral-200 text-neutral-700 dark:bg-zinc-800 dark:text-zinc-400',
}

// diffLineClass colors one unified-diff line: + adds, - deletes, @@ hunks,
// file headers — mirroring the conventional GitHub palette.
function diffLineClass(line: string): string {
  if (line.startsWith('+++') || line.startsWith('---')) return 'text-neutral-500 dark:text-zinc-500'
  if (line.startsWith('diff --git') || line.startsWith('index ')) return 'text-neutral-400 dark:text-zinc-600 font-semibold'
  if (line.startsWith('@@')) return 'text-sky-700 dark:text-sky-400'
  if (line.startsWith('+')) return 'bg-emerald-50 text-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300'
  if (line.startsWith('-')) return 'bg-red-50 text-red-800 dark:bg-red-950/40 dark:text-red-300'
  return 'text-neutral-700 dark:text-zinc-300'
}

export function ChangeRunPanel({ project, taskId, canOperator, onChanged }: {
  project: string
  taskId: string
  canOperator: boolean
  onChanged?: () => void
}) {
  const { t } = useTranslation()
  const [version, setVersion] = useState(0)
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [showDiff, setShowDiff] = useState(false)

  const reload = useCallback(() => {
    setVersion((v) => v + 1)
    onChanged?.()
  }, [onChanged])

  // Poll while an action is in flight — clone + container verification can
  // take tens of seconds, and the apply response only returns at the end.
  const [pollVersion, setPollVersion] = useState(0)
  useEffect(() => {
    if (!busy) return
    const timer = window.setInterval(() => setPollVersion((v) => v + 1), 2000)
    return () => window.clearInterval(timer)
  }, [busy])

  const listState = useApiJson<{ proposals: ChangeRunProposal[] }>(
    `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs`,
    version + pollVersion,
    { silentStatuses: [404] },
  )

  const proposal = useMemo<ChangeRunProposal | null>(() => {
    if (listState.status !== 'ok') return null
    return listState.data?.proposals?.[0] ?? null
  }, [listState])

  async function act(action: 'apply' | 'reject' | 'rollback', confirmMsg?: string) {
    if (!proposal) return
    if (confirmMsg && !window.confirm(confirmMsg)) return
    setBusy(action)
    setError(null)
    try {
      await apiPost(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs/${encodeURIComponent(proposal.id)}/${action}`,
        {},
      )
      reload()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(null)
    }
  }

  if (listState.status === 'error') {
    const status = (listState.error as { status?: number }).status
    if (status === 404) return null // task list surface without worktree plumbing
    return null
  }
  if (!proposal) return null

  const diffText = proposal.diff || proposal.patch || ''
  const isAwaiting = proposal.state === 'awaiting_approval'
  const isApplied = proposal.state === 'applied'
  const isFailed = proposal.state === 'verification_failed'
  const chipCls = stateChip[proposal.state] ?? 'bg-neutral-200 text-neutral-700 dark:bg-zinc-800 dark:text-zinc-400'

  return (
    <div className="border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40" data-testid="change-run-panel">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
          {t('changerun.title', { defaultValue: '变更提案 (Change Run)' })}
        </span>
        <span className={cn('rounded-full px-2 py-0.5 text-xs font-medium', chipCls)}>
          {t(`changerun.state.${proposal.state}`, { defaultValue: proposal.state })}
        </span>
      </div>

      {proposal.request && (
        <p className="mt-1.5 text-sm text-neutral-700 dark:text-zinc-300">{proposal.request}</p>
      )}

      {proposal.paths.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1">
          {proposal.paths.map((p) => (
            <code key={p} className="rounded bg-neutral-100 px-1.5 py-0.5 text-xs text-neutral-700 dark:bg-zinc-800 dark:text-zinc-300">{p}</code>
          ))}
        </div>
      )}

      {isApplied && proposal.appliedSha && (
        <p className="mt-2 text-xs text-neutral-500 dark:text-zinc-500">
          {t('changerun.appliedSha', { defaultValue: '已应用提交' })}: <code>{proposal.appliedSha.slice(0, 10)}</code>
          {proposal.rollbackState && ` · ${proposal.rollbackState}`}
        </p>
      )}

      {isFailed && proposal.verification && (
        <div className="mt-2 rounded-lg border border-red-200 bg-red-50 p-2 text-xs text-red-800 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
          {Object.entries(proposal.verification).slice(0, 6).map(([k, v]) => (
            <p key={k} className="truncate"><span className="font-mono">{k}</span>: {v}</p>
          ))}
        </div>
      )}

      {diffText && (
        <div className="mt-2">
          <button
            type="button"
            className="text-xs font-medium text-sky-600 hover:underline dark:text-sky-400"
            onClick={() => setShowDiff((v) => !v)}
          >
            {showDiff ? t('changerun.hideDiff', { defaultValue: '收起 Diff' }) : t('changerun.showDiff', { defaultValue: '查看 Diff' })}
          </button>
          {showDiff && (
            <pre className="mt-1.5 max-h-80 overflow-auto rounded-lg bg-neutral-50 p-2 text-xs leading-5 dark:bg-zinc-900">
              {diffText.split('\n').map((line, i) => (
                <div key={i} className={cn('px-1 font-mono whitespace-pre', diffLineClass(line))}>{line || ' '}</div>
              ))}
            </pre>
          )}
        </div>
      )}

      {error && (
        <p className="mt-2 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
          {error}
        </p>
      )}

      {canOperator && (
        <div className="mt-3 flex flex-wrap gap-2">
          {isAwaiting && (
            <>
              <button
                type="button"
                disabled={busy !== null}
                data-testid="changerun-apply"
                onClick={() => void act('apply')}
                className="rounded-lg bg-emerald-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-emerald-500 disabled:opacity-50"
              >
                {busy === 'apply' ? t('changerun.applying', { defaultValue: '应用中…' }) : t('changerun.apply', { defaultValue: '应用' })}
              </button>
              <button
                type="button"
                disabled={busy !== null}
                data-testid="changerun-reject"
                onClick={() => void act('reject')}
                className="rounded-lg border border-neutral-300 px-3 py-1.5 text-sm font-medium text-neutral-700 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
              >
                {t('changerun.reject', { defaultValue: '拒绝' })}
              </button>
            </>
          )}
          {isApplied && (
            <button
              type="button"
              disabled={busy !== null}
              data-testid="changerun-rollback"
              onClick={() => void act('rollback', t('changerun.rollbackConfirm', {
                defaultValue: '回滚将把本次提案触及的文件恢复到应用前状态；若应用后有人工改动将被拒绝。确认回滚？',
              }))}
              className="rounded-lg border border-red-300 px-3 py-1.5 text-sm font-medium text-red-700 hover:bg-red-50 disabled:opacity-50 dark:border-red-900/60 dark:text-red-400 dark:hover:bg-red-950/30"
            >
              {busy === 'rollback' ? t('changerun.rollingBack', { defaultValue: '回滚中…' }) : t('changerun.rollback', { defaultValue: '回滚' })}
            </button>
          )}
        </div>
      )}
    </div>
  )
}
