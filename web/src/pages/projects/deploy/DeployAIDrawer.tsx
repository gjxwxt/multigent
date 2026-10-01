// DeployAIDrawer：部署中心二期 M4——部署页专属的右下角 AI 排查抽屉。
// 硬边界：纯对话无工具（不碰代码/写锁，与预览 Copilot 的本质区别）。
// 助手每轮回答可携带 FINDINGS 结构化发现项（现象/预期/复现路径），服务端解析后
// 以复选列表可视化；勾选发现项 → POST /deploy/ai/tasks（created_by: deploy-ai）。
// 对话状态只存 localStorage（服务端无会话）；模型 = 项目 agent 的模型账号，
// 默认 glm-5.3-flash，输出 16k 硬封顶（OD 短对话红线）。
import { useCallback, useEffect, useRef, useState, type KeyboardEvent as RKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import {
  Bot,
  ChevronDown,
  ListChecks,
  Maximize2,
  Minimize2,
  Send,
  Sparkles,
  Trash2,
  X,
} from 'lucide-react'
import { apiFetch, apiPost } from '../../../lib/api'
import { showToast } from '../../../components/ui/Toast'
import { cn } from '../../../lib/cn'

export type DeployAIFinding = {
  id: string
  title: string
  symptom: string
  expectation?: string
  repro?: string
  severity?: string
}

type DeployAIMessage = {
  id: string
  role: 'user' | 'assistant'
  content: string
  findings?: DeployAIFinding[]
  error?: string
  isThinking?: boolean
  isDone?: boolean
}

type DeployAIContext = {
  project: string
  model: string
  deployPort?: number
  lastDeploy?: string
  lastDeployBy?: string
  healthStatus?: string
  deployConfigured?: boolean
}

type DeployAIDrawerProps = {
  project: string
  open: boolean
  onClose: () => void
  onTaskCreated?: () => void
}

const storageKey = (project: string) => `multigent_deploy_ai_chat_${project}`

function severityCls(severity?: string): string {
  switch ((severity || '').toLowerCase()) {
    case 'high':
      return 'bg-red-50 text-red-700 border-red-200 dark:bg-red-950/50 dark:text-red-300 dark:border-red-800'
    case 'medium':
      return 'bg-amber-50 text-amber-700 border-amber-200 dark:bg-amber-950/50 dark:text-amber-300 dark:border-amber-800'
    case 'low':
      return 'bg-neutral-100 text-neutral-600 border-neutral-200 dark:bg-zinc-800 dark:text-zinc-400 dark:border-zinc-700'
    default:
      return 'bg-sky-50 text-sky-700 border-sky-200 dark:bg-sky-950/50 dark:text-sky-300 dark:border-sky-800'
  }
}

function severityLabel(severity: string | undefined, t: (k: string, dv: string) => string): string {
  switch ((severity || '').toLowerCase()) {
    case 'high':
      return t('projectDeploy.aiSeverityHigh', '高')
    case 'medium':
      return t('projectDeploy.aiSeverityMedium', '中')
    case 'low':
      return t('projectDeploy.aiSeverityLow', '低')
    default:
      return t('projectDeploy.aiSeverityUnset', '未定级')
  }
}

export function DeployAIDrawer({ project, open, onClose, onTaskCreated }: DeployAIDrawerProps) {
  const { t } = useTranslation()
  const [isMaximized, setIsMaximized] = useState(false)
  const [minimized, setMinimized] = useState(false)
  const [input, setInput] = useState('')
  const [isStreaming, setIsStreaming] = useState(false)
  const [ctx, setCtx] = useState<DeployAIContext | null>(null)
  const [checked, setChecked] = useState<Record<string, boolean>>({})
  const [creating, setCreating] = useState(false)
  const [msgs, setMsgs] = useState<DeployAIMessage[]>(() => {
    try {
      const saved = localStorage.getItem(storageKey(project))
      if (saved) {
        const parsed = JSON.parse(saved)
        if (Array.isArray(parsed)) return parsed
      }
    } catch {}
    return []
  })

  const abortRef = useRef<AbortController | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Persist per project; clear checked state when history changes shape.
  useEffect(() => {
    try {
      localStorage.setItem(storageKey(project), JSON.stringify(msgs))
    } catch {}
  }, [msgs, project])

  useEffect(() => {
    if (!open || !project) return
    let cancelled = false
    apiFetch<DeployAIContext>(`/api/v1/projects/${encodeURIComponent(project)}/deploy/ai/context`, { suppressToast: true })
      .then((c) => {
        if (!cancelled) setCtx(c)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open, project])

  useEffect(() => {
    if (open && !minimized && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [msgs, open, minimized])

  const handleClearHistory = () => {
    setMsgs([])
    setChecked({})
    try {
      localStorage.removeItem(storageKey(project))
    } catch {}
  }

  const handleSend = async () => {
    const text = input.trim()
    if (!text || isStreaming) return
    setInput('')
    const userMsg: DeployAIMessage = { id: `m-${Date.now()}-u`, role: 'user', content: text }
    const assistantMsg: DeployAIMessage = {
      id: `m-${Date.now()}-a`,
      role: 'assistant',
      content: '',
      isThinking: true,
      isDone: false,
    }
    setMsgs((prev) => [...prev, userMsg, assistantMsg])
    setIsStreaming(true)
    const controller = new AbortController()
    abortRef.current = controller

    try {
      const history = msgs
        .filter((m) => !m.isThinking && !m.error)
        .slice(-12)
        .map((m) => ({ role: m.role, content: m.content }))
      const res = await apiPost<{ reply: string; findings: DeployAIFinding[] }>(
        `/api/v1/projects/${encodeURIComponent(project)}/deploy/ai/chat`,
        { message: text, history },
        // 抽屉内自行渲染错误气泡，不走全局 toast。
        { suppressToast: true },
      )
      setMsgs((prev) =>
        prev.map((m) =>
          m.id === assistantMsg.id
            ? {
                ...m,
                content: res.reply,
                findings: res.findings?.length ? res.findings : undefined,
                isThinking: false,
                isDone: true,
              }
            : m,
        ),
      )
    } catch (err) {
      if ((err as Error).name !== 'AbortError') {
        const errMsg = err instanceof Error ? err.message : String(err)
        setMsgs((prev) =>
          prev.map((m) =>
            m.id === assistantMsg.id
              ? { ...m, isThinking: false, isDone: true, error: errMsg, content: m.content || '' }
              : m,
          ),
        )
      }
    } finally {
      setIsStreaming(false)
      abortRef.current = null
    }
  }

  const handleStop = () => {
    abortRef.current?.abort()
    abortRef.current = null
    setIsStreaming(false)
    setMsgs((prev) =>
      prev.map((m, i) =>
        i === prev.length - 1 && m.role === 'assistant' && !m.isDone
          ? { ...m, isThinking: false, isDone: true, content: m.content || '' }
          : m,
      ),
    )
  }

  const checkedFindings = useCallback(() => {
    const out: { finding: DeployAIFinding; msgIndex: number }[] = []
    msgs.forEach((m, msgIndex) => {
      ;(m.findings ?? []).forEach((f) => {
        if (checked[`${m.id}:${f.id}`]) out.push({ finding: f, msgIndex })
      })
    })
    return out
  }, [msgs, checked])

  const handleCreateTask = async () => {
    const picked = checkedFindings()
    if (!picked.length || creating) return
    setCreating(true)
    try {
      const severity = picked.map((p) => (p.finding.severity || '').toLowerCase())
      const top = severity.includes('high') ? 'high' : severity.includes('medium') ? 'medium' : severity.includes('low') ? 'low' : ''
      const summary = msgs
        .filter((m) => !m.isThinking && !m.error && m.content)
        .slice(-6)
        .map((m) => `${m.role === 'user' ? '用户' : '助手'}: ${m.content}`)
        .join('\n')
        .slice(0, 4000)
      await apiPost<{ id: string }>(
        `/api/v1/projects/${encodeURIComponent(project)}/deploy/ai/tasks`,
        {
          title: picked.length === 1 ? picked[0].finding.title : undefined,
          findings: picked.map((p) => p.finding),
          conversationSummary: summary,
          severity: top,
        },
      )
      showToast(t('projectDeploy.aiTaskCreated', { defaultValue: '任务已创建（created_by deploy-ai）' }), 'success')
      setChecked({})
      onTaskCreated?.()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setCreating(false)
    }
  }

  const handleKeyDown = (e: RKeyboardEvent<HTMLTextAreaElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      void handleSend()
    }
  }

  if (!open) return null

  const pickedCount = checkedFindings().length

  const renderMessage = (m: DeployAIMessage) => {
    if (m.role === 'user') {
      return (
        <div key={m.id} className="flex justify-end">
          <div className="max-w-[85%] rounded-2xl rounded-br-md bg-sky-600 px-3 py-2 text-xs leading-relaxed text-white">
            {m.content}
          </div>
        </div>
      )
    }
    return (
      <div key={m.id} className="flex flex-col gap-1.5">
        <div className="flex justify-start">
          <div
            className={cn(
              'max-w-[92%] rounded-2xl rounded-bl-md border px-3 py-2 text-xs leading-relaxed',
              m.error
                ? 'border-red-200 bg-red-50 text-red-700 dark:border-red-800 dark:bg-red-950/50 dark:text-red-300'
                : 'border-neutral-200 bg-white text-neutral-700 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200',
            )}
          >
            {m.isThinking ? (
              <span className="inline-flex items-center gap-1.5 text-neutral-400 dark:text-zinc-500">
                <Sparkles className="size-3 animate-pulse" />
                {t('projectDeploy.aiThinking', { defaultValue: '正在分析…' })}
              </span>
            ) : m.error ? (
              <span>❌ {m.error}</span>
            ) : (
              <span className="whitespace-pre-wrap">{m.content}</span>
            )}
          </div>
        </div>
        {!m.isThinking && (m.findings?.length ?? 0) > 0 && (
          <div className="ml-1 space-y-1.5">
            <p className="inline-flex items-center gap-1 text-[10px] font-semibold uppercase tracking-wide text-neutral-400 dark:text-zinc-500">
              <ListChecks className="size-3" />
              {t('projectDeploy.aiFindings', { defaultValue: '发现项' })}
            </p>
            {m.findings!.map((f) => {
              const key = `${m.id}:${f.id}`
              return (
                <label
                  key={key}
                  className={cn(
                    'flex cursor-pointer items-start gap-2 rounded-lg border p-2 transition-colors',
                    checked[key]
                      ? 'border-sky-300 bg-sky-50/70 dark:border-sky-700 dark:bg-sky-950/40'
                      : 'border-neutral-200 bg-white hover:border-neutral-300 dark:border-zinc-700 dark:bg-zinc-900/70 dark:hover:border-zinc-600',
                  )}
                >
                  <input
                    type="checkbox"
                    className="mt-0.5 size-3.5 shrink-0 accent-sky-600"
                    checked={Boolean(checked[key])}
                    onChange={() =>
                      setChecked((prev) => ({ ...prev, [key]: !prev[key] }))
                    }
                  />
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-1.5">
                      <span className="truncate text-xs font-medium text-neutral-800 dark:text-zinc-100">{f.title}</span>
                      <span className={cn('shrink-0 rounded-full border px-1.5 py-px text-[10px] font-medium', severityCls(f.severity))}>
                        {severityLabel(f.severity, t)}
                      </span>
                    </span>
                    {f.symptom && (
                      <span className="mt-0.5 block text-[11px] leading-relaxed text-neutral-500 dark:text-zinc-400">
                        {t('projectDeploy.aiSymptom', { defaultValue: '现象' })}：{f.symptom}
                      </span>
                    )}
                    {f.expectation && (
                      <span className="block text-[11px] leading-relaxed text-neutral-500 dark:text-zinc-400">
                        {t('projectDeploy.aiExpectation', { defaultValue: '预期' })}：{f.expectation}
                      </span>
                    )}
                    {f.repro && (
                      <span className="block font-mono text-[10px] leading-relaxed text-neutral-400 dark:text-zinc-500">
                        {t('projectDeploy.aiRepro', { defaultValue: '复现' })}：{f.repro}
                      </span>
                    )}
                  </span>
                </label>
              )
            })}
          </div>
        )}
      </div>
    )
  }

  return createPortal(
    <div
      className={cn(
        'fixed z-50 flex flex-col overflow-hidden rounded-xl border border-neutral-200 bg-neutral-50 shadow-2xl dark:border-zinc-700 dark:bg-zinc-900',
        isMaximized
          ? 'bottom-4 left-1/2 right-4 top-16 -translate-x-1/2'
          : 'bottom-6 right-6 h-[560px] w-[420px]',
        minimized && 'h-11',
      )}
    >
      {/* Header */}
      <div className="flex shrink-0 items-center justify-between border-b border-neutral-200 bg-white px-3 py-2 dark:border-zinc-700 dark:bg-zinc-900">
        <div className="flex min-w-0 items-center gap-2">
          <span className="flex size-6 shrink-0 items-center justify-center rounded-lg bg-sky-600/10">
            <Bot className="size-3.5 text-sky-600 dark:text-sky-400" />
          </span>
          <div className="min-w-0">
            <p className="truncate text-xs font-semibold text-neutral-800 dark:text-zinc-100">
              {t('projectDeploy.aiTitle', { defaultValue: '部署助手' })}
            </p>
            <p className="truncate text-[10px] text-neutral-400 dark:text-zinc-500">
              {ctx
                ? `${ctx.model}${ctx.lastDeploy ? ` · ${ctx.lastDeploy}` : ''}`
                : t('projectDeploy.aiSubtitle', { defaultValue: '纯对话排查 · 不执行任何操作' })}
            </p>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-0.5">
          <button
            type="button"
            title={t('projectDeploy.aiClearHistory', { defaultValue: '清空对话' })}
            onClick={handleClearHistory}
            className="rounded p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
          >
            <Trash2 className="size-3.5" />
          </button>
          <button
            type="button"
            title={minimized ? t('projectDeploy.aiRestore', { defaultValue: '展开' }) : t('projectDeploy.aiMinimize', { defaultValue: '收起' })}
            onClick={() => setMinimized((v) => !v)}
            className="rounded p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
          >
            <ChevronDown className={cn('size-3.5 transition-transform', minimized && 'rotate-180')} />
          </button>
          <button
            type="button"
            title={isMaximized ? t('projectDeploy.aiUnmaximize', { defaultValue: '还原' }) : t('projectDeploy.aiMaximize', { defaultValue: '放大' })}
            onClick={() => setIsMaximized((v) => !v)}
            className="rounded p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
          >
            {isMaximized ? <Minimize2 className="size-3.5" /> : <Maximize2 className="size-3.5" />}
          </button>
          <button
            type="button"
            title={t('projectDeploy.aiClose', { defaultValue: '关闭' })}
            onClick={onClose}
            className="rounded p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
          >
            <X className="size-3.5" />
          </button>
        </div>
      </div>

      {!minimized && (
        <>
          {/* Messages */}
          <div ref={scrollRef} className="flex-1 space-y-3 overflow-y-auto px-3 py-3">
            {msgs.length === 0 && (
              <div className="mt-6 space-y-2 text-center">
                <Sparkles className="mx-auto size-5 text-sky-500" />
                <p className="text-xs font-medium text-neutral-600 dark:text-zinc-300">
                  {t('projectDeploy.aiEmptyTitle', { defaultValue: '描述你遇到的部署问题' })}
                </p>
                <p className="mx-auto max-w-[280px] text-[11px] leading-relaxed text-neutral-400 dark:text-zinc-500">
                  {t('projectDeploy.aiEmptyBody', {
                    defaultValue: '例如：登录接口返回 502、页面空白、发布后健康探针异常。助手只会对话分析，不会改动代码或配置；发现的问题会整理成发现项，勾选后可一键转为任务。',
                  })}
                </p>
              </div>
            )}
            {msgs.map(renderMessage)}
          </div>

          {/* Findings → task action bar */}
          {pickedCount > 0 && (
            <div className="shrink-0 border-t border-neutral-200 bg-white px-3 py-2 dark:border-zinc-700 dark:bg-zinc-900">
              <button
                type="button"
                onClick={() => void handleCreateTask()}
                disabled={creating}
                className="inline-flex w-full items-center justify-center gap-1.5 rounded-lg bg-sky-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-sky-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                <ListChecks className="size-3.5" />
                {creating
                  ? t('projectDeploy.aiCreating', { defaultValue: '创建中…' })
                  : t('projectDeploy.aiCreateTask', { defaultValue: '转为任务（{{count}}）', count: pickedCount })}
              </button>
            </div>
          )}

          {/* Input */}
          <div className="shrink-0 border-t border-neutral-200 bg-white px-3 py-2 dark:border-zinc-700 dark:bg-zinc-900">
            <div className="flex items-end gap-2">
              <textarea
                ref={textareaRef}
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                rows={2}
                placeholder={t('projectDeploy.aiInputPlaceholder', { defaultValue: '描述现象、预期与复现路径…' })}
                className="max-h-28 flex-1 resize-none rounded-lg border border-neutral-200 bg-neutral-50 px-2.5 py-1.5 text-xs text-neutral-700 placeholder:text-neutral-400 focus:border-sky-400 focus:outline-none dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200 dark:placeholder:text-zinc-500"
              />
              {isStreaming ? (
                <button
                  type="button"
                  onClick={handleStop}
                  className="shrink-0 rounded-lg border border-neutral-200 p-2 text-neutral-500 hover:bg-neutral-100 dark:border-zinc-700 dark:hover:bg-zinc-800"
                  title={t('projectDeploy.aiStop', { defaultValue: '停止' })}
                >
                  <span className="block size-3 rounded-sm bg-neutral-500 dark:bg-zinc-400" />
                </button>
              ) : (
                <button
                  type="button"
                  onClick={() => void handleSend()}
                  disabled={!input.trim()}
                  className="shrink-0 rounded-lg bg-sky-600 p-2 text-white hover:bg-sky-700 disabled:cursor-not-allowed disabled:opacity-50"
                  title={t('projectDeploy.aiSend', { defaultValue: '发送' })}
                >
                  <Send className="size-3.5" />
                </button>
              )}
            </div>
            <p className="mt-1 text-[10px] text-neutral-400 dark:text-zinc-600">
              {t('projectDeploy.aiHint', { defaultValue: 'Cmd/Ctrl+Enter 发送 · 对话保存在本地浏览器' })}
            </p>
          </div>
        </>
      )}
    </div>,
    document.body,
  )
}
