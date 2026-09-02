import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowLeft, Loader2, X } from 'lucide-react'
import { apiFetch } from '../../lib/api'
import { cn } from '../../lib/cn'
import { overlayDismissProps } from '../ui/overlay'
import { DesignChoiceCard, type DesignSource } from './DesignChoiceCard'

export type DesignProjectOption = {
  id: string
  name: string
  designSystemId?: string
  updatedAt?: string
}

/**
 * Stage-one modal of the design gate: pick the design source before anything
 * is generated (docs §5.1/§8.1). Clicking "existing" jumps straight into the
 * OD project list (one step — a gray dead-end button here confused users);
 * the list view's back button only returns to the source chooser.
 */
export function DesignSourceChoiceModal({
  project,
  taskID,
  taskTitle,
  busy,
  error,
  onClose,
  onConfirmExisting,
  onStartGenerate,
}: {
  project: string
  taskID: string
  taskTitle?: string
  busy?: boolean
  error?: string | null
  onClose: () => void
  onConfirmExisting: (projectId: string, projectName: string) => void
  onStartGenerate: () => void
}) {
  const { t } = useTranslation()
  const [selected, setSelected] = useState<DesignSource>('generate')
  const [pickingExisting, setPickingExisting] = useState(false)
  const [projects, setProjects] = useState<DesignProjectOption[] | null>(null)
  const [listErr, setListErr] = useState<string | null>(null)
  const [listBusy, setListBusy] = useState(false)
  const [picked, setPicked] = useState<DesignProjectOption | null>(null)
  const listLoadedFor = useRef('')

  useEffect(() => {
    if (!pickingExisting || listLoadedFor.current === project) return
    listLoadedFor.current = project
    setListBusy(true)
    setListErr(null)
    apiFetch<{ projects: DesignProjectOption[] }>(
      `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/projects`,
    )
      .then((data) => setProjects(data?.projects ?? []))
      .catch((e) => {
        setListErr(e instanceof Error ? e.message : String(e))
        listLoadedFor.current = ''
      })
      .finally(() => setListBusy(false))
  }, [pickingExisting, project, taskID])

  function handleSelect(value: DesignSource) {
    setSelected(value)
    // One-step entry: choosing "existing" is the intent to browse the list.
    if (value === 'existing') setPickingExisting(true)
  }

  const confirmLabel = pickingExisting
    ? t('designGate.confirm', { defaultValue: '确认并流转' })
    : selected === 'generate'
      ? t('designGate.start', { defaultValue: '开始设计' })
      : t('designGate.chooseProject', { defaultValue: '下一步：选择项目' })

  function handleNext() {
    if (busy) return
    if (!pickingExisting) {
      if (selected === 'generate') {
        onStartGenerate()
      } else {
        setPickingExisting(true)
      }
      return
    }
    if (picked) onConfirmExisting(picked.id, picked.name)
  }

  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/45 p-4 backdrop-blur-[1px]" role="presentation" {...overlayDismissProps(onClose)}>
      <div
        className="w-full max-w-2xl animate-scale-in rounded-xl border border-neutral-200 bg-white shadow-2xl dark:border-zinc-700 dark:bg-zinc-900"
        role="dialog"
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-3 border-b border-neutral-100 px-5 py-4 dark:border-zinc-800">
          <div className="min-w-0">
            <h2 className="text-base font-semibold text-neutral-900 dark:text-zinc-100">
              {t('designGate.title', { defaultValue: '设计确认' })}
            </h2>
            <p className="mt-0.5 truncate text-xs text-neutral-500 dark:text-zinc-400">
              {taskTitle ? `${taskTitle} · ` : ''}
              {t('designGate.subtitle', { defaultValue: '先选择设计来源，未做选择前不会触发生成。' })}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
            aria-label="close"
          >
            <X className="size-4" />
          </button>
        </div>

        {!pickingExisting ? (
          <div className="space-y-4 px-5 py-4">
            <div className="flex gap-3" role="radiogroup" aria-label={t('designGate.title', { defaultValue: '设计确认' })}>
              <DesignChoiceCard
                value="existing"
                title={t('designGate.choice.existing', { defaultValue: '选择已有设计' })}
                description={t('designGate.choice.existingDesc', { defaultValue: '从已有项目列表中选择，设计已在 OpenDesign 完成。' })}
                selected={selected === 'existing'}
                onSelect={handleSelect}
              />
              <DesignChoiceCard
                value="generate"
                title={t('designGate.choice.generate', { defaultValue: '从需求生成' })}
                description={t('designGate.choice.generateDesc', { defaultValue: '由平台调用 OD 基于上游需求文档自动设计出首版，可在弹窗内对话修改与版本恢复。' })}
                recommended
                selected={selected === 'generate'}
                onSelect={handleSelect}
              />
            </div>
            {error && <p className="text-sm text-red-600 dark:text-red-400">{error}</p>}
          </div>
        ) : (
          <div className="space-y-3 px-5 py-4">
            <button
              type="button"
              onClick={() => setPickingExisting(false)}
              className="inline-flex items-center gap-1 text-xs font-medium text-neutral-500 hover:text-sky-600 dark:text-zinc-400 dark:hover:text-sky-400"
            >
              <ArrowLeft className="size-3.5" />
              {t('designGate.backToSource', { defaultValue: '返回' })}
            </button>
            {listBusy ? (
              <p className="flex items-center gap-2 py-6 text-sm text-neutral-500 dark:text-zinc-400">
                <Loader2 className="size-4 animate-spin" />
                {t('api.loading', { defaultValue: '加载中…' })}
              </p>
            ) : listErr ? (
              <p className="text-sm text-red-600 dark:text-red-400">{listErr}</p>
            ) : (projects ?? []).length === 0 ? (
              <p className="py-6 text-sm text-neutral-500 dark:text-zinc-400">
                {t('designGate.noExisting', { defaultValue: 'OpenDesign 中还没有可用项目，可改用「从需求生成」。' })}
              </p>
            ) : (
              <ul className="max-h-64 space-y-1.5 overflow-y-auto pr-1">
                {(projects ?? []).map((p) => (
                  <li key={p.id}>
                    <button
                      type="button"
                      onClick={() => setPicked(p)}
                      className={cn(
                        'w-full rounded-lg border px-3 py-2 text-left transition-colors',
                        picked?.id === p.id
                          ? 'border-sky-500 bg-sky-50/70 dark:border-sky-500 dark:bg-sky-950/40'
                          : 'border-neutral-200 hover:border-neutral-300 dark:border-zinc-700 dark:hover:border-zinc-600',
                      )}
                    >
                      <span className="block truncate text-sm font-medium text-neutral-900 dark:text-zinc-100">{p.name || p.id}</span>
                      <span className="mt-0.5 flex items-center gap-2 text-[11px] text-neutral-400 dark:text-zinc-500">
                        <span className="font-mono">{p.id}</span>
                        {p.designSystemId && <span>· {p.designSystemId}</span>}
                        {p.updatedAt && <span>· {p.updatedAt}</span>}
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
            {error && <p className="text-sm text-red-600 dark:text-red-400">{error}</p>}
          </div>
        )}

        <div className="flex items-center justify-end gap-2 border-t border-neutral-100 px-5 py-3 dark:border-zinc-800">
          <button
            type="button"
            onClick={onClose}
            disabled={busy}
            className="rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm font-medium text-neutral-600 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
          >
            {t('common.cancel', { defaultValue: '取消' })}
          </button>
          <button
            type="button"
            onClick={handleNext}
            disabled={busy || (pickingExisting && !picked)}
            className={cn(
              'rounded-lg px-3 py-2 text-sm font-semibold text-white transition-colors disabled:opacity-50',
              pickingExisting || selected === 'generate' ? 'bg-sky-600 hover:bg-sky-700' : 'bg-neutral-400 dark:bg-zinc-700',
            )}
          >
            {busy ? t('forms.working', { defaultValue: '处理中…' }) : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
