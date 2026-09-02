import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { FolderGit2, Globe, Loader2, Trash2 } from 'lucide-react'
import { apiFetch, apiPost } from '../../lib/api'
import { cn } from '../../lib/cn'

type WorktreeResource = {
  exists: boolean
  branch?: string
  dirtyFiles?: number
  unpushedCommits?: number
  diskBytes?: number | null
  diskTimedOut?: boolean
}

type PreviewResource = {
  status: string
  port?: number
  expiresAt?: string
  readOnly?: boolean
}

type TaskResources = {
  worktree: WorktreeResource
  preview?: PreviewResource | null
  locked: boolean
  lockReason?: string
}

type Props = {
  taskId: string
  project: string
  /** Row-level hints from the list API decide whether the icon shows. */
  hasWorktree?: boolean
  hasPreview?: boolean
}

const POPOVER_WIDTH = 288 // w-72

function formatBytes(bytes: number, t: (k: string, o?: Record<string, unknown>) => string): string {
  if (bytes >= 1024 * 1024 * 1024) return t('tasks.resources.sizeGB', { value: (bytes / 1024 / 1024 / 1024).toFixed(1) })
  if (bytes >= 1024 * 1024) return t('tasks.resources.sizeMB', { value: (bytes / 1024 / 1024).toFixed(0) })
  return t('tasks.resources.sizeKB', { value: Math.max(1, Math.round(bytes / 1024)) })
}

