import { useTranslation } from 'react-i18next'
import { Check, Layers, Sparkles } from 'lucide-react'
import { cn } from '../../lib/cn'

export type DesignSource = 'existing' | 'generate'

/**
 * One selectable card of the design-source chooser, styled after the
 * "new GitLab project" two-card dialog (docs §5.1): a leading radio dot, a
 * title with an optional 推荐 chip, and a two-line description.
 */
export function DesignChoiceCard({
  value,
  title,
  description,
  recommended = false,
  selected,
  onSelect,
}: {
  value: DesignSource
  title: string
  description: string
  recommended?: boolean
  selected: boolean
  onSelect: (value: DesignSource) => void
}) {
  const { t } = useTranslation()
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      data-value={value}
      onClick={() => onSelect(value)}
      className={cn(
        'group flex min-h-[96px] flex-1 basis-0 flex-col rounded-xl border p-4 text-left transition-colors',
        selected
          ? 'border-sky-500 bg-sky-50/70 ring-1 ring-sky-500 dark:border-sky-500 dark:bg-sky-950/40'
          : 'border-neutral-300 bg-white hover:border-neutral-400 dark:border-zinc-700 dark:bg-zinc-900 dark:hover:border-zinc-600',
      )}
    >
      <span className="flex items-center gap-2">
        <span
          className={cn(
            'flex size-[18px] shrink-0 items-center justify-center rounded-full border transition-colors',
            selected
              ? 'border-sky-600 bg-sky-600 text-white dark:border-sky-500 dark:bg-sky-500'
              : 'border-neutral-400 bg-white group-hover:border-sky-500 dark:border-zinc-500 dark:bg-zinc-900',
          )}
        >
          {selected && <Check className="size-3" strokeWidth={3} />}
        </span>
        <span className="flex items-center gap-1.5 text-sm font-medium text-neutral-900 dark:text-zinc-100">
          {value === 'existing' ? <Layers className="size-4 text-neutral-400 dark:text-zinc-500" /> : <Sparkles className="size-4 text-sky-500" />}
          {title}
          {recommended && (
            <span className="rounded-full bg-sky-100 px-1.5 py-0.5 text-[10px] font-semibold text-sky-700 dark:bg-sky-900/60 dark:text-sky-300">
              {t('designGate.choice.recommended', { defaultValue: '推荐' })}
            </span>
          )}
        </span>
      </span>
      <span className="mt-2 text-xs leading-relaxed text-neutral-500 dark:text-zinc-400">{description}</span>
    </button>
  )
}
