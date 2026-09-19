import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronDown, ChevronRight, FileDiff, Loader2 } from 'lucide-react'
import { apiFetch } from '../../lib/api'
import { cn } from '../../lib/cn'

type DiffFile = {
  path: string
  oldPath?: string
  status: string
  additions: number
  deletions: number
  binary?: boolean
}

type TaskDiff = {
  base: string
  head: string
  files: DiffFile[]
  patch: string
  truncated?: boolean
  note?: string
}

type Props = {
  project: string
  taskID: string
}

const STATUS_STYLE: Record<string, string> = {
  added: 'text-emerald-700 dark:text-emerald-400',
  deleted: 'text-red-700 dark:text-red-400',
  renamed: 'text-violet-700 dark:text-violet-400',
  modified: 'text-amber-700 dark:text-amber-400',
}

// The approval gate is only as honest as the evidence in front of it. Fields the
// authoring agent declared for itself are a self-report; this panel is the one
// thing in the review dialog that comes from git rather than from the model.
export function TaskDiffEvidence({ project, taskID }: Props) {
  const { t } = useTranslation()
  const [diff, setDiff] = useState<TaskDiff | null>(null)
  const [unavailable, setUnavailable] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setDiff(null)
    setUnavailable(null)
    apiFetch<TaskDiff>(
      `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/diff`,
      // A task without recorded commits is the normal case early in a run, not
      // an error worth a toast.
      { silentStatuses: [404, 409] },
    )
      .then((res) => {
        if (!cancelled) setDiff(res)
      })
      .catch((err: unknown) => {
        if (!cancelled) setUnavailable(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [project, taskID])

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-neutral-100 bg-white px-2.5 py-2 text-xs text-neutral-500 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-400">
        <Loader2 className="h-3.5 w-3.5 animate-spin" />
        {t('tasks.diff.loading', { defaultValue: 'Loading diff…' })}
      </div>
    )
  }

  if (!diff) {
    return (
      <div className="rounded-lg border border-dashed border-neutral-200 px-2.5 py-2 text-xs text-neutral-500 dark:border-zinc-800 dark:text-zinc-400">
        {unavailable ?? t('tasks.diff.unavailable', { defaultValue: 'No diff available for this task yet.' })}
      </div>
    )
  }

  const totalAdditions = diff.files.reduce((sum, file) => sum + file.additions, 0)
  const totalDeletions = diff.files.reduce((sum, file) => sum + file.deletions, 0)

  return (
    <div className="rounded-lg border border-neutral-200 bg-white dark:border-zinc-800 dark:bg-zinc-950">
      <button
        type="button"
        onClick={() => setExpanded((prev) => !prev)}
        className="flex w-full items-center gap-2 px-2.5 py-2 text-left text-xs"
      >
        {expanded ? <ChevronDown className="h-3.5 w-3.5 shrink-0 text-neutral-400" /> : <ChevronRight className="h-3.5 w-3.5 shrink-0 text-neutral-400" />}
        <FileDiff className="h-3.5 w-3.5 shrink-0 text-neutral-500" />
        <span className="font-medium text-neutral-800 dark:text-zinc-200">
          {t('tasks.diff.filesChanged', {
            fileCount: diff.files.length,
            defaultValue: '{{fileCount}} files changed',
          })}
        </span>
        <span className="text-emerald-700 dark:text-emerald-400">+{totalAdditions}</span>
        <span className="text-red-700 dark:text-red-400">-{totalDeletions}</span>
        <span className="ml-auto font-mono text-[10px] text-neutral-400">
          {diff.base.slice(0, 7)}..{diff.head.slice(0, 7)}
        </span>
      </button>

      {diff.files.length === 0 ? (
        <div className="px-2.5 pb-2 text-xs text-neutral-500 dark:text-zinc-400">
          {t('tasks.diff.empty', { defaultValue: 'These two commits have identical trees.' })}
        </div>
      ) : (
        <ul className="max-h-40 overflow-y-auto border-t border-neutral-100 px-1 py-1 dark:border-zinc-800">
          {diff.files.map((file) => (
            <li key={`${file.status}:${file.path}`} className="flex items-baseline gap-2 px-1.5 py-0.5 font-mono text-[11px]">
              <span className={cn('w-14 shrink-0 capitalize', STATUS_STYLE[file.status] ?? 'text-neutral-600 dark:text-zinc-400')}>
                {file.status}
              </span>
              <span className="truncate text-neutral-700 dark:text-zinc-300" title={file.oldPath ? `${file.oldPath} → ${file.path}` : file.path}>
                {file.oldPath ? `${file.oldPath} → ${file.path}` : file.path}
              </span>
              <span className="ml-auto shrink-0 text-neutral-400">
                {file.binary ? t('tasks.diff.binary', { defaultValue: 'binary' }) : `+${file.additions} -${file.deletions}`}
              </span>
            </li>
          ))}
        </ul>
      )}

      {expanded && diff.patch && (
        <pre className="max-h-[60vh] overflow-auto border-t border-neutral-100 bg-neutral-50 px-2.5 py-2 text-[11px] leading-relaxed dark:border-zinc-800 dark:bg-zinc-900">
          {diff.patch.split('\n').map((line, idx) => (
            <div
              key={`${idx}:${line.slice(0, 24)}`}
              className={cn(
                'whitespace-pre',
                line.startsWith('+') && !line.startsWith('+++') && 'bg-emerald-50 text-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-200',
                line.startsWith('-') && !line.startsWith('---') && 'bg-red-50 text-red-900 dark:bg-red-950/40 dark:text-red-200',
                line.startsWith('@@') && 'text-sky-700 dark:text-sky-400',
              )}
            >
              {line || ' '}
            </div>
          ))}
        </pre>
      )}

      {expanded && !diff.patch && (
        <div className="border-t border-neutral-100 px-2.5 py-2 text-xs text-neutral-500 dark:border-zinc-800 dark:text-zinc-400">
          {diff.note ?? t('tasks.diff.noPatch', { defaultValue: 'Patch not shown.' })}
        </div>
      )}

      {diff.truncated && (
        <div className="border-t border-amber-100 bg-amber-50 px-2.5 py-1.5 text-[11px] text-amber-800 dark:border-amber-900/50 dark:bg-amber-950/40 dark:text-amber-300">
          {t('tasks.diff.truncated', { defaultValue: 'This diff is truncated — review the full range before approving.' })}
        </div>
      )}
    </div>
  )
}
