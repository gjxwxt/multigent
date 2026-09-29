import { useEffect, forwardRef, useImperativeHandle, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { FileText } from 'lucide-react'
import { cn } from '../../lib/cn'

export type AssetCandidate = {
  id: string
  displayName: string
  currentSha: string
  size?: number
  mime?: string
}

type Props = {
  candidates: AssetCandidate[]
  query: string
  /** Remaining binding budget; 0 disables selection. */
  remaining: number
  /** Caret-anchored position, computed by the parent from the textarea. */
  style?: React.CSSProperties
  /** Selecting a file immediately stages the binding (reference / not required). */
  onPick: (fileId: string) => void
  onClose: () => void
}

export type AssetMentionPopoverHandle = {
  /** Keyboard handling delegated from the textarea (focus never leaves it). */
  handleKey: (e: React.KeyboardEvent) => void
}

function formatBytes(n?: number): string {
  if (n == null) return ''
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)}MB`
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))}KB`
  return `${n}B`
}

const popoverCls =
  'absolute z-30 w-80 overflow-hidden rounded-lg border border-neutral-200 bg-white shadow-lg dark:border-zinc-700 dark:bg-zinc-900'

/**
 * Inline picker for the @-mention flow in CreateTaskDialog: filters the
 * project asset library by the query typed after "@". ZCode-style one-step
 * selection — picking a file inserts the marker and stages the binding with
 * role/required defaults; the user's prose around the @ carries the intent.
 * Keyboard-first: arrows move, Enter picks, Esc closes; focus stays in the
 * textarea (keys are delegated via the handle).
 */
export const AssetMentionPopover = forwardRef<AssetMentionPopoverHandle, Props>(function AssetMentionPopover(
  { candidates, query, remaining, style, onPick, onClose },
  ref,
) {
  const { t } = useTranslation()
  const [activeIdx, setActiveIdx] = useState(0)
  const containerRef = useRef<HTMLDivElement>(null)

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return candidates
    return candidates.filter((c) => c.displayName.toLowerCase().includes(q))
  }, [candidates, query])

  useEffect(() => {
    setActiveIdx(0)
  }, [query])

  // Click-outside closes. The textarea is outside this container, so clicking
  // back into the text dismisses the picker — standard mentions behavior.
  useEffect(() => {
    function onMouseDown(e: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) onClose()
    }
    document.addEventListener('mousedown', onMouseDown)
    return () => document.removeEventListener('mousedown', onMouseDown)
  }, [onClose])

  useImperativeHandle(ref, () => ({
    handleKey(e: React.KeyboardEvent) {
      if (e.key === 'ArrowDown') {
        setActiveIdx((i) => Math.min(i + 1, Math.max(filtered.length - 1, 0)))
      } else if (e.key === 'ArrowUp') {
        setActiveIdx((i) => Math.max(i - 1, 0))
      } else if (e.key === 'Enter') {
        const candidate = filtered[activeIdx]
        if (candidate && remaining > 0) onPick(candidate.id)
      } else if (e.key === 'Escape') {
        onClose()
      }
    },
  }))

  return (
    <div ref={containerRef} className={popoverCls} style={style} data-testid="asset-mention-popover">
      <div className="border-b border-neutral-100 px-3 py-1.5 text-xs text-neutral-400 dark:border-zinc-800 dark:text-zinc-500">
        {remaining > 0 ? t('tasks.assets.remaining', { count: remaining }) : t('tasks.assets.maxReached')}
      </div>
      <div className="max-h-52 overflow-y-auto">
        {filtered.length === 0 ? (
          <div className="px-3 py-4 text-xs text-neutral-400 dark:text-zinc-500">{t('tasks.assets.emptyLibrary')}</div>
        ) : (
          filtered.map((candidate, idx) => (
            <button
              key={candidate.id}
              type="button"
              onMouseEnter={() => setActiveIdx(idx)}
              onClick={() => remaining > 0 && onPick(candidate.id)}
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
    </div>
  )
})