export function TaskResourcesPopover({ taskId, project, hasWorktree, hasPreview }: Props) {
  const { t, i18n } = useTranslation()
  const [open, setOpen] = useState(false)
  const [resources, setResources] = useState<TaskResources | null>(null)
  const [loading, setLoading] = useState(false)
  const [cleaning, setCleaning] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [confirmCleanup, setConfirmCleanup] = useState(false)
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)
  const wrapRef = useRef<HTMLSpanElement>(null)
  const buttonRef = useRef<HTMLButtonElement>(null)
  const popRef = useRef<HTMLDivElement>(null)

  // The table wrapper is overflow-x-auto, which clips absolutely-positioned
  // children — the popover therefore renders through a portal with fixed
  // positioning and must be re-placed whenever its anchor moves (table
  // scroll, window resize).
  const place = () => {
    const btn = buttonRef.current
    if (!btn) return
    const rect = btn.getBoundingClientRect()
    const height = popRef.current?.offsetHeight ?? 180
    let left = rect.right - POPOVER_WIDTH
    if (left < 8) left = Math.min(rect.left, window.innerWidth - POPOVER_WIDTH - 8)
    if (left + POPOVER_WIDTH > window.innerWidth - 8) left = window.innerWidth - POPOVER_WIDTH - 8
    let top = rect.bottom + 8
    if (top + height > window.innerHeight - 8 && rect.top - height - 8 > 8) top = rect.top - height - 8
    setPos({ top: Math.max(8, top), left: Math.max(8, left) })
  }

  useEffect(() => {
    if (!open) return
    const onOutside = (e: MouseEvent) => {
      const target = e.target as Node
      if (wrapRef.current?.contains(target)) return
      // Portal content lives under document.body in the DOM; the React tree
      // still routes its synthetic events through the wrapping span, so the
      // native outside-click check must consult the popover node explicitly.
      if (popRef.current?.contains(target)) return
      setOpen(false)
    }
    document.addEventListener('mousedown', onOutside)
    return () => document.removeEventListener('mousedown', onOutside)
  }, [open])

  useLayoutEffect(() => {
    if (!open) return
    place()
    const reposition = () => place()
    window.addEventListener('resize', reposition)
    window.addEventListener('scroll', reposition, true)
    return () => {
      window.removeEventListener('resize', reposition)
      window.removeEventListener('scroll', reposition, true)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, resources, confirmCleanup, error, loading])

  const load = async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await apiFetch<TaskResources>(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/resources`,
        { suppressToast: true },
      )
      setResources(data)
    } catch {
      setError(t('tasks.resources.loadFailed'))
    } finally {
      setLoading(false)
    }
  }

  const toggle = async (e: React.MouseEvent) => {
    e.stopPropagation()
    const next = !open
    setOpen(next)
    setConfirmCleanup(false)
    if (next) await load()
  }

  const stopPreview = async (e: React.MouseEvent) => {
    e.stopPropagation()
    setCleaning(true)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/preview/stop`, {}, { suppressToast: true })
      await load()
    } catch {
      setError(t('tasks.resources.stopFailed'))
    } finally {
      setCleaning(false)
    }
  }

  const cleanupWorktree = async (e: React.MouseEvent) => {
    e.stopPropagation()
    setCleaning(true)
    setError(null)
    try {
      await apiPost(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/resources/worktree/cleanup`,
        {},
        { suppressToast: true },
      )
      setConfirmCleanup(false)
      await load()
    } catch {
      setError(t('tasks.resources.cleanupFailed'))
    } finally {
      setCleaning(false)
    }
  }

  if (!hasWorktree && !hasPreview) return null

  const wt = resources?.worktree
  const preview = resources?.preview
  const wtBusy = cleaning || loading
  const previewStatusCls =
    preview?.status === 'running'
      ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300'
      : preview?.status === 'error'
        ? 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
        : 'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300'
  const fmtDateTime = (v?: string) =>
    v
      ? new Date(v).toLocaleString(i18n.language === 'zh-CN' ? 'zh-CN' : 'en-US', { dateStyle: 'short', timeStyle: 'short' })
      : ''

  return (
    <span ref={wrapRef} className="relative inline-flex" onClick={(e) => e.stopPropagation()}>
      <button
        ref={buttonRef}
        type="button"
        onClick={toggle}
        className={cn(
          'rounded p-1 transition-colors',
          open
            ? 'bg-sky-100 text-sky-700 dark:bg-sky-900/40 dark:text-sky-300'
            : 'text-neutral-500 hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-zinc-300',
        )}
        title={t('tasks.resources.title')}
      >
        <FolderGit2 className="size-3.5" strokeWidth={1.8} />
      </button>
      {open && pos && createPortal(
        <div
          ref={popRef}
          className="fixed z-[95] w-72 rounded-lg border border-neutral-200 bg-white p-3 text-xs shadow-lg dark:border-zinc-700 dark:bg-zinc-900"
          style={{ top: pos.top, left: pos.left }}
        >
          {loading && !resources ? (
            <div className="flex items-center gap-2 py-2 text-neutral-500 dark:text-zinc-400">
              <Loader2 className="size-3.5 animate-spin" /> {t('tasks.resources.loading')}
            </div>
          ) : (
            <div className="space-y-3">
              {preview && (
                <div>
                  <div className="mb-1 flex items-center gap-1.5 font-semibold text-neutral-700 dark:text-zinc-200">
                    <Globe className="size-3.5" strokeWidth={1.8} /> {t('tasks.resources.preview')}
                    <span className={cn('ml-auto rounded px-1.5 py-0.5 text-[10px] font-medium', previewStatusCls)}>{preview.status}</span>
                  </div>
                  <div className="text-neutral-500 dark:text-zinc-400">
                    {preview.port ? <div>port {preview.port}</div> : null}
                    {preview.expiresAt ? <div>{t('tasks.resources.expiresAt', { time: fmtDateTime(preview.expiresAt) })}</div> : null}
                  </div>
                  <button
                    type="button"
                    onClick={stopPreview}
                    disabled={wtBusy}
                    className="mt-1 w-full rounded-md border border-amber-500/40 bg-amber-50 px-2 py-1 font-medium text-amber-700 transition hover:bg-amber-100 disabled:opacity-50 dark:border-amber-500/30 dark:bg-amber-950/40 dark:text-amber-300"
                  >
                    {t('tasks.resources.stopPreview')}
                  </button>
                </div>
              )}

              {wt?.exists ? (
                <div>
                  <div className="mb-1 flex items-center gap-1.5 font-semibold text-neutral-700 dark:text-zinc-200">
                    <FolderGit2 className="size-3.5" strokeWidth={1.8} /> {t('tasks.resources.worktree')}
                  </div>
                  <div className="space-y-0.5 text-neutral-500 dark:text-zinc-400">
                    {wt.branch ? <div>{t('tasks.resources.branch', { branch: wt.branch })}</div> : null}
                    {(wt.dirtyFiles ?? 0) > 0 ? (
                      <div className="text-amber-600 dark:text-amber-400">{t('tasks.resources.dirtyFiles', { count: wt.dirtyFiles })}</div>
                    ) : null}
                    {(wt.unpushedCommits ?? 0) > 0 ? (
                      <div className="text-amber-600 dark:text-amber-400">{t('tasks.resources.unpushedCommits', { count: wt.unpushedCommits })}</div>
                    ) : null}
                    {wt.diskTimedOut ? <div>{t('tasks.resources.sizeTimeout')}</div> : wt.diskBytes ? <div>{formatBytes(wt.diskBytes, t)}</div> : null}
                  </div>
                  {resources?.locked ? (
                    <div className="mt-1 rounded-md bg-neutral-100 px-2 py-1 text-[11px] text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                      {resources.lockReason || t('tasks.resources.lockedDefault')}
                    </div>
                  ) : confirmCleanup ? (
                    <div className="mt-1 space-y-1">
                      <div className="rounded-md bg-amber-50 px-2 py-1 text-[11px] text-amber-700 dark:bg-amber-950/40 dark:text-amber-300">
                        {(wt.dirtyFiles ?? 0) > 0 || (wt.unpushedCommits ?? 0) > 0
                          ? t('tasks.resources.cleanupConfirmDirty')
                          : t('tasks.resources.cleanupConfirm')}
                      </div>
                      <div className="flex gap-1">
                        <button
                          type="button"
                          onClick={cleanupWorktree}
                          disabled={wtBusy}
                          className="flex-1 rounded-md bg-red-600 px-2 py-1 font-medium text-white transition hover:bg-red-700 disabled:opacity-50"
                        >
                          {wtBusy ? <Loader2 className="mx-auto size-3.5 animate-spin" /> : t('tasks.resources.cleanupYes')}
                        </button>
                        <button
                          type="button"
                          onClick={(e) => {
                            e.stopPropagation()
                            setConfirmCleanup(false)
                          }}
                          className="rounded-md border border-neutral-200 px-2 py-1 text-neutral-600 transition hover:bg-neutral-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
                        >
                          {t('tasks.resources.cleanupNo')}
                        </button>
                      </div>
                    </div>
                  ) : (
                    <button
                      type="button"
                      onClick={(e) => {
                        e.stopPropagation()
                        setConfirmCleanup(true)
                      }}
                      disabled={wtBusy || resources?.locked}
                      title={resources?.lockReason || t('tasks.resources.cleanup')}
                      className="mt-1 flex w-full items-center justify-center gap-1 rounded-md border border-red-500/40 bg-red-50 px-2 py-1 font-medium text-red-700 transition hover:bg-red-100 disabled:cursor-not-allowed disabled:opacity-50 dark:border-red-500/30 dark:bg-red-950/40 dark:text-red-300"
                    >
                      <Trash2 className="size-3.5" strokeWidth={1.8} /> {t('tasks.resources.cleanup')}
                    </button>
                  )}
                </div>
              ) : (
                !preview && <div className="py-1 text-neutral-500 dark:text-zinc-400">{t('tasks.resources.nothing')}</div>
              )}

              {error && <div className="text-red-600 dark:text-red-400">{error}</div>}
            </div>
          )}
        </div>,
        document.body,
      )}
    </span>
  )
}
