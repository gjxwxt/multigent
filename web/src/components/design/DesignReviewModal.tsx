import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ExternalLink, Loader2, RefreshCw, Send, X } from 'lucide-react'
import { apiFetch, apiPost } from '../../lib/api'
import { cn } from '../../lib/cn'
import { overlayDismissProps } from '../ui/overlay'
import { DESIGN_EMBED_MODE } from './mode'

export type DesignRunStatus = 'none' | 'generating' | 'done' | 'error' | string

type DesignChatResponse = {
  conversationId?: string
  answer?: string
  status?: number
}

/**
 * Stage-two modal of the design gate (docs §5.2/§8.2): the OD Studio iframe
 * (Plan A) or a new-tab guide card (Plan B), a run-status badge polling
 * design/status, a chat bar that forwards messages through the platform, and
 * the confirm / rework action bar. Embedding mode comes from the module
 * constant, never from props.
 */
export function DesignReviewModal({
  project,
  taskID,
  taskTitle,
  projectId,
  proxyUrl,
  launchUrl,
  conversationId,
  onConversationId,
  onConfirm,
  onRework,
  onClose,
}: {
  project: string
  taskID: string
  taskTitle?: string
  projectId: string
  proxyUrl: string
  launchUrl: string
  conversationId?: string
  onConversationId?: (id: string) => void
  onConfirm: () => void
  onRework: () => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [runStatus, setRunStatus] = useState<DesignRunStatus>('generating')
  const [confirmBusy, setConfirmBusy] = useState(false)
  const [chatBusy, setChatBusy] = useState(false)
  const [chatErr, setChatErr] = useState<string | null>(null)
  const [chatDraft, setChatDraft] = useState('')
  const [frameNonce, setFrameNonce] = useState(0)
  const convRef = useRef(conversationId || '')
  convRef.current = conversationId || ''

  // Run-status polling: 10s per docs §6.5. Best-effort: failures leave the
  // last badge state until the next tick.
  useEffect(() => {
    let alive = true
    const timer = window.setInterval(async () => {
      try {
        const data = await apiFetch<{ runStatus?: DesignRunStatus }>(
          `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/status`,
          { silentStatuses: [401, 403, 404, 502] },
        )
        if (alive && data?.runStatus) setRunStatus(data.runStatus)
      } catch {
        /* polling is best-effort */
      }
    }, 10000)
    return () => {
      alive = false
      window.clearInterval(timer)
    }
  }, [project, taskID])

  async function sendChat() {
    const message = chatDraft.trim()
    if (!message || chatBusy) return
    setChatBusy(true)
    setChatErr(null)
    try {
      const data = await apiPost<DesignChatResponse>(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/chat`,
        { conversationId: convRef.current || undefined, message },
        { suppressToast: true },
      )
      if (data?.conversationId) {
        convRef.current = data.conversationId
        onConversationId?.(data.conversationId)
      }
      setChatDraft('')
    } catch (e) {
      setChatErr(e instanceof Error ? e.message : String(e))
    } finally {
      setChatBusy(false)
    }
  }

  const statusBadge = (() => {
    if (runStatus === 'done') {
      return { cls: 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-950/40 dark:text-emerald-300 dark:border-emerald-800', label: t('designGate.status.done', { defaultValue: '已完成' }), pulse: false }
    }
    if (runStatus === 'error') {
      return { cls: 'bg-red-50 text-red-700 border-red-200 dark:bg-red-950/40 dark:text-red-300 dark:border-red-800', label: t('designGate.status.error', { defaultValue: '生成失败' }), pulse: false }
    }
    if (runStatus === 'none') {
      return { cls: 'bg-neutral-100 text-neutral-600 border-neutral-200 dark:bg-zinc-800 dark:text-zinc-400 dark:border-zinc-700', label: t('designGate.status.idle', { defaultValue: '未开始' }), pulse: false }
    }
    return { cls: 'bg-sky-50 text-sky-700 border-sky-200 dark:bg-sky-950/40 dark:text-sky-300 dark:border-sky-800', label: t('designGate.status.generating', { defaultValue: '生成中…' }), pulse: true }
  })()

  function confirm() {
    if (confirmBusy) return
    setConfirmBusy(true)
    try {
      onConfirm()
    } finally {
      setConfirmBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/45 p-4 backdrop-blur-[1px]" role="presentation" {...overlayDismissProps(onClose)}>
      <div
        className="flex max-h-[94vh] w-full max-w-6xl animate-scale-in flex-col overflow-hidden rounded-xl border border-neutral-200 bg-white shadow-2xl dark:border-zinc-700 dark:bg-zinc-900"
        role="dialog"
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Top bar: title, run badge, mode badge, close */}
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
            <span className="hidden rounded-full border border-neutral-200 bg-neutral-50 px-2 py-0.5 font-mono text-[10px] text-neutral-500 sm:inline dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-400">
              {DESIGN_EMBED_MODE === 'iframe' ? 'Plan A · iframe' : 'Plan B · 外部标签页'}
            </span>
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
          <div className="min-h-0 flex-1 bg-neutral-100 dark:bg-zinc-950">
            <iframe
              key={frameNonce}
              src={proxyUrl}
              title={t('designGate.title', { defaultValue: '设计确认' })}
              className="size-full border-0"
              allow="clipboard-write"
            />
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
                onClick={() => window.open(launchUrl, '_blank', 'noopener')}
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

        {/* Chat bar (platform-forwarded multi-turn editing) */}
        <div className="shrink-0 border-t border-neutral-100 px-4 py-2.5 dark:border-zinc-800">
          {chatErr && <p className="mb-1.5 text-xs text-red-600 dark:text-red-400">{chatErr}</p>}
          <div className="flex items-center gap-2">
            <input
              value={chatDraft}
              onChange={(e) => setChatDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.nativeEvent.isComposing) void sendChat()
              }}
              placeholder={t('designGate.chat.placeholder', { defaultValue: '描述要修改的地方，由平台转发给设计助手…' })}
              disabled={chatBusy}
              className="min-w-0 flex-1 rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm text-neutral-900 outline-none focus:border-sky-400 disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-100"
            />
            <button
              type="button"
              onClick={() => void sendChat()}
              disabled={chatBusy || !chatDraft.trim()}
              className="inline-flex shrink-0 items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white hover:bg-sky-700 disabled:opacity-50"
            >
              {chatBusy ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4" />}
              {t('designGate.chat.send', { defaultValue: '发送' })}
            </button>
            {DESIGN_EMBED_MODE === 'iframe' && (
              <button
                type="button"
                onClick={() => setFrameNonce((n) => n + 1)}
                title={t('designGate.chat.reload', { defaultValue: '重新加载设计画布' })}
                className="shrink-0 rounded-lg border border-neutral-300 p-2 text-neutral-500 hover:bg-neutral-50 dark:border-zinc-600 dark:text-zinc-400 dark:hover:bg-zinc-800"
              >
                <RefreshCw className="size-4" />
              </button>
            )}
          </div>
        </div>

        {/* Bottom action bar: rework left, confirm right */}
        <div className="flex shrink-0 items-center justify-between border-t border-neutral-100 px-5 py-3 dark:border-zinc-800">
          <button
            type="button"
            onClick={onRework}
            disabled={confirmBusy}
            className="rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm font-medium text-neutral-600 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
          >
            {t('designGate.back', { defaultValue: '打回需求' })}
          </button>
          <button
            type="button"
            onClick={confirm}
            disabled={confirmBusy}
            className="rounded-lg bg-sky-600 px-4 py-2 text-sm font-semibold text-white hover:bg-sky-700 disabled:opacity-50"
          >
            {confirmBusy ? t('forms.working', { defaultValue: '处理中…' }) : t('designGate.confirm', { defaultValue: '确认并流转' })}
          </button>
        </div>
      </div>
    </div>
  )
}
