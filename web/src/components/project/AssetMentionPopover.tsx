import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { FileText } from 'lucide-react'
import { cn } from '../../lib/cn'

export type AssetRole = 'requirement_input' | 'reference'

export type AssetCandidate = {
  id: string
  displayName: string
  currentSha: string
  size?: number
  mime?: string
}

export type AssetPick = {
  fileId: string
  role: AssetRole
  required: boolean
}

type Props = {
  candidates: AssetCandidate[]
  query: string
  /** Remaining binding budget; 0 disables selection. */
  remaining: number
  onPick: (pick: AssetPick) => void
  onClose: () => void
}

const popoverCls =
  'absolute z-30 mt-1 w-full overflow-hidden rounded-lg border border-neutral-200 bg-white shadow-lg dark:border-zinc-700 dark:bg-zinc-900'

function formatBytes(n?: number): string {
  if (n == null) return ''
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)}MB`
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))}KB`
  return `${n}B`
}

/**
 * Inline picker for the @-mention flow in CreateTaskDialog: filters the
 * project asset library by the query typed after "@", and on selection asks
 * for the input role (requirement/reference) plus the required flag before
 * the binding is staged. Keyboard-first: arrows move, Enter advances,
 * Esc closes.
 */
export function AssetMentionPopover({ candidates, query, remaining, onPick, onClose }: Props) {
  const { t } = useTranslation()
  const [activeIdx, setActiveIdx] = useState(0)
  const [picked, setPicked] = useState<AssetCandidate | null>(null)
  const [role, setRole] = useState<AssetRole>('reference')
  const [required, setRequired] = useState(false)
  const listRef = useRef<HTMLDivElement>(null)

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return candidates
    return candidates.filter((c) => c.displayName.toLowerCase().includes(q))
  }, [candidates, query])

  useEffect(() => {
    setActiveIdx(0)
  }, [query])

  function choose(candidate: AssetCandidate) {
    if (remaining <= 0) return
    // Fresh selection starts from the role defaults: reference + not required.
    setPicked(candidate)
    setRole('reference')
    setRequired(false)
  }

  function confirm() {
    if (!picked) return
    onPick({ fileId: picked.id, role, required })
  }

  function onKeyDown(e: React.KeyboardEvent) {
    if (!picked) {
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setActiveIdx((i) => Math.min(i + 1, Math.max(filtered.length - 1, 0)))
      } else if (e.key === 'ArrowUp') {
        e.preventDefault()
        setActiveIdx((i) => Math.max(i - 1, 0))
      } else if (e.key === 'Enter') {
        e.preventDefault()
        const candidate = filtered[activeIdx]
        if (candidate) choose(candidate)
      } else if (e.key === 'Escape') {
        e.preventDefault()
        onClose()
      }
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      confirm()
    } else if (e.key === 'Escape') {
      e.preventDefault()
      setPicked(null)
    }
  }

  const noMatch = filtered.length === 0 && !picked

  return (
    <div
      className={popoverCls}
      data-testid="asset-mention-popover"
      onKeyDown={onKeyDown}
      {...{ tabIndex: -1 }}
      onMouseDown={(e) => e.preventDefault()}
    >
      {picked ? (
        <div className="space-y-2.5 p-3">
          <div className="flex items-center gap-2 text-sm font-medium text-neutral-800 dark:text-zinc-100">
            <FileText className="h-4 w-4 text-neutral-400" />
            {picked.displayName}
          </div>
          <div className="grid grid-cols-2 gap-2">
            {(['requirement_input', 'reference'] as AssetRole[]).map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setRole(r)}
                className={cn(
                  'rounded-md border px-2.5 py-1.5 text-xs font-medium transition-colors',
                  role === r
                    ? 'border-sky-400 bg-sky-50 text-sky-700 dark:border-sky-500 dark:bg-sky-950 dark:text-sky-300'
                    : 'border-neutral-200 text-neutral-600 hover:bg-neutral-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800',
                )}
              >
                {t(`tasks.assets.role.${r}`)}
              </button>
            ))}
          </div>
          <label className="flex items-center gap-2 text-xs text-neutral-600 dark:text-zinc-400">
            <input type="checkbox" checked={required} onChange={(e) => setRequired(e.target.checked)} className="h-3.5 w-3.5" />
            {t('tasks.assets.requiredToggle')}
          </label>
          <div className="flex items-center justify-end gap-2">
            <button
              type="button"
              onClick={() => setPicked(null)}
              className="rounded-md px-2.5 py-1 text-xs text-neutral-500 hover:text-neutral-700 dark:text-zinc-400 dark:hover:text-zinc-200"
            >
              {t('tasks.assets.back')}
            </button>
            <button
              type="button"
              onClick={confirm}
              className="rounded-md bg-sky-600 px-3 py-1 text-xs font-medium text-white hover:bg-sky-500 dark:bg-sky-500 dark:hover:bg-sky-400"
            >
              {t('tasks.assets.attach')}
            </button>
          </div>
        </div>
      ) : (
        <>
          <div className="border-b border-neutral-100 px-3 py-1.5 text-xs text-neutral-400 dark:border-zinc-800 dark:text-zinc-500">
            {remaining > 0 ? t('tasks.assets.remaining', { count: remaining }) : t('tasks.assets.maxReached')}
          </div>
          <div ref={listRef} className="max-h-52 overflow-y-auto">
            {noMatch ? (
              <div className="px-3 py-4 text-xs text-neutral-400 dark:text-zinc-500">{t('tasks.assets.emptyLibrary')}</div>
            ) : (
              filtered.map((candidate, idx) => (
                <button
                  key={candidate.id}
                  type="button"
                  onMouseEnter={() => setActiveIdx(idx)}
                  onClick={() => choose(candidate)}
                  disabled={remaining <= 0}
                  className={cn(
                    'flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm disabled:cursor-not-allowed disabled:opacity-50',
                    idx === activeIdx
                      ? 'bg-sky-50 text-neutral-900 dark:bg-sky-950/60 dark:text-zinc-100'
                      : 'text-neutral-700 dark:text-zinc-300',
                  )}
                >
                  <FileText className="h-3.5 w-3.5 shrink-0 text-neutral-400" />
                  <span className="min-w-0 flex-1 truncate">{candidate.displayName}</span>
                  <span className="shrink-0 font-mono text-[10px] text-neutral-400 dark:text-zinc-500">
                    {candidate.currentSha.slice(0, 8)}
                  </span>
                  {candidate.size != null && (
                    <span className="shrink-0 text-[10px] text-neutral-400 dark:text-zinc-500">{formatBytes(candidate.size)}</span>
                  )}
                </button>
              ))
            )}
          </div>
        </>
      )}
    </div>
  )
}
