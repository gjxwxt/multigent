import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { ExternalLink, Globe, Info, Lock, Maximize2, Minimize2, RotateCw, Sparkles, X } from 'lucide-react'
import { cn } from '../../lib/cn'

export type PreviewDrawerProps = {
  project: string
  taskId: string
  taskTitle: string
  previewUrl: string
  previewToken?: string
  canOperator: boolean
  onClose: () => void
  onOpenChangeRun?: () => void
}

export function PreviewDrawer({
  project: _project,
  taskId,
  taskTitle,
  previewUrl,
  previewToken,
  canOperator: _canOperator,
  onClose,
  onOpenChangeRun,
}: PreviewDrawerProps) {
  const { t } = useTranslation()
  const [iframeKey, setIframeKey] = useState(0)
  const [sidebarOpen, setSidebarOpen] = useState(true)
  const [isMaximized, setIsMaximized] = useState(false)
  const iframeRef = useRef<HTMLIFrameElement | null>(null)

  // Escape key to close modal
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        onClose()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  // Construct target preview URL (exchanging preview token if present)
  const targetUrl = useMemo(() => {
    if (!previewUrl) return ''
    if (!previewToken) return previewUrl
    const sep = previewUrl.includes('?') ? '&' : '?'
    return `${previewUrl}${sep}pvt=${encodeURIComponent(previewToken)}`
  }, [previewUrl, previewToken])

  const handleReload = () => {
    setIframeKey((k) => k + 1)
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-2 backdrop-blur-xs sm:p-4">
      <div
        className={cn(
          'relative flex flex-col overflow-hidden rounded-2xl border border-neutral-200 bg-white shadow-2xl transition-all duration-200 dark:border-zinc-700 dark:bg-zinc-900',
          isMaximized ? 'h-full w-full rounded-none' : 'h-[92vh] w-[96vw] max-w-[1720px]'
        )}
      >
        {/* Top bar */}
        <div className="flex h-12 shrink-0 items-center justify-between border-b border-neutral-200 bg-neutral-50/90 px-4 dark:border-zinc-800 dark:bg-zinc-900/90">
          <div className="flex items-center gap-3 overflow-hidden">
            <span className="flex size-7 items-center justify-center rounded-lg bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-400">
              <Globe className="size-4" />
            </span>
            <div className="flex min-w-0 items-center gap-2">
              <h2 className="truncate text-sm font-semibold text-neutral-900 dark:text-zinc-100">
                {t('tasks.previewDrawer.title', { defaultValue: '实时预览与调优抽屉' })}
              </h2>
              <span className="rounded-md bg-neutral-200/80 px-2 py-0.5 text-[11px] font-mono text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400">
                {taskId}
              </span>
              <span className="hidden truncate text-xs text-neutral-400 dark:text-zinc-500 md:inline">
                {taskTitle}
              </span>
            </div>
          </div>

          <div className="flex items-center gap-1.5">
            <button
              type="button"
              onClick={handleReload}
              title={t('tasks.previewDrawer.reload', { defaultValue: '刷新预览页面' })}
              className="flex size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
            >
              <RotateCw className="size-4" />
            </button>
            <a
              href={targetUrl}
              target="_blank"
              rel="noopener noreferrer"
              title={t('tasks.previewDrawer.openExternal', { defaultValue: '在新标签页打开 ↗' })}
              className="flex size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
            >
              <ExternalLink className="size-4" />
            </a>
            <button
              type="button"
              onClick={() => setSidebarOpen((v) => !v)}
              title={t('tasks.previewDrawer.toggleCopilot', { defaultValue: '展开/收起调优助手' })}
              className={cn(
                'flex items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-colors',
                sidebarOpen
                  ? 'bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-300'
                  : 'text-neutral-600 hover:bg-neutral-200/60 dark:text-zinc-400 dark:hover:bg-zinc-800'
              )}
            >
              <Sparkles className="size-3.5" />
              <span>{t('tasks.previewDrawer.copilotBtn', { defaultValue: '调优助手' })}</span>
            </button>
            <button
              type="button"
              onClick={() => setIsMaximized((v) => !v)}
              className="hidden size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100 sm:flex"
            >
              {isMaximized ? <Minimize2 className="size-4" /> : <Maximize2 className="size-4" />}
            </button>
            <button
              type="button"
              onClick={onClose}
              className="flex size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
            >
              <X className="size-4" />
            </button>
          </div>
        </div>

        {/* Main Area: Iframe + Copilot Drawer */}
        <div className="relative flex min-h-0 flex-1 overflow-hidden">
          {/* Iframe Viewport */}
          <div className="relative flex-1 overflow-hidden bg-neutral-100 dark:bg-zinc-950">
            {targetUrl ? (
              <iframe
                key={iframeKey}
                ref={iframeRef}
                src={targetUrl}
                title={`preview-${taskId}`}
                className="size-full border-none"
                sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
              />
            ) : (
              <div className="flex size-full items-center justify-center text-sm text-neutral-400 dark:text-zinc-600">
                {t('tasks.previewDrawer.noUrl', { defaultValue: '无可用预览地址' })}
              </div>
            )}
          </div>

          {/* Copilot Drawer Panel (Slice A: Read-Only Priority) */}
          {sidebarOpen && (
            <div className="flex w-88 shrink-0 flex-col border-l border-neutral-200 bg-white dark:border-zinc-800 dark:bg-zinc-900 sm:w-96">
              <div className="flex h-11 items-center justify-between border-b border-neutral-100 px-4 dark:border-zinc-800">
                <div className="flex items-center gap-2">
                  <Sparkles className="size-4 text-sky-500" />
                  <span className="text-xs font-semibold text-neutral-800 dark:text-zinc-200">
                    {t('tasks.previewDrawer.copilotTitle', { defaultValue: '智能调优助手' })}
                  </span>
                </div>
                <span className="rounded-full bg-amber-100 px-2 py-0.5 text-[10px] font-medium text-amber-800 dark:bg-amber-950/60 dark:text-amber-300">
                  {t('tasks.previewDrawer.sliceABadge', { defaultValue: '只读模式 (Slice A)' })}
                </span>
              </div>

              {/* Chat & Feedback Body */}
              <div className="flex-1 space-y-4 overflow-y-auto p-4">
                {/* Slice A Notice Card */}
                <div className="rounded-xl border border-amber-200/80 bg-amber-50/60 p-3.5 text-xs text-amber-900 dark:border-amber-900/40 dark:bg-amber-950/30 dark:text-amber-200">
                  <div className="flex items-start gap-2">
                    <Info className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
                    <div className="space-y-1.5 leading-relaxed">
                      <p className="font-semibold">
                        {t('tasks.previewDrawer.turnLedgerDisabled', { defaultValue: '逐轮变更账本未启用' })}
                      </p>
                      <p className="text-[11px] text-amber-800/90 dark:text-amber-300/80">
                        {t('tasks.previewDrawer.turnLedgerDesc', {
                          defaultValue: '当前处于只读抽屉灰度阶段（Slice A）。逐轮代码修改与即时变更账本（Slice B）尚未开启，无法在此直接提交 AI 修改。',
                        })}
                      </p>
                      {onOpenChangeRun && (
                        <button
                          type="button"
                          onClick={() => {
                            onClose()
                            onOpenChangeRun()
                          }}
                          className="mt-1 inline-flex items-center gap-1 font-medium text-amber-700 underline decoration-amber-400 underline-offset-2 hover:text-amber-900 dark:text-amber-300 dark:hover:text-amber-100"
                        >
                          {t('tasks.previewDrawer.viewChangeRun', { defaultValue: '查看任务详情中的 Change Run 补丁 ↗' })}
                        </button>
                      )}
                    </div>
                  </div>
                </div>

                {/* Capability & Safety Card */}
                <div className="rounded-xl border border-neutral-200 bg-neutral-50/60 p-3 text-xs text-neutral-600 dark:border-zinc-800 dark:bg-zinc-800/40 dark:text-zinc-400">
                  <div className="flex items-center gap-2 font-medium text-neutral-800 dark:text-zinc-200">
                    <Lock className="size-3.5 text-neutral-500" />
                    <span>{t('tasks.previewDrawer.securityBoundary', { defaultValue: '安全与权限保护' })}</span>
                  </div>
                  <p className="mt-1.5 text-[11px] leading-relaxed">
                    {t('tasks.previewDrawer.securityBoundaryDesc', {
                      defaultValue: '预览页面共享访问凭据仅具备查看权限（preview.view）。所有代码变更均受 Change Run 状态机、隔离沙箱与操作者 RBAC 严格保护。',
                    })}
                  </p>
                </div>
              </div>

              {/* Bottom Input (Disabled in Slice A) */}
              <div className="border-t border-neutral-100 bg-neutral-50/50 p-3.5 dark:border-zinc-800 dark:bg-zinc-900/50">
                <div className="relative">
                  <textarea
                    disabled
                    rows={3}
                    placeholder={t('tasks.previewDrawer.inputDisabledPlaceholder', {
                      defaultValue: '只读抽屉模式：暂未开启抽屉内直接提交 AI 变更…',
                    })}
                    className="w-full resize-none rounded-xl border border-neutral-200 bg-neutral-100/70 p-2.5 text-xs text-neutral-400 outline-none cursor-not-allowed dark:border-zinc-800 dark:bg-zinc-800/50 dark:text-zinc-500"
                  />
                </div>
                <div className="mt-2 flex items-center justify-between text-[11px] text-neutral-400 dark:text-zinc-500">
                  <span>{t('tasks.previewDrawer.readOnlyHint', { defaultValue: '只读预览环境' })}</span>
                  <button
                    disabled
                    className="cursor-not-allowed rounded-lg bg-neutral-200 px-3 py-1 font-medium text-neutral-400 dark:bg-zinc-800 dark:text-zinc-600"
                  >
                    {t('tasks.previewDrawer.sendBtn', { defaultValue: '发送' })}
                  </button>
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>,
    document.body
  )
}
