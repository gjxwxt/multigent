import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as RKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {
  AlertCircle,
  ArrowLeft,
  Check,
  Code2,
  Crosshair,
  Database,
  ExternalLink,
  FileCode,
  Globe,
  History,
  Layers,
  Lock,
  Maximize2,
  MessageSquare,
  Minimize2,
  Minus,
  PanelRight,
  RotateCcw,
  RotateCw,
  Send,
  Sparkles,
  Square,
  Trash2,
  X,
} from 'lucide-react'
import { apiUrl } from '../../lib/api'
import { getStoredToken } from '../../lib/auth'
import { cn } from '../../lib/cn'
import { type DOMTarget, decodeDOMTarget } from '../../lib/domTarget'

export type { DOMTarget }
export { decodeDOMTarget }

export type FixtureScenario = {
  name: string
  description?: string
}

export type TaskSandboxStatus = {
  hasContract: boolean
  engine?: string
  storage?: string
  fixtureVersion?: string
  activeScenario?: string
  availableScenarios?: FixtureScenario[]
  leaseId?: string
  state?: string
  resetCount?: number
  expiresAt?: string
}

export type TurnReceipt = {
  id: string
  turnId: string
  status: string
  revision: number
  baselineTree?: string
  baselineCommit?: string
  requestDigest?: string
  redactedPrompt?: string
  creator?: string
  createdAt?: string
  updatedAt?: string
  displayDiff?: string
  touchedPaths?: string[]
  failureReason?: string
}

export type CopilotTool = {
  type: 'bash' | 'read' | 'edit' | 'search' | 'other'
  name: string
  target: string
}

export type PreviewSkillProfile = {
  id: string
  name: string
  description: string
  skillIds: string[]
}

export type CopilotMsg = {
  id: string
  role: 'user' | 'assistant'
  content: string
  profile?: string
  domTarget?: DOMTarget
  tools?: CopilotTool[]
  isThinking?: boolean
  isDone?: boolean
  isStopped?: boolean
  error?: string
}

export type PreviewDrawerProps = {
  project: string
  taskId: string
  taskTitle: string
  previewUrl: string
  previewToken?: string
  previewStatus?: string
  canOperator: boolean
  onClose: () => void
  onOpenChangeRun?: () => void
  onStartPreview?: () => Promise<void>
}

function diffLineClass(line: string): string {
  if (line.startsWith('diff --git ') || line.startsWith('Index: ')) {
    return 'bg-sky-50 text-sky-700 dark:bg-sky-950/30 dark:text-sky-300'
  }
  if (line.startsWith('@@ ')) {
    return 'bg-violet-50 text-violet-700 dark:bg-violet-950/30 dark:text-violet-300'
  }
  if (line.startsWith('+++') || line.startsWith('---')) {
    return 'bg-neutral-100 text-neutral-600 dark:bg-zinc-900 dark:text-zinc-400'
  }
  if (line.startsWith('+')) {
    return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950/30 dark:text-emerald-300'
  }
  if (line.startsWith('-')) {
    return 'bg-red-50 text-red-700 dark:bg-red-950/30 dark:text-red-300'
  }
  return 'text-neutral-700 dark:text-zinc-300'
}

function formatTurnTime(isoStr?: string): string {
  if (!isoStr) return ''
  try {
    const d = new Date(isoStr)
    if (isNaN(d.getTime())) return isoStr
    return d.toLocaleString(undefined, {
      month: 'numeric',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })
  } catch {
    return isoStr || ''
  }
}

function renderTurnStatusBadge(status: string) {
  switch (status) {
    case 'CAPTURED':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-emerald-50 px-2 py-0.5 text-[10px] font-semibold text-emerald-700 border border-emerald-200 dark:bg-emerald-950/50 dark:text-emerald-300 dark:border-emerald-800">
          <span className="size-1.5 rounded-full bg-emerald-500" />
          已捕获 (可撤销)
        </span>
      )
    case 'ROLLED_BACK':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-neutral-100 px-2 py-0.5 text-[10px] font-semibold text-neutral-600 border border-neutral-200 dark:bg-zinc-800 dark:text-zinc-400 dark:border-zinc-700">
          已撤销
        </span>
      )
    case 'SUPERSEDED':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-neutral-100 px-2 py-0.5 text-[10px] font-semibold text-neutral-500 border border-neutral-200 dark:bg-zinc-800 dark:text-zinc-500 dark:border-zinc-700">
          已覆盖
        </span>
      )
    case 'COMMITTING':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-sky-50 px-2 py-0.5 text-[10px] font-semibold text-sky-700 border border-sky-200 dark:bg-sky-950/50 dark:text-sky-300 dark:border-sky-800 animate-pulse">
          正在收编…
        </span>
      )
    case 'COMMITTED':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-indigo-50 px-2 py-0.5 text-[10px] font-semibold text-indigo-700 border border-indigo-200 dark:bg-indigo-950/50 dark:text-indigo-300 dark:border-indigo-800">
          已收编入库
        </span>
      )
    case 'PENDING':
    case 'EXECUTING':
    case 'CAPTURING':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-amber-50 px-2 py-0.5 text-[10px] font-semibold text-amber-700 border border-amber-200 dark:bg-amber-950/50 dark:text-amber-300 dark:border-amber-800 animate-pulse">
          <span className="size-1.5 rounded-full bg-amber-500 animate-ping" />
          {status === 'CAPTURING' ? '捕获核验中' : '智能体执行中'}
        </span>
      )
    case 'REVERTING':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-orange-50 px-2 py-0.5 text-[10px] font-semibold text-orange-700 border border-orange-200 dark:bg-orange-950/50 dark:text-orange-300 dark:border-orange-800 animate-pulse">
          正在回滚…
        </span>
      )
    case 'REVERT_FAILED':
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-red-100 px-2 py-0.5 text-[10px] font-semibold text-red-800 border border-red-300 dark:bg-red-950/60 dark:text-red-200 dark:border-red-800">
          回滚失败 (需人工排查)
        </span>
      )
    case 'FAILED':
    default:
      return (
        <span className="inline-flex items-center gap-1 rounded-full bg-red-50 px-2 py-0.5 text-[10px] font-semibold text-red-700 border border-red-200 dark:bg-red-950/50 dark:text-red-300 dark:border-red-800">
          执行失败
        </span>
      )
  }
}

function renderDiffContent(diffText: string) {
  const lines = diffText.split('\n')
  return (
    <div className="overflow-x-auto rounded-xl border border-neutral-200 bg-neutral-900 p-2.5 font-mono text-[11px] leading-relaxed text-neutral-100 dark:border-zinc-800 dark:bg-zinc-950">
      {lines.map((line, idx) => (
        <div key={idx} className={cn('px-2 py-0.5 whitespace-pre', diffLineClass(line))}>
          <span className="inline-block w-8 select-none text-right text-neutral-500 mr-3 opacity-60">
            {idx + 1}
          </span>
          {line || ' '}
        </div>
      ))}
    </div>
  )
}

const copilotMdComponents = {
  pre({ children, ...props }: React.ComponentProps<'pre'>) {
    return (
      <pre className="my-2 overflow-auto rounded-lg border border-neutral-200/80 bg-neutral-900 p-2.5 text-xs text-neutral-100 dark:border-zinc-700/60 font-mono" {...props}>
        {children}
      </pre>
    )
  },
  code({ children, className, ...props }: React.ComponentProps<'code'>) {
    const isInline = !className
    if (isInline) {
      return (
        <code className="rounded bg-neutral-100 px-1 py-0.5 font-mono text-[11px] text-sky-700 dark:bg-zinc-800 dark:text-sky-300" {...props}>
          {children}
        </code>
      )
    }
    const text = String(children || '')
    const lines = text.split('\n')
    const hasDiff = lines.some((l) => l.startsWith('+') || l.startsWith('-'))
    if (hasDiff) {
      return (
        <code className="block min-w-max font-mono text-[11px]" {...props}>
          {lines.map((l, i) => (
            <span key={i} className={cn('block px-2 py-0.5', diffLineClass(l))}>
              {l || ' '}
            </span>
          ))}
        </code>
      )
    }
    return <code className={cn('font-mono text-[11px]', className)} {...props}>{children}</code>
  },
  p({ children, ...props }: React.ComponentProps<'p'>) {
    return <p className="my-1 leading-relaxed text-xs" {...props}>{children}</p>
  },
  ul({ children, ...props }: React.ComponentProps<'ul'>) {
    return <ul className="my-1 ml-4 list-disc space-y-0.5 text-xs" {...props}>{children}</ul>
  },
  ol({ children, ...props }: React.ComponentProps<'ol'>) {
    return <ol className="my-1 ml-4 list-decimal space-y-0.5 text-xs" {...props}>{children}</ol>
  },
  li({ children, ...props }: React.ComponentProps<'li'>) {
    return <li className="leading-relaxed" {...props}>{children}</li>
  },
  h1({ children, ...props }: React.ComponentProps<'h1'>) {
    return <h1 className="mt-2 mb-1 text-sm font-bold" {...props}>{children}</h1>
  },
  h2({ children, ...props }: React.ComponentProps<'h2'>) {
    return <h2 className="mt-1.5 mb-1 text-xs font-bold" {...props}>{children}</h2>
  },
  h3({ children, ...props }: React.ComponentProps<'h3'>) {
    return <h3 className="mt-1 mb-0.5 text-xs font-semibold" {...props}>{children}</h3>
  },
  blockquote({ children, ...props }: React.ComponentProps<'blockquote'>) {
    return <blockquote className="my-1 border-l-2 border-neutral-300 pl-2 text-neutral-500 italic dark:border-zinc-600 dark:text-zinc-400 text-xs" {...props}>{children}</blockquote>
  },
} as import('react-markdown').Components

export function PreviewDrawer({
  project,
  taskId,
  taskTitle,
  previewUrl,
  previewToken,
  previewStatus,
  canOperator,
  onClose,
  onStartPreview,
}: PreviewDrawerProps) {
  const { t } = useTranslation()
  const [iframeKey, setIframeKey] = useState(0)
  const [isMaximized, setIsMaximized] = useState(false)
  const [startBusy, setStartBusy] = useState(false)
  const iframeRef = useRef<HTMLIFrameElement | null>(null)

  // Layout mode: 'floating' (default, doesn't squish viewport) or 'docked' (split-screen side-by-side)
  const [layoutMode, setLayoutMode] = useState<'floating' | 'docked'>(() => {
    try {
      const saved = localStorage.getItem('multigent_preview_drawer_layout')
      if (saved === 'docked' || saved === 'floating') return saved
    } catch {}
    return 'floating'
  })

  // Copilot visibility & minimized states: default closed to keep preview clean!
  const [copilotOpen, setCopilotOpen] = useState(false)
  const [copilotMinimized, setCopilotMinimized] = useState(false)

  // Visual DOM element selector state
  const [isInspectingDOM, setIsInspectingDOM] = useState(false)
  const [inspectorConnected, setInspectorConnected] = useState(false)
  const [selectedDOM, setSelectedDOM] = useState<DOMTarget | null>(null)
  const [reloadKey, setReloadKey] = useState(() => Date.now())

  // Lock and error feedback
  const [lockWarning, setLockWarning] = useState<string | null>(null)
  const [isStreaming, setIsStreaming] = useState(false)
  const [isBusy, setIsBusy] = useState(false)
  const [input, setInput] = useState('')

  // Turn receipt & history states
  const [turnReceiptsEnabled, setTurnReceiptsEnabled] = useState(false)
  const [activeTab, setActiveTab] = useState<'chat' | 'turns'>('chat')
  const [turns, setTurns] = useState<TurnReceipt[]>([])
  const [loadingTurns, setLoadingTurns] = useState(false)
  const [selectedTurnId, setSelectedTurnId] = useState<string | null>(null)
  const [turnDiffMap, setTurnDiffMap] = useState<Record<string, string>>({})
  const [loadingDiff, setLoadingDiff] = useState(false)
  const [isRollingBack, setIsRollingBack] = useState(false)
  const [rollbackError, setRollbackError] = useState<string | null>(null)
  const [showRollbackConfirm, setShowRollbackConfirm] = useState(false)
  const [availableProfiles, setAvailableProfiles] = useState<PreviewSkillProfile[]>([])
  const [selectedProfile, setSelectedProfile] = useState<string | null>(null)

  // Test-data fixture sandbox states (Direction C Phase 2)
  const [sandboxStatus, setSandboxStatus] = useState<TaskSandboxStatus | null>(null)
  const [loadingSandbox, setLoadingSandbox] = useState(false)
  const [resettingSandbox, setResettingSandbox] = useState(false)
  const [switchingScenario, setSwitchingScenario] = useState(false)
  const [sandboxPopoverOpen, setSandboxPopoverOpen] = useState(false)
  const [sandboxMsg, setSandboxMsg] = useState<{ type: 'success' | 'error'; text: string } | null>(null)
  const sandboxPopoverRef = useRef<HTMLDivElement>(null)

  const abortCtrlRef = useRef<AbortController | null>(null)
  const chatScrollRef = useRef<HTMLDivElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Chat history per taskId in localStorage
  const storageKey = `multigent_preview_copilot_chat_${taskId}`
  const [msgs, setMsgs] = useState<CopilotMsg[]>(() => {
    try {
      const saved = localStorage.getItem(`multigent_preview_copilot_chat_${taskId}`)
      if (saved) {
        const parsed = JSON.parse(saved)
        if (Array.isArray(parsed)) {
          return parsed.map((m: any) => {
            if (!m || typeof m !== 'object') return m
            const { domTarget: _unused, ...rest } = m
            return rest
          })
        }
      }
    } catch {}
    return []
  })

  // Security (Phase 0): Strip domTarget metadata from localStorage persistence
  useEffect(() => {
    try {
      const sanitized = msgs.map((m) => {
        if (!m.domTarget) return m
        const { domTarget: _unused, ...rest } = m
        return rest
      })
      localStorage.setItem(storageKey, JSON.stringify(sanitized))
    } catch {}
  }, [msgs, storageKey])

  const handleClearHistory = () => {
    setMsgs([])
    try {
      localStorage.removeItem(storageKey)
    } catch {}
  }

  // Auto-scroll chat to bottom
  useEffect(() => {
    if (copilotOpen && !copilotMinimized && chatScrollRef.current) {
      chatScrollRef.current.scrollTop = chatScrollRef.current.scrollHeight
    }
  }, [msgs, copilotOpen, copilotMinimized])

  // Probe server preview status for running sessions
  const checkStatus = useCallback(async () => {
    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {}
      if (token) headers['Authorization'] = `Bearer ${token}`
      const res = await fetch(apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/status`), {
        headers,
      })
      if (!res.ok) return
      const data = await res.json()
      if (data) {
        setIsBusy(Boolean(data.busy))
        if (typeof data.turnReceiptsEnabled === 'boolean') {
          setTurnReceiptsEnabled(data.turnReceiptsEnabled)
        }
      }
    } catch {}
  }, [project, taskId])

  const fetchTurns = useCallback(async () => {
    setLoadingTurns(true)
    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {}
      if (token) headers['Authorization'] = `Bearer ${token}`
      const res = await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/turns`),
        { headers }
      )
      if (!res.ok) return
      const data = await res.json()
      if (data && Array.isArray(data.receipts)) {
        setTurns(data.receipts)
      }
    } catch {
      // ignore
    } finally {
      setLoadingTurns(false)
    }
  }, [project, taskId])

  const fetchProfiles = useCallback(async () => {
    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {}
      if (token) headers['Authorization'] = `Bearer ${token}`
      const res = await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/profiles`),
        { headers }
      )
      if (!res.ok) return
      const data = await res.json()
      if (data && data.ok && Array.isArray(data.profiles)) {
        setAvailableProfiles(data.profiles)
      }
    } catch {
      // ignore
    }
  }, [project, taskId])

  // Fetch turns and skill profiles when copilot is opened or taskId changes
  useEffect(() => {
    if (copilotOpen) {
      void fetchTurns()
      void fetchProfiles()
    }
  }, [copilotOpen, fetchTurns, fetchProfiles])

  const handleSelectTurn = useCallback(
    async (turn: TurnReceipt) => {
      setSelectedTurnId(turn.turnId)
      if (turn.displayDiff) {
        setTurnDiffMap((prev) => ({ ...prev, [turn.turnId]: turn.displayDiff! }))
        return
      }
      if (turnDiffMap[turn.turnId]) {
        return
      }
      setLoadingDiff(true)
      try {
        const token = getStoredToken()
        const headers: Record<string, string> = {}
        if (token) headers['Authorization'] = `Bearer ${token}`
        const res = await fetch(
          apiUrl(
            `/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/turns/${encodeURIComponent(turn.turnId)}/diff`
          ),
          { headers }
        )
        if (!res.ok) return
        const data = await res.json()
        if (data && typeof data.displayDiff === 'string') {
          setTurnDiffMap((prev) => ({ ...prev, [turn.turnId]: data.displayDiff }))
        }
      } catch {
        // ignore
      } finally {
        setLoadingDiff(false)
      }
    },
    [project, taskId, turnDiffMap]
  )

  useEffect(() => {
    setShowRollbackConfirm(false)
    setRollbackError(null)
  }, [selectedTurnId])

  const handleRollbackTurn = useCallback(
    async (turnId: string) => {
      if (!turnReceiptsEnabled || !canOperator || isRollingBack) return
      setIsRollingBack(true)
      setRollbackError(null)
      try {
        const token = getStoredToken()
        const headers: Record<string, string> = { 'Content-Type': 'application/json' }
        if (token) headers['Authorization'] = `Bearer ${token}`
        const res = await fetch(
          apiUrl(
            `/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/turns/${encodeURIComponent(turnId)}/rollback`
          ),
          { method: 'POST', headers }
        )
        if (!res.ok) {
          const data = await res.json().catch(() => ({}))
          throw new Error(data.error || `回滚失败 (${res.status})`)
        }
        setShowRollbackConfirm(false)
        await fetchTurns()
        setIframeKey((k) => k + 1)
      } catch (err: any) {
        setRollbackError(err?.message || '回滚执行失败')
      } finally {
        setIsRollingBack(false)
      }
    },
    [turnReceiptsEnabled, canOperator, isRollingBack, project, taskId, fetchTurns]
  )

  useEffect(() => {
    void checkStatus()
    void fetchProfiles()
    const timer = setInterval(() => {
      void checkStatus()
    }, 5000)
    return () => clearInterval(timer)
  }, [checkStatus, fetchProfiles])

  const handleSetLayoutMode = (mode: 'floating' | 'docked') => {
    setLayoutMode(mode)
    try {
      localStorage.setItem('multigent_preview_drawer_layout', mode)
    } catch {}
  }

  const handleToggleCopilot = () => {
    if (!copilotOpen) {
      setCopilotOpen(true)
      setCopilotMinimized(false)
    } else if (copilotMinimized) {
      setCopilotMinimized(false)
    } else {
      setCopilotOpen(false)
    }
  }

  const previewOrigin = useMemo(() => {
    if (!previewUrl) return ''
    try {
      return new URL(previewUrl).origin
    } catch {
      return ''
    }
  }, [previewUrl])

  const postToPreview = useCallback((msg: { type: string; [key: string]: unknown }) => {
    if (!iframeRef.current?.contentWindow) return false
    if (!previewOrigin) return false
    try {
      iframeRef.current.contentWindow.postMessage(msg, previewOrigin)
      return true
    } catch {
      return false
    }
  }, [previewOrigin])

  const handleStartInspect = useCallback(() => {
    if (!previewOrigin) return
    setIsInspectingDOM(true)
    setInspectorConnected(false)
    postToPreview({ type: 'MG_START_INSPECTOR' })
  }, [postToPreview, previewOrigin])

  const handleStopInspect = useCallback(() => {
    setIsInspectingDOM(false)
    setInspectorConnected(false)
    postToPreview({ type: 'MG_STOP_INSPECTOR' })
    setCopilotOpen(true)
    setCopilotMinimized(false)
  }, [postToPreview])

  const handleToggleInspect = () => {
    if (!previewOrigin) return
    if (isInspectingDOM) {
      handleStopInspect()
    } else {
      handleStartInspect()
    }
  }

  // Window message bridge for cross-origin DOM selection and bidirectional handshake
  useEffect(() => {
    function handleMessage(e: MessageEvent) {
      if (!e || !e.data || typeof e.data !== 'object') return
      // Security gate: verify event origin and source against expected preview iframe (fail-closed)
      if (!previewOrigin || e.origin !== previewOrigin) return
      if (!iframeRef.current?.contentWindow || e.source !== iframeRef.current.contentWindow) return

      const t = e.data.type
      if (t === 'MG_INSPECTOR_ACTIVE' || t === 'MG_PONG_INSPECTOR') {
        if (e.data.active !== false) {
          setInspectorConnected(true)
        }
      } else if (t === 'MG_INSPECTOR_MOUNTED') {
        if (isInspectingDOM) {
          postToPreview({ type: 'MG_START_INSPECTOR' })
        }
      } else if (t === 'MG_DOM_SELECTED' && e.data.target) {
        const decoded = decodeDOMTarget(e.data.target)
        if (decoded) {
          setSelectedDOM(decoded)
          setIsInspectingDOM(false)
          setInspectorConnected(false)
          setCopilotOpen(true)
          setCopilotMinimized(false)
          setTimeout(() => {
            textareaRef.current?.focus()
          }, 60)
        }
      } else if (t === 'MG_DOM_CANCELLED') {
        setIsInspectingDOM(false)
        setInspectorConnected(false)
        setCopilotOpen(true)
        setCopilotMinimized(false)
      }
    }
    window.addEventListener('message', handleMessage)
    return () => window.removeEventListener('message', handleMessage)
  }, [isInspectingDOM, postToPreview, previewOrigin])

  // Periodic heartbeat / retry until iframe acknowledges inspection mode
  useEffect(() => {
    if (!isInspectingDOM) return

    // Immediately dispatch start request
    postToPreview({ type: 'MG_START_INSPECTOR' })

    // Keep checking gently until connected
    const timer = setInterval(() => {
      if (inspectorConnected) {
        clearInterval(timer)
        return
      }
      postToPreview({ type: 'MG_START_INSPECTOR' })
    }, 350)

    return () => clearInterval(timer)
  }, [isInspectingDOM, inspectorConnected, postToPreview])

  // Keyboard shortcut: Cmd+K / Ctrl+K toggles Copilot, Escape handles hierarchy
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        handleToggleCopilot()
        return
      }
      if (e.key === 'Escape') {
        if (isInspectingDOM) {
          e.preventDefault()
          handleStopInspect()
          return
        }
        if (copilotOpen && !copilotMinimized) {
          e.preventDefault()
          setCopilotOpen(false)
          return
        }
        e.preventDefault()
        onClose()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [copilotOpen, copilotMinimized, isInspectingDOM, onClose])

  // Construct target preview URL with cache-busting timestamp
  const targetUrl = useMemo(() => {
    if (!previewUrl) return ''
    const sep = previewUrl.includes('?') ? '&' : '?'
    const tokenPart = previewToken ? `pvt=${encodeURIComponent(previewToken)}&` : ''
    return `${previewUrl}${sep}${tokenPart}_t=${reloadKey}`
  }, [previewUrl, previewToken, reloadKey])

  const handleReload = useCallback(() => {
    setReloadKey(Date.now())
    setIframeKey((k) => k + 1)
  }, [])

  const handleStartFromDrawer = async () => {
    if (!onStartPreview) return
    setStartBusy(true)
    try {
      await onStartPreview()
      setReloadKey(Date.now())
      setIframeKey((k) => k + 1)
    } finally {
      setStartBusy(false)
    }
  }

  // Fetch fixture sandbox status
  const fetchSandboxStatus = useCallback(async () => {
    if (!taskId) return
    setLoadingSandbox(true)
    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {}
      if (token) headers['Authorization'] = `Bearer ${token}`
      const res = await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/fixture-sandbox`),
        { headers }
      )
      if (!res.ok) return
      const data = await res.json()
      if (data && typeof data === 'object') {
        setSandboxStatus(data)
      }
    } catch {} finally {
      setLoadingSandbox(false)
    }
  }, [project, taskId])

  useEffect(() => {
    fetchSandboxStatus()
  }, [fetchSandboxStatus])

  // Dismiss sandbox popover on outside click
  useEffect(() => {
    if (!sandboxPopoverOpen) return
    const handleClickOutside = (e: MouseEvent) => {
      if (sandboxPopoverRef.current && !sandboxPopoverRef.current.contains(e.target as Node)) {
        setSandboxPopoverOpen(false)
      }
    }
    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [sandboxPopoverOpen])

  // Fast reset (<100ms) restoring private DB to immutable baseline
  const handleResetSandbox = async () => {
    if (!canOperator || resettingSandbox) return
    setResettingSandbox(true)
    setSandboxMsg(null)
    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {}
      if (token) headers['Authorization'] = `Bearer ${token}`
      const res = await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/fixture-sandbox/reset`),
        {
          method: 'POST',
          headers,
        }
      )
      if (!res.ok) {
        const err = await res.json().catch(() => ({ error: 'reset failed' }))
        setSandboxMsg({ type: 'error', text: err.error || err.message || '重置失败' })
        return
      }
      setSandboxMsg({
        type: 'success',
        text: t('tasks.previewDrawer.sandbox.resetSuccess', { defaultValue: '已秒级恢复至干净基准工件' }),
      })
      handleReload()
      await fetchSandboxStatus()
    } catch (e: any) {
      setSandboxMsg({ type: 'error', text: e?.message || '重置失败' })
    } finally {
      setResettingSandbox(false)
    }
  }

  // Switch active scenario
  const handleSwitchScenario = async (scenario: string) => {
    if (!canOperator || switchingScenario) return
    setSwitchingScenario(true)
    setSandboxMsg(null)
    try {
      const token = getStoredToken()
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (token) headers['Authorization'] = `Bearer ${token}`
      const res = await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/fixture-sandbox/scenario`),
        {
          method: 'POST',
          headers,
          body: JSON.stringify({ scenario }),
        }
      )
      if (!res.ok) {
        const err = await res.json().catch(() => ({ error: 'switch failed' }))
        setSandboxMsg({ type: 'error', text: err.error || err.message || '切换场景失败' })
        return
      }
      setSandboxMsg({
        type: 'success',
        text: t('tasks.previewDrawer.sandbox.scenarioSuccess', { scenario, defaultValue: `已切换至场景：${scenario}` }),
      })
      handleReload()
      await fetchSandboxStatus()
    } catch (e: any) {
      setSandboxMsg({ type: 'error', text: e?.message || '切换场景失败' })
    } finally {
      setSwitchingScenario(false)
    }
  }

  const isRunning = previewStatus === 'running'
  const isCopilotActive = copilotOpen && !copilotMinimized

  function processEvent(dataStr: string, assistantMsg: CopilotMsg): boolean {
    if (dataStr === '{"type":"done"}') {
      assistantMsg.isThinking = false
      assistantMsg.isDone = true
      return true
    }
    if (dataStr === '{"type":"stopped"}') {
      assistantMsg.isThinking = false
      assistantMsg.isStopped = true
      if (!assistantMsg.content.includes('[已中止修改]')) {
        assistantMsg.content += '\n🛑 [已中止修改]'
      }
      return true
    }

    try {
      const wrapper = JSON.parse(dataStr)
      const raw = wrapper.payload || dataStr
      const obj = typeof raw === 'string' && raw.startsWith('{')
        ? JSON.parse(raw)
        : (typeof raw === 'object' ? raw : null)

      if (obj) {
        if (obj.type === 'system' || obj.subtype === 'thinking_tokens') {
          assistantMsg.isThinking = true
          return false
        }
        if (obj.type === 'assistant' && obj.message?.content) {
          assistantMsg.isThinking = false
          const arr = Array.isArray(obj.message.content)
            ? obj.message.content
            : [{ type: 'text', text: String(obj.message.content) }]
          for (const b of arr) {
            if (b.type === 'text' && b.text) {
              assistantMsg.content = (assistantMsg.content ? assistantMsg.content + '\n' : '') + b.text
            }
            if (b.type === 'tool_use' && b.name) {
              if (!assistantMsg.tools) assistantMsg.tools = []
              let toolType: CopilotTool['type'] = 'other'
              let target = ''
              const low = b.name.toLowerCase()
              if (low.includes('bash') || low.includes('exec') || low.includes('cmd')) {
                toolType = 'bash'
                target = (b.input && (b.input.command || b.input.cmd)) || ''
              } else if (low.includes('read') || low.includes('view')) {
                toolType = 'read'
                target = (b.input && (b.input.file_path || b.input.path || b.input.AbsolutePath)) || ''
              } else if (low.includes('edit') || low.includes('replace') || low.includes('write')) {
                toolType = 'edit'
                target = (b.input && (b.input.file_path || b.input.path || b.input.TargetFile)) || ''
              } else if (low.includes('grep') || low.includes('search') || low.includes('find')) {
                toolType = 'search'
                target = (b.input && (b.input.pattern || b.input.Query || b.input.Pattern)) || ''
              } else {
                target = typeof b.input === 'string' ? b.input : JSON.stringify(b.input || '').slice(0, 40)
              }
              target = target.trim()
              const exists = assistantMsg.tools.some((t) => t.name === b.name && t.target === target)
              if (!exists) {
                assistantMsg.tools.push({ type: toolType, name: b.name, target: target || b.name })
              }
            }
          }
          return false
        }
        if (obj.type === 'result' && obj.result) {
          assistantMsg.isThinking = false
          if (!assistantMsg.content) assistantMsg.content = obj.result
          return false
        }
      } else if (typeof raw === 'string') {
        const clean = raw.trim()
        if (clean && !clean.startsWith('===') && !clean.startsWith('Command:') && !clean.startsWith('Started:') && !clean.startsWith('{')) {
          assistantMsg.content = (assistantMsg.content ? assistantMsg.content + '\n' : '') + clean
        }
      }
    } catch {}
    return false
  }

  const handleSend = async (overridePrompt?: string, profileOverride?: string) => {
    if (!turnReceiptsEnabled) return
    const text = (overridePrompt ?? input).trim()
    if (!text && !selectedDOM) return
    if (isStreaming) return

    const activeProfile = profileOverride !== undefined ? profileOverride : selectedProfile

    setLockWarning(null)
    const dom = selectedDOM
    setSelectedDOM(null)
    setInput('')

    let displayContent = text
    let finalPrompt = text
    if (dom) {
      const domLabel = dom.id ? `${dom.tag}#${dom.id}` : dom.tag
      displayContent = `@DOM(${domLabel}) ${text || '请针对该元素进行审查'}`
      const structuralInfo = [
        `- 选择器: ${dom.selector}`,
        `- 标签: <${dom.tag}>`,
        dom.id ? `- 标识 (id): ${dom.id}` : null,
        dom.role ? `- 角色 (role): ${dom.role}` : null,
        dom.type ? `- 类型 (type): ${dom.type}` : null,
        dom.testId ? `- 测试标识 (data-testid): ${dom.testId}` : null,
      ].filter(Boolean).join('\n')
      finalPrompt = `【目标页面 DOM 元素结构元数据】:\n${structuralInfo}\n\n【用户审查需求】:\n${text || '请根据上述目标 DOM 元素位置与上下文进行分析'}`
    }

    const userMsg: CopilotMsg = {
      id: 'msg-' + Date.now() + '-u',
      role: 'user',
      content: displayContent,
      profile: activeProfile || undefined,
      domTarget: dom || undefined,
    }

    const assistantMsg: CopilotMsg = {
      id: 'msg-' + Date.now() + '-a',
      role: 'assistant',
      content: '',
      tools: [],
      isThinking: true,
      isDone: false,
      isStopped: false,
    }

    setMsgs((prev) => [...prev, userMsg, assistantMsg])
    setIsStreaming(true)
    setIsBusy(true)

    const controller = new AbortController()
    abortCtrlRef.current = controller

    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        'Accept': 'text/event-stream',
      }
      if (token) headers['Authorization'] = `Bearer ${token}`

      const historyPayload = msgs.slice(-10).map((m) => ({
        role: m.role,
        content: m.content,
      }))

      const reqBody: Record<string, any> = {
        message: finalPrompt,
        history: historyPayload,
      }
      if (activeProfile) {
        reqBody.profile = activeProfile
      }

      const res = await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/chat`),
        {
          method: 'POST',
          headers,
          body: JSON.stringify(reqBody),
          signal: controller.signal,
        }
      )

      if (!res.ok) {
        const rawText = await res.text()
        let errMsg = rawText || `HTTP ${res.status}`
        try {
          const errData = JSON.parse(rawText)
          errMsg = errData.message || (errData.error && errData.error.message) || errMsg
        } catch {}

        if (res.status === 409 || res.status === 429) {
          setLockWarning(errMsg)
        }

        setMsgs((prev) =>
          prev.map((m) =>
            m.id === assistantMsg.id
              ? { ...m, isThinking: false, isDone: true, error: errMsg, content: m.content ? `${m.content}\n\n❌ ${errMsg}` : `❌ ${errMsg}` }
              : m
          )
        )
        return
      }

      const reader = res.body?.getReader()
      if (!reader) throw new Error('No response body')

      const decoder = new TextDecoder()
      let buffer = ''

      while (true) {
        const { done, value } = await reader.read()
        if (done) {
          setMsgs((prev) =>
            prev.map((m) =>
              m.id === assistantMsg.id
                ? { ...m, isThinking: false, isDone: true }
                : m
            )
          )
          break
        }
        buffer += decoder.decode(value, { stream: true })
        const parts = buffer.split('\n')
        buffer = parts.pop() ?? ''

        for (const part of parts) {
          const trimmed = part.trim()
          if (!trimmed.startsWith('data: ')) continue
          const dataStr = trimmed.slice(6)
          const isEnd = processEvent(dataStr, assistantMsg)
          setMsgs((prev) =>
            prev.map((m) => (m.id === assistantMsg.id ? { ...assistantMsg } : m))
          )
          if (isEnd) break
        }
      }
    } catch (err) {
      if ((err as Error).name !== 'AbortError') {
        const errMsg = err instanceof Error ? err.message : String(err)
        setMsgs((prev) =>
          prev.map((m) =>
            m.id === assistantMsg.id
              ? { ...m, isThinking: false, isDone: true, error: errMsg, content: m.content ? `${m.content}\n\n❌ ${errMsg}` : `❌ ${errMsg}` }
              : m
          )
        )
      }
    } finally {
      setIsStreaming(false)
      setIsBusy(false)
      abortCtrlRef.current = null
      setIframeKey((k) => k + 1)
      void fetchTurns()
    }
  }

  const handleStopExecution = async () => {
    if (abortCtrlRef.current) {
      abortCtrlRef.current.abort()
      abortCtrlRef.current = null
    }
    setIsStreaming(false)
    setIsBusy(false)

    try {
      const token = getStoredToken()
      const headers: Record<string, string> = {}
      if (token) headers['Authorization'] = `Bearer ${token}`
      await fetch(
        apiUrl(`/api/v1/projects/${encodeURIComponent(project || 'current')}/tasks/${encodeURIComponent(taskId)}/preview/stop`),
        { method: 'POST', headers }
      )
    } catch {}

    setMsgs((prev) => {
      const last = prev[prev.length - 1]
      if (last && last.role === 'assistant' && !last.isDone) {
        return prev.map((m, i) =>
          i === prev.length - 1
            ? { ...m, isThinking: false, isStopped: true, isDone: true, content: m.content ? `${m.content}\n🛑 [已中止修改]` : '🛑 [已中止修改]' }
            : m
        )
      }
      return prev
    })
  }

  const handleTextareaKeyDown = (e: RKeyboardEvent<HTMLTextAreaElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      void handleSend()
    }
  }

  // Render assistant content body & input
  const renderCopilotBody = () => {
    const selectedTurn = selectedTurnId ? turns.find((t) => t.turnId === selectedTurnId) : null
    const currentDiff = selectedTurnId ? turnDiffMap[selectedTurnId] || selectedTurn?.displayDiff || '' : ''

    return (
      <div className="flex flex-1 flex-col overflow-hidden">
        {/* Navigation Tabs Header */}
        <div className="flex shrink-0 items-center justify-between border-b border-neutral-200/80 bg-neutral-100/50 px-3 py-1.5 dark:border-zinc-800 dark:bg-zinc-800/40">
          <div className="flex items-center gap-1">
            <button
              type="button"
              onClick={() => setActiveTab('chat')}
              className={cn(
                'inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition cursor-pointer',
                activeTab === 'chat'
                  ? 'bg-white text-sky-700 shadow-2xs dark:bg-zinc-900 dark:text-sky-400 font-semibold'
                  : 'text-neutral-500 hover:bg-neutral-200/50 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200'
              )}
            >
              <MessageSquare className="size-3.5" />
              <span>对话调优</span>
            </button>
            <button
              type="button"
              onClick={() => {
                setActiveTab('turns')
                void fetchTurns()
              }}
              className={cn(
                'inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition cursor-pointer',
                activeTab === 'turns'
                  ? 'bg-white text-sky-700 shadow-2xs dark:bg-zinc-900 dark:text-sky-400 font-semibold'
                  : 'text-neutral-500 hover:bg-neutral-200/50 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200'
              )}
            >
              <History className="size-3.5" />
              <span>改动回合</span>
              {turns.length > 0 && (
                <span className="rounded-full bg-neutral-200/80 px-1.5 py-0.2 text-[10px] font-mono text-neutral-600 dark:bg-zinc-700 dark:text-zinc-300">
                  {turns.length}
                </span>
              )}
            </button>
          </div>

          {activeTab === 'turns' && (
            <button
              type="button"
              onClick={() => void fetchTurns()}
              disabled={loadingTurns}
              title="刷新回合列表"
              className="rounded-md p-1 text-neutral-400 hover:bg-neutral-200/60 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200 transition cursor-pointer"
            >
              <RotateCw className={cn('size-3.5', loadingTurns && 'animate-spin')} />
            </button>
          )}
        </div>

        {activeTab === 'turns' ? (
          /* Turns & Diff History View */
          <div className="flex-1 overflow-y-auto p-3.5 text-xs">
            {selectedTurnId ? (
              /* Turn Detail View */
              <div className="space-y-3">
                <div className="flex items-center justify-between pb-1">
                  <button
                    type="button"
                    onClick={() => setSelectedTurnId(null)}
                    className="inline-flex items-center gap-1 text-xs font-medium text-sky-600 hover:text-sky-700 dark:text-sky-400 dark:hover:text-sky-300 cursor-pointer"
                  >
                    <ArrowLeft className="size-3.5" />
                    <span>返回回合列表</span>
                  </button>
                  {selectedTurn && renderTurnStatusBadge(selectedTurn.status)}
                </div>

                {selectedTurn ? (
                  <div className="space-y-3">
                    {/* Meta summary card */}
                    <div className="rounded-xl border border-neutral-200 bg-neutral-50/70 p-3 dark:border-zinc-800 dark:bg-zinc-900/50 space-y-2">
                      <div className="flex items-center justify-between text-[11px] text-neutral-500 dark:text-zinc-400">
                        <span className="font-mono font-medium text-neutral-700 dark:text-zinc-300">
                          {selectedTurn.turnId}
                        </span>
                        <span>{formatTurnTime(selectedTurn.createdAt)}</span>
                      </div>

                      {selectedTurn.redactedPrompt && (
                        <div>
                          <div className="text-[10px] font-medium text-neutral-400 dark:text-zinc-500 mb-0.5">
                            调优需求 (已脱敏)
                          </div>
                          <div className="rounded bg-white p-2 text-[11px] text-neutral-800 dark:bg-zinc-950 dark:text-zinc-200 font-mono whitespace-pre-wrap border border-neutral-200/60 dark:border-zinc-800">
                            {selectedTurn.redactedPrompt}
                          </div>
                        </div>
                      )}

                      {selectedTurn.failureReason && (
                        <div className="rounded-lg border border-red-200 bg-red-50/80 p-2 text-[11px] text-red-800 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-200">
                          <span className="font-semibold">失败原因：</span>
                          <span className="font-mono">{selectedTurn.failureReason}</span>
                        </div>
                      )}

                      {selectedTurn.touchedPaths && selectedTurn.touchedPaths.length > 0 && (
                        <div>
                          <div className="text-[10px] font-medium text-neutral-400 dark:text-zinc-500 mb-1 flex items-center gap-1">
                            <FileCode className="size-3" />
                            <span>改动文件 ({selectedTurn.touchedPaths.length})</span>
                          </div>
                          <div className="flex flex-wrap gap-1">
                            {selectedTurn.touchedPaths.map((p) => (
                              <span
                                key={p}
                                className="rounded bg-neutral-200/70 px-1.5 py-0.5 font-mono text-[10px] text-neutral-700 dark:bg-zinc-800 dark:text-zinc-300"
                              >
                                {p}
                              </span>
                            ))}
                          </div>
                        </div>
                      )}

                      <div className="flex items-center justify-between pt-1 text-[10px] text-neutral-400 dark:text-zinc-500">
                        <span>版本修订: Rev {selectedTurn.revision}</span>
                        {selectedTurn.creator && <span>执行者: {selectedTurn.creator}</span>}
                      </div>
                    </div>

                    {/* Diff Viewer */}
                    <div className="space-y-1.5">
                      <div className="flex items-center justify-between text-[11px] font-semibold text-neutral-700 dark:text-zinc-300">
                        <span className="flex items-center gap-1.5">
                          <Code2 className="size-3.5 text-neutral-500" />
                          <span>变更详情 (DisplayDiff)</span>
                        </span>
                      </div>
                      {loadingDiff ? (
                        <div className="flex items-center justify-center py-8 text-neutral-400">
                          <RotateCw className="size-4 animate-spin mr-2" />
                          <span>加载 Diff 中…</span>
                        </div>
                      ) : currentDiff ? (
                        renderDiffContent(currentDiff)
                      ) : (
                        <div className="rounded-xl border border-neutral-200/70 bg-neutral-50/50 p-4 text-center text-xs text-neutral-400 dark:border-zinc-800 dark:bg-zinc-950/40">
                          无代码改动内容或该回合未触碰文件
                        </div>
                      )}
                    </div>

                    {/* Turn Actions: Rollback */}
                    {selectedTurn.status === 'CAPTURED' && (
                      <div className="rounded-xl border border-neutral-200/80 bg-neutral-50/80 p-3 dark:border-zinc-800 dark:bg-zinc-900/40 space-y-2">
                        <div className="flex items-center justify-between">
                          <span className="text-xs font-semibold text-neutral-800 dark:text-zinc-200 flex items-center gap-1.5">
                            <RotateCcw className="size-3.5 text-amber-600 dark:text-amber-400" />
                            <span>回合操作</span>
                          </span>
                          {turnReceiptsEnabled && canOperator && !showRollbackConfirm && (
                            <button
                              type="button"
                              onClick={() => setShowRollbackConfirm(true)}
                              disabled={isRollingBack}
                              className="inline-flex items-center gap-1.5 rounded-lg border border-amber-300 bg-amber-50 px-2.5 py-1 text-xs font-medium text-amber-800 hover:bg-amber-100 transition dark:border-amber-800/80 dark:bg-amber-950/40 dark:text-amber-300 dark:hover:bg-amber-900/60 cursor-pointer"
                            >
                              <RotateCcw className="size-3.5" />
                              <span>回滚此回合</span>
                            </button>
                          )}
                        </div>

                        {!turnReceiptsEnabled ? (
                          <div className="flex items-center gap-2 text-[11px] text-neutral-500 dark:text-zinc-400">
                            <Lock className="size-3.5 shrink-0 text-neutral-400" />
                            <span>写操作目前在只读审查模式下已冻结（MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS=false），禁止触发回滚写操作。</span>
                          </div>
                        ) : !canOperator ? (
                          <div className="flex items-center gap-2 text-[11px] text-neutral-500 dark:text-zinc-400">
                            <Lock className="size-3.5 shrink-0 text-neutral-400" />
                            <span>当前用户权限不足，仅项目 Operator 可回滚改动。</span>
                          </div>
                        ) : showRollbackConfirm ? (
                          <div className="rounded-lg border border-amber-200 bg-amber-50/60 p-2.5 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-200 space-y-2">
                            <div className="font-medium">确认回滚此回合的所有代码修改？</div>
                            <p className="text-[11px] text-amber-700 dark:text-amber-300/80">
                              系统将执行逆向补丁将 Worktree 还原至此回合前的基线状态，并同步释放回合快照。
                            </p>
                            {rollbackError && (
                              <div className="text-[11px] text-red-600 dark:text-red-400 font-mono">
                                {rollbackError}
                              </div>
                            )}
                            <div className="flex items-center gap-2 pt-1">
                              <button
                                type="button"
                                onClick={() => void handleRollbackTurn(selectedTurn.turnId)}
                                disabled={isRollingBack}
                                className="inline-flex items-center gap-1 rounded-md bg-amber-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-amber-700 transition disabled:opacity-50 cursor-pointer"
                              >
                                {isRollingBack ? (
                                  <>
                                    <RotateCw className="size-3 animate-spin" />
                                    <span>回滚中…</span>
                                  </>
                                ) : (
                                  <span>确认回滚</span>
                                )}
                              </button>
                              <button
                                type="button"
                                onClick={() => {
                                  setShowRollbackConfirm(false)
                                  setRollbackError(null)
                                }}
                                disabled={isRollingBack}
                                className="rounded-md border border-neutral-300 bg-white px-2.5 py-1 text-xs font-medium text-neutral-700 hover:bg-neutral-50 transition dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-300 cursor-pointer"
                              >
                                取消
                              </button>
                            </div>
                          </div>
                        ) : rollbackError ? (
                          <div className="text-[11px] text-red-600 dark:text-red-400 font-mono">
                            {rollbackError}
                          </div>
                        ) : null}
                      </div>
                    )}
                  </div>
                ) : (
                  <div className="py-6 text-center text-neutral-400">未找到回合详情</div>
                )}
              </div>
            ) : (
              /* Turns List View */
              <div className="space-y-2.5">
                {loadingTurns ? (
                  <div className="flex flex-col items-center justify-center py-10 text-neutral-400">
                    <RotateCw className="size-5 animate-spin mb-2" />
                    <span>加载改动回合中…</span>
                  </div>
                ) : turns.length === 0 ? (
                  <div className="flex flex-col items-center justify-center py-12 text-center text-neutral-400 dark:text-zinc-500">
                    <History className="size-8 mb-2 opacity-30" />
                    <div className="text-xs font-semibold text-neutral-600 dark:text-zinc-400">暂无改动回合记录</div>
                    <div className="mt-1 text-[11px] max-w-xs text-neutral-400 dark:text-zinc-500 leading-relaxed">
                      智能调优所执行的每一次沙箱改动均会生成审计回执与快照，供随时核验与审查。
                    </div>
                  </div>
                ) : (
                  turns.map((t) => (
                    <div
                      key={t.turnId}
                      onClick={() => void handleSelectTurn(t)}
                      className="cursor-pointer rounded-xl border border-neutral-200/80 bg-white p-3 shadow-2xs hover:border-sky-300 hover:shadow-xs transition dark:border-zinc-800 dark:bg-zinc-900/60 dark:hover:border-sky-700/60"
                    >
                      <div className="flex items-center justify-between gap-2">
                        <div className="flex items-center gap-2 overflow-hidden">
                          <span className="font-mono text-xs font-medium text-neutral-800 dark:text-zinc-200 truncate">
                            {t.turnId}
                          </span>
                        </div>
                        {renderTurnStatusBadge(t.status)}
                      </div>

                      {t.redactedPrompt && (
                        <p className="mt-1.5 line-clamp-2 text-[11px] text-neutral-600 dark:text-zinc-300">
                          {t.redactedPrompt}
                        </p>
                      )}

                      <div className="mt-2 flex items-center justify-between text-[10px] text-neutral-400 dark:text-zinc-500 pt-1 border-t border-neutral-100 dark:border-zinc-800/80">
                        <div className="flex items-center gap-2">
                          <span>{formatTurnTime(t.createdAt)}</span>
                          {t.touchedPaths && t.touchedPaths.length > 0 && (
                            <span className="rounded bg-neutral-100 px-1.5 py-0.2 font-mono text-[10px] text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400">
                              {t.touchedPaths.length} 文件
                            </span>
                          )}
                        </div>
                        <span className="text-sky-600 dark:text-sky-400 font-medium">查看详情 →</span>
                      </div>
                    </div>
                  ))
                )}
              </div>
            )}
          </div>
        ) : (
          /* Chat Body and Input */
          <>
            <div ref={chatScrollRef} className="flex-1 space-y-3.5 overflow-y-auto p-4 text-xs">
              {/* Read-only Banner when Turn Receipts Flag is false */}
              {!turnReceiptsEnabled && (
                <div className="rounded-xl border border-sky-200/80 bg-sky-50/80 p-2.5 text-[11px] text-sky-900 shadow-2xs dark:border-sky-800/60 dark:bg-sky-950/40 dark:text-sky-200 flex items-start gap-2">
                  <AlertCircle className="size-4 shrink-0 text-sky-600 dark:text-sky-400 mt-0.5" />
                  <div className="space-y-0.5">
                    <div className="font-semibold">只读审查模式</div>
                    <div className="text-[10px] leading-relaxed opacity-90">
                      Receipt Flag 未开启（只读模式），禁止提交调优指令或触发代码写操作。您可切换至「改动回合」标签查看历史代码变更与 Diff 审计记录。
                    </div>
                  </div>
                </div>
              )}

              {/* Workflow Concurrency / Write Lock Warning Banner */}
              {lockWarning && (
                <div className="rounded-xl border border-amber-300 bg-amber-50/90 p-3 text-amber-900 shadow-xs dark:border-amber-800 dark:bg-amber-950/60 dark:text-amber-200 animate-fade-in">
                  <div className="flex items-start gap-2">
                    <Lock className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
                    <div className="space-y-1">
                      <div className="font-semibold">{lockWarning}</div>
                      <div className="text-[11px] opacity-85 leading-relaxed">
                        {t('tasks.previewDrawer.workflowLocked', {
                          defaultValue: '工作流写保护：当前节点正由智能体后台执行中。待流转至人工审核节点后即可提交即时调优。',
                        })}
                      </div>
                    </div>
                  </div>
                </div>
              )}

              {/* Empty state with helpful prompt chips */}
              {msgs.length === 0 ? (
                <div className="flex flex-col items-center justify-center py-6 text-center text-neutral-500 dark:text-zinc-400">
                  <div className="mb-3 flex size-12 items-center justify-center rounded-2xl bg-sky-50 text-sky-600 shadow-xs dark:bg-sky-950/60 dark:text-sky-400">
                    <Sparkles className="size-6" />
                  </div>
                  <h4 className="text-sm font-semibold text-neutral-800 dark:text-zinc-200">
                    {t('tasks.previewDrawer.copilotTitle', { defaultValue: '智能调优助手' })}
                  </h4>
                  <p className="mt-1 max-w-xs text-[11px] text-neutral-400 dark:text-zinc-500">
                    与任务当前代码分支实时对话，支持点击页面选取 DOM 元素并即时修改。
                  </p>

                  {/* Quick chips */}
                  <div className="mt-4 flex flex-col gap-2 w-full max-w-xs text-left">
                    <button
                      type="button"
                      onClick={handleToggleInspect}
                      disabled={!previewOrigin}
                      title={!previewOrigin ? '预览源未配置或不匹配，已禁用元素选取' : (isInspectingDOM ? '取消选取 (Esc)' : '在页面上点击选择目标元素')}
                      className="flex items-center gap-2 rounded-xl border border-sky-200/80 bg-sky-50/70 p-2.5 text-xs text-sky-800 hover:bg-sky-100 disabled:opacity-50 disabled:cursor-not-allowed transition dark:border-sky-800 dark:bg-sky-950/40 dark:text-sky-300"
                    >
                      <Crosshair className="size-3.5 shrink-0 text-sky-600 dark:text-sky-400" />
                      <span className="truncate">{t('tasks.previewDrawer.emptyPrompt1', { defaultValue: '🎯 选取页面元素并修改' })}</span>
                    </button>
                    <button
                      type="button"
                      onClick={() => void handleSend('请优化当前页面的整体色彩搭配、边距排版和视觉层次', 'ui-polish')}
                      disabled={!turnReceiptsEnabled || !canOperator}
                      title={!turnReceiptsEnabled ? '只读审查模式，禁止发起调优' : undefined}
                      className="flex items-center gap-2 rounded-xl border border-neutral-200/80 bg-neutral-50/70 p-2.5 text-xs text-neutral-700 hover:bg-neutral-100 disabled:opacity-50 disabled:cursor-not-allowed transition dark:border-zinc-800 dark:bg-zinc-800/40 dark:text-zinc-300"
                    >
                      <span>🎨</span>
                      <span className="truncate">{t('tasks.previewDrawer.emptyPrompt2', { defaultValue: '🎨 调整整体色调与排版' })}</span>
                    </button>
                    <button
                      type="button"
                      onClick={() => void handleSend('请检查并修复页面表单输入项的交互逻辑与校验提示', 'form-logic')}
                      disabled={!turnReceiptsEnabled || !canOperator}
                      title={!turnReceiptsEnabled ? '只读审查模式，禁止发起调优' : undefined}
                      className="flex items-center gap-2 rounded-xl border border-neutral-200/80 bg-neutral-50/70 p-2.5 text-xs text-neutral-700 hover:bg-neutral-100 disabled:opacity-50 disabled:cursor-not-allowed transition dark:border-zinc-800 dark:bg-zinc-800/40 dark:text-zinc-300"
                    >
                      <span>⚡</span>
                      <span className="truncate">{t('tasks.previewDrawer.emptyPrompt3', { defaultValue: '⚡ 修复表单提交与校验逻辑' })}</span>
                    </button>
                    <button
                      type="button"
                      onClick={() => void handleSend('请适配移动端与窄屏幕视口下的排版与流式响应式布局', 'responsive-layout')}
                      disabled={!turnReceiptsEnabled || !canOperator}
                      title={!turnReceiptsEnabled ? '只读审查模式，禁止发起调优' : undefined}
                      className="flex items-center gap-2 rounded-xl border border-neutral-200/80 bg-neutral-50/70 p-2.5 text-xs text-neutral-700 hover:bg-neutral-100 disabled:opacity-50 disabled:cursor-not-allowed transition dark:border-zinc-800 dark:bg-zinc-800/40 dark:text-zinc-300"
                    >
                      <span>📱</span>
                      <span className="truncate">{t('tasks.previewDrawer.emptyPrompt4', { defaultValue: '📱 适配移动端视口宽度' })}</span>
                    </button>
                  </div>
                </div>
              ) : (
                msgs.map((m) => {
                  if (m.role === 'user') {
                    return (
                      <div key={m.id} className="flex justify-end">
                        <div className="max-w-[85%] rounded-2xl rounded-tr-xs bg-sky-600 px-3.5 py-2.5 text-white shadow-xs dark:bg-sky-500">
                          {m.profile && (
                            <div className="mb-1.5 inline-flex items-center gap-1 rounded bg-sky-700/80 px-2 py-0.5 text-[10px] font-medium text-sky-100">
                              <Sparkles className="size-2.5" />
                              <span>{availableProfiles.find((p) => p.id === m.profile)?.name || m.profile}</span>
                            </div>
                          )}
                          {m.domTarget && (
                            <div className="mb-1.5 inline-flex items-center gap-1 rounded bg-sky-700/80 px-2 py-0.5 text-[10px] font-mono text-sky-100">
                              <span>🎯 @DOM</span>
                              <span>({m.domTarget.tag})</span>
                            </div>
                          )}
                          <div className="whitespace-pre-wrap break-words leading-relaxed text-xs">
                            {m.content}
                          </div>
                        </div>
                      </div>
                    )
                  }

                  return (
                    <div key={m.id} className="flex justify-start">
                      <div className="max-w-[95%] rounded-2xl rounded-tl-xs border border-neutral-200/80 bg-neutral-50/90 p-3 text-neutral-800 shadow-xs dark:border-zinc-800 dark:bg-zinc-800/50 dark:text-zinc-100">
                        {/* Stopped notice */}
                        {m.isStopped && (
                          <div className="mb-2 flex items-center gap-1.5 rounded-lg border border-red-200 bg-red-50/70 px-2.5 py-1 text-[11px] font-medium text-red-700 dark:border-red-900/40 dark:bg-red-950/30 dark:text-red-300">
                            <span>🛑</span>
                            <span>修改已被手动中止</span>
                          </div>
                        )}

                        {/* Thinking Spinner */}
                        {m.isThinking && !m.content && (!m.tools || m.tools.length === 0) && (
                          <div className="flex items-center gap-2 py-2 text-xs text-sky-600 dark:text-sky-400">
                            <div className="size-3.5 animate-spin rounded-full border-2 border-sky-500 border-t-transparent" />
                            <span>正在分析代码并规划修改方案…</span>
                          </div>
                        )}

                        {/* Collapsible Tool Executions Timeline */}
                        {m.tools && m.tools.length > 0 && (
                          <details
                            className="group my-2 rounded-xl border border-neutral-200 bg-white/80 p-2.5 text-xs text-neutral-700 shadow-2xs dark:border-zinc-700/60 dark:bg-zinc-900/70 dark:text-zinc-300"
                            open={!m.isDone && !m.isStopped}
                          >
                            <summary className="flex cursor-pointer select-none items-center justify-between font-medium outline-none">
                              <span className="flex items-center gap-1.5">
                                <span>🛠️</span>
                                <span>
                                  执行了 <strong>{m.tools.length}</strong> 个操作
                                </span>
                                {!m.isDone && !m.isStopped && (
                                  <span className="animate-pulse rounded-full bg-sky-100 px-1.5 py-0.5 text-[10px] font-semibold text-sky-700 dark:bg-sky-950 dark:text-sky-300">
                                    执行中
                                  </span>
                                )}
                              </span>
                              <span className="text-[11px] text-neutral-400 group-open:rotate-180 transition-transform">
                                ▾
                              </span>
                            </summary>
                            <div className="mt-2 space-y-1.5 border-t border-neutral-100 pt-2 dark:border-zinc-800">
                              {m.tools.map((tool, idx) => {
                                const icon =
                                  tool.type === 'bash' ? '⚡' : tool.type === 'read' ? '📖' : tool.type === 'edit' ? '✏️' : tool.type === 'search' ? '🔍' : '🔧'
                                const badgeCls =
                                  tool.type === 'edit'
                                    ? 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-300'
                                    : tool.type === 'bash'
                                    ? 'bg-violet-100 text-violet-800 dark:bg-violet-950/60 dark:text-violet-300'
                                    : 'bg-neutral-100 text-neutral-700 dark:bg-zinc-800 dark:text-zinc-300'
                                return (
                                  <div key={idx} className="flex items-center gap-2 text-[11px] font-mono">
                                    <span className={cn('shrink-0 rounded px-1.5 py-0.5 text-[10px] font-semibold', badgeCls)}>
                                      {icon} {tool.name}
                                    </span>
                                    <span className="truncate text-neutral-600 dark:text-zinc-400" title={tool.target}>
                                      {tool.target}
                                    </span>
                                  </div>
                                )
                              })}
                            </div>
                          </details>
                        )}

                        {/* Formatted Markdown Body with Diff Styling */}
                        {m.content && (
                          <div className="leading-relaxed">
                            <Markdown remarkPlugins={[remarkGfm]} components={copilotMdComponents}>
                              {m.content}
                            </Markdown>
                          </div>
                        )}

                        {/* Error display */}
                        {m.error && (
                          <div className="mt-2 flex items-center gap-1.5 text-xs text-rose-600 dark:text-rose-400 font-medium">
                            <AlertCircle className="size-3.5 shrink-0" />
                            <span>{m.error}</span>
                          </div>
                        )}
                      </div>
                    </div>
                  )
                })
              )}
            </div>

            {/* Bottom Input Area */}
            <div className="border-t border-neutral-100 bg-white/70 p-3.5 dark:border-zinc-800 dark:bg-zinc-900/70">
              {/* Selected DOM target pill above input */}
              {selectedDOM && (
                <div className="group relative mb-2 inline-flex items-center gap-1.5 rounded-lg border border-sky-300 bg-sky-50 px-2.5 py-1 text-xs font-medium text-sky-800 shadow-2xs dark:border-sky-800 dark:bg-sky-950/60 dark:text-sky-200">
                  <span className="font-semibold text-sky-600 dark:text-sky-400">🎯 @DOM</span>
                  <span className="max-w-[180px] truncate font-mono text-[11px]">{selectedDOM.tag}{selectedDOM.id ? `#${selectedDOM.id}` : ''}</span>
                  <button
                    type="button"
                    onClick={() => setSelectedDOM(null)}
                    className="ml-0.5 rounded p-0.5 text-sky-500 hover:bg-sky-200/60 hover:text-sky-800 dark:text-sky-400 dark:hover:bg-sky-900"
                    title={t('common.delete', { defaultValue: '移除' })}
                  >
                    <X className="size-3" />
                  </button>

                  {/* Hover details card */}
                  <div className="pointer-events-none absolute bottom-full left-0 z-50 mb-1.5 hidden w-72 rounded-xl border border-neutral-200 bg-white/95 p-3 text-left shadow-xl backdrop-blur-md group-hover:block dark:border-zinc-700 dark:bg-zinc-900/95">
                    <div className="mb-1.5 text-[11px] font-semibold text-neutral-800 dark:text-zinc-200">
                      {t('tasks.previewDrawer.targetContext', { defaultValue: '目标元素定位上下文' })}
                    </div>
                    <div className="space-y-1 font-mono text-[10px] text-neutral-600 break-all dark:text-zinc-400">
                      <div>
                        <span className="text-neutral-400">{t('tasks.previewDrawer.selector', { defaultValue: '选择器' })}: </span>
                        {selectedDOM.selector}
                      </div>
                      {selectedDOM.role && (
                        <div>
                          <span className="text-neutral-400">role: </span>
                          {selectedDOM.role}
                        </div>
                      )}
                      {selectedDOM.type && (
                        <div>
                          <span className="text-neutral-400">type: </span>
                          {selectedDOM.type}
                        </div>
                      )}
                      {selectedDOM.testId && (
                        <div>
                          <span className="text-neutral-400">data-testid: </span>
                          {selectedDOM.testId}
                        </div>
                      )}
                    </div>
                  </div>
                </div>
              )}

              {!turnReceiptsEnabled ? (
                <div className="mb-2 flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-neutral-100/80 px-2.5 py-1.5 text-[11px] text-neutral-600 dark:border-zinc-800 dark:bg-zinc-800/60 dark:text-zinc-400">
                  <Lock className="size-3 shrink-0" />
                  <span>只读审查模式：调优写操作暂未开放（可在「改动回合」中审查过往改动）</span>
                </div>
              ) : !canOperator ? (
                <div className="mb-2 flex items-center gap-1.5 rounded-lg border border-amber-200 bg-amber-50/80 px-2.5 py-1.5 text-[11px] text-amber-700 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-300">
                  <Lock className="size-3 shrink-0" />
                  <span>当前无 Operator 权限，仅可查看调优状态</span>
                </div>
              ) : null}

              {turnReceiptsEnabled && canOperator && availableProfiles.length > 0 && (
                <div className="mb-2 flex flex-wrap items-center gap-1.5">
                  <span className="text-[10px] text-neutral-400 dark:text-zinc-500 font-medium">技能指引:</span>
                  {availableProfiles.map((p) => {
                    const isSelected = selectedProfile === p.id
                    return (
                      <button
                        key={p.id}
                        type="button"
                        onClick={() => setSelectedProfile(isSelected ? null : p.id)}
                        title={`${p.description} (包含: ${p.skillIds.join(', ')})`}
                        className={cn(
                          'inline-flex items-center gap-1 rounded-lg px-2 py-0.5 text-[10px] font-medium transition border',
                          isSelected
                            ? 'bg-sky-50 border-sky-300 text-sky-700 dark:bg-sky-950/60 dark:border-sky-700 dark:text-sky-300 shadow-2xs'
                            : 'bg-neutral-50 border-neutral-200 text-neutral-600 hover:bg-neutral-100 dark:bg-zinc-800/60 dark:border-zinc-700 dark:text-zinc-400 dark:hover:bg-zinc-800'
                        )}
                      >
                        <Sparkles className={cn('size-2.5', isSelected ? 'text-sky-600 dark:text-sky-400' : 'text-neutral-400 dark:text-zinc-500')} />
                        <span>{p.name}</span>
                        {isSelected && (
                          <X className="size-2.5 ml-0.5 hover:text-sky-900 dark:hover:text-sky-100" />
                        )}
                      </button>
                    )
                  })}
                </div>
              )}

              <div className="relative">
                <textarea
                  ref={textareaRef}
                  rows={2}
                  value={input}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={handleTextareaKeyDown}
                  disabled={isStreaming || !canOperator || !turnReceiptsEnabled}
                  placeholder={
                    !turnReceiptsEnabled
                      ? '只读审查模式，调优写操作已禁用…'
                      : !canOperator
                      ? '仅项目 Operator 可发起调优'
                      : selectedDOM
                      ? `针对 @DOM(${selectedDOM.tag}${selectedDOM.id ? `#${selectedDOM.id}` : ''}) 输入修改要求 (⌘+Enter 发送)…`
                      : t('tasks.previewDrawer.inputPlaceholder', { defaultValue: '描述想要修改的页面内容或样式 (⌘+Enter 发送)…' })
                  }
                  className="w-full resize-none rounded-xl border border-neutral-200 bg-neutral-50/80 p-2.5 text-xs text-neutral-800 outline-none transition focus:border-sky-500 focus:bg-white dark:border-zinc-800 dark:bg-zinc-800/60 dark:text-zinc-100 dark:focus:border-sky-500 dark:focus:bg-zinc-900 disabled:opacity-60 disabled:cursor-not-allowed"
                />
              </div>

              <div className="mt-2 flex items-center justify-between text-[11px] text-neutral-400 dark:text-zinc-500">
                <span className="hidden sm:inline">⌘+Enter 发送 · Enter 换行</span>
                <span className="sm:hidden" />
                <div className="flex items-center gap-2">
                  {isStreaming ? (
                    <button
                      type="button"
                      onClick={() => void handleStopExecution()}
                      className="inline-flex items-center gap-1.5 rounded-lg bg-red-600 px-3 py-1 font-semibold text-white shadow-xs hover:bg-red-500 active:scale-95 transition"
                    >
                      <Square className="size-3 fill-white" />
                      <span>{t('tasks.previewDrawer.stopBtn', { defaultValue: '中止' })}</span>
                    </button>
                  ) : (
                    <button
                      type="button"
                      onClick={() => void handleSend()}
                      disabled={(!input.trim() && !selectedDOM) || !canOperator || !turnReceiptsEnabled}
                      title={!turnReceiptsEnabled ? '只读审查模式，禁止提交修改' : !canOperator ? '仅项目 Operator 可发起调优' : undefined}
                      className="inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-1 font-semibold text-white shadow-xs hover:bg-sky-500 active:scale-95 disabled:cursor-not-allowed disabled:opacity-40 transition"
                    >
                      <Send className="size-3" />
                      <span>{t('tasks.previewDrawer.sendBtn', { defaultValue: '发送' })}</span>
                    </button>
                  )}
                </div>
              </div>
            </div>
          </>
        )}
      </div>
    )
  }

  return createPortal(
    <div
      className={cn(
        'fixed inset-0 z-[100] flex items-center justify-center bg-black/60 backdrop-blur-xs transition-all duration-150',
        isMaximized ? 'p-0' : 'p-2 sm:p-4'
      )}
    >
      <div
        className={cn(
          'relative flex flex-col overflow-hidden bg-white shadow-2xl transition-all duration-200 dark:bg-zinc-900',
          isMaximized
            ? 'h-full w-full rounded-none border-0'
            : 'h-[92vh] w-[96vw] max-w-[1720px] rounded-2xl border border-neutral-200 dark:border-zinc-700'
        )}
      >
        {/* Top Navigation Bar */}
        <div className="flex h-12 shrink-0 items-center justify-between border-b border-neutral-200 bg-neutral-50/90 px-4 dark:border-zinc-800 dark:bg-zinc-900/90">
          <div className="flex items-center gap-3 overflow-hidden">
            <span
              className={cn(
                'relative flex size-7 items-center justify-center rounded-lg transition-colors',
                isRunning
                  ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-400'
                  : 'bg-neutral-200 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400'
              )}
            >
              <Globe className="size-4" />
              {isRunning && (
                <span className="absolute -top-0.5 -right-0.5 size-2 rounded-full bg-emerald-500 ring-2 ring-white dark:ring-zinc-900 animate-pulse" />
              )}
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
            {sandboxStatus?.hasContract && (
              <div className="relative" ref={sandboxPopoverRef}>
                <button
                  type="button"
                  onClick={() => setSandboxPopoverOpen((v) => !v)}
                  title={t('tasks.previewDrawer.sandbox.btnTitle', {
                    defaultValue: '测试数据沙盒：私有数据库、一键重置与场景切换',
                  })}
                  className={cn(
                    'flex items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-colors',
                    sandboxPopoverOpen
                      ? 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-300'
                      : 'text-neutral-600 hover:bg-neutral-200/60 dark:text-zinc-400 dark:hover:bg-zinc-800'
                  )}
                >
                  <Database className={cn('size-3.5 text-amber-600 dark:text-amber-400', loadingSandbox && 'animate-pulse')} />
                  <span>{t('tasks.previewDrawer.sandbox.btnText', { defaultValue: '数据沙盒' })}</span>
                  {typeof sandboxStatus.resetCount === 'number' && sandboxStatus.resetCount > 0 && (
                    <span className="rounded bg-amber-200/70 px-1 py-0.2 text-[10px] font-mono font-semibold text-amber-800 dark:bg-amber-900/80 dark:text-amber-200">
                      ↺{sandboxStatus.resetCount}
                    </span>
                  )}
                </button>

                {sandboxPopoverOpen && (
                  <div className="absolute right-0 top-full z-50 mt-1.5 w-80 rounded-xl border border-neutral-200 bg-white p-3.5 shadow-xl backdrop-blur-md dark:border-zinc-800 dark:bg-zinc-900 animate-in fade-in zoom-in-95 duration-150">
                    <div className="flex items-center justify-between border-b border-neutral-100 pb-2.5 dark:border-zinc-800">
                      <div className="flex items-center gap-2">
                        <Database className="size-4 text-amber-600 dark:text-amber-400" />
                        <span className="text-xs font-semibold text-neutral-900 dark:text-zinc-100">
                          {t('tasks.previewDrawer.sandbox.panelTitle', { defaultValue: '测试数据沙盒' })}
                        </span>
                      </div>
                      <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] font-mono text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400">
                        {sandboxStatus.engine || 'sqlite'}
                      </span>
                    </div>

                    <div className="my-2.5 space-y-2 text-xs">
                      {sandboxStatus.storage && (
                        <div className="flex items-center justify-between text-neutral-500 dark:text-zinc-400">
                          <span>{t('tasks.previewDrawer.sandbox.storageLabel', { defaultValue: '存储路径' })}</span>
                          <span className="truncate max-w-44 font-mono text-[11px] text-neutral-800 dark:text-zinc-200" title={sandboxStatus.storage}>
                            {sandboxStatus.storage}
                          </span>
                        </div>
                      )}

                      {sandboxStatus.leaseId && (
                        <div className="flex items-center justify-between text-neutral-500 dark:text-zinc-400">
                          <span>{t('tasks.previewDrawer.sandbox.leaseInfo', { defaultValue: '沙盒租期' })}</span>
                          <span className="font-mono text-[11px] text-neutral-800 dark:text-zinc-200">
                            {sandboxStatus.expiresAt ? (
                              (() => {
                                const diff = Math.max(0, Math.round((new Date(sandboxStatus.expiresAt).getTime() - Date.now()) / 60000))
                                return t('tasks.previewDrawer.sandbox.expiry', { mins: diff, defaultValue: `剩余 ${diff} 分钟` })
                              })()
                            ) : (
                              sandboxStatus.leaseId
                            )}
                          </span>
                        </div>
                      )}
                    </div>

                    {/* Scenario Switcher */}
                    {sandboxStatus.availableScenarios && sandboxStatus.availableScenarios.length > 0 && (
                      <div className="border-t border-neutral-100 pt-2.5 dark:border-zinc-800">
                        <label className="block mb-1.5 text-[11px] font-medium text-neutral-700 dark:text-zinc-300">
                          {t('tasks.previewDrawer.sandbox.scenarioLabel', { defaultValue: '测试场景' })}
                        </label>
                        <div className="space-y-1 max-h-36 overflow-y-auto">
                          {sandboxStatus.availableScenarios.map((sc) => {
                            const isActive = (sandboxStatus.activeScenario || 'default') === sc.name
                            return (
                              <button
                                key={sc.name}
                                type="button"
                                disabled={!canOperator || switchingScenario || isActive}
                                onClick={() => void handleSwitchScenario(sc.name)}
                                className={cn(
                                  'flex w-full items-center justify-between rounded-lg px-2.5 py-1.5 text-left text-xs transition-colors',
                                  isActive
                                    ? 'bg-amber-50 font-semibold text-amber-900 dark:bg-amber-950/40 dark:text-amber-200'
                                    : 'text-neutral-700 hover:bg-neutral-100 dark:text-zinc-300 dark:hover:bg-zinc-800/60',
                                  (!canOperator || switchingScenario) && !isActive && 'opacity-50 cursor-not-allowed'
                                )}
                              >
                                <div className="min-w-0 pr-2">
                                  <div className="truncate">{sc.name}</div>
                                  {sc.description && (
                                    <div className="truncate text-[10px] text-neutral-400 dark:text-zinc-500 font-normal">
                                      {sc.description}
                                    </div>
                                  )}
                                </div>
                                {isActive && <Check className="size-3.5 shrink-0 text-amber-600 dark:text-amber-400" />}
                              </button>
                            )
                          })}
                        </div>
                      </div>
                    )}

                    {/* Reset Button */}
                    <div className="border-t border-neutral-100 pt-2.5 mt-2.5 dark:border-zinc-800">
                      <button
                        type="button"
                        disabled={!canOperator || resettingSandbox}
                        onClick={() => void handleResetSandbox()}
                        className={cn(
                          'flex w-full items-center justify-center gap-1.5 rounded-lg bg-amber-600 px-3 py-2 text-xs font-semibold text-white shadow-xs transition hover:bg-amber-500 disabled:opacity-50'
                        )}
                      >
                        <RotateCcw className={cn('size-3.5', resettingSandbox && 'animate-spin')} />
                        <span>
                          {resettingSandbox
                            ? t('tasks.previewDrawer.sandbox.resetting', { defaultValue: '正在恢复基线…' })
                            : t('tasks.previewDrawer.sandbox.resetBtn', { defaultValue: '一键重置沙盒 (<100ms)' })}
                        </span>
                      </button>
                      <p className="mt-1.5 text-[10px] text-neutral-400 dark:text-zinc-500 leading-tight text-center">
                        {t('tasks.previewDrawer.sandbox.resetDesc', {
                          defaultValue: '秒级抹除写污染，恢复至初始基准快照。重置后自动重载预览页面。',
                        })}
                      </p>
                    </div>

                    {/* Status feedback & RBAC notice */}
                    {sandboxMsg && (
                      <div
                        className={cn(
                          'mt-2 rounded-lg px-2.5 py-1.5 text-[11px]',
                          sandboxMsg.type === 'success'
                            ? 'bg-emerald-50 text-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300'
                            : 'bg-rose-50 text-rose-800 dark:bg-rose-950/40 dark:text-rose-300'
                        )}
                      >
                        {sandboxMsg.text}
                      </div>
                    )}
                    {!canOperator && (
                      <div className="mt-2 text-[10px] text-amber-600 dark:text-amber-400 text-center">
                        {t('tasks.previewDrawer.sandbox.operatorRequired', { defaultValue: '需要项目 Operator 权限方可重置或切换场景' })}
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}
            <button
              type="button"
              onClick={handleReload}
              disabled={!isRunning}
              title={t('tasks.previewDrawer.reload', { defaultValue: '刷新预览页面' })}
              className="flex size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 disabled:opacity-30 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
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
              onClick={handleToggleCopilot}
              title={t('tasks.previewDrawer.toggleCopilot', { defaultValue: '展开/收起调优助手 (⌘K)' })}
              className={cn(
                'flex items-center gap-1.5 rounded-lg px-2.5 py-1 text-xs font-medium transition-colors',
                isCopilotActive
                  ? 'bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-300'
                  : 'text-neutral-600 hover:bg-neutral-200/60 dark:text-zinc-400 dark:hover:bg-zinc-800'
              )}
            >
              <Sparkles className="size-3.5" />
              <span>{t('tasks.previewDrawer.copilotBtn', { defaultValue: '调优助手' })}</span>
              <kbd className="rounded bg-black/5 px-1 py-0.5 text-[10px] font-mono text-neutral-500 dark:bg-white/10 dark:text-zinc-400">
                ⌘K
              </kbd>
            </button>
            <button
              type="button"
              onClick={() => setIsMaximized((v) => !v)}
              title={
                isMaximized
                  ? t('tasks.previewDrawer.exitFullscreen', { defaultValue: '还原窗口大小' })
                  : t('tasks.previewDrawer.fullscreen', { defaultValue: '全屏最大化' })
              }
              className="hidden size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100 sm:flex"
            >
              {isMaximized ? <Minimize2 className="size-4" /> : <Maximize2 className="size-4" />}
            </button>
            <button
              type="button"
              onClick={onClose}
              title={t('common.close', { defaultValue: '关闭' })}
              className="flex size-8 items-center justify-center rounded-lg text-neutral-500 hover:bg-neutral-200/60 hover:text-neutral-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
            >
              <X className="size-4" />
            </button>
          </div>
        </div>

        {/* Main Area: Iframe + Copilot (Docked or Floating) */}
        <div className="relative flex min-h-0 flex-1 overflow-hidden">
          {/* Iframe Viewport: takes 100% width in floating mode */}
          <div className={cn('relative flex-1 overflow-hidden bg-neutral-100 dark:bg-zinc-950', isInspectingDOM && 'cursor-crosshair')}>
            {previewStatus && previewStatus !== 'running' ? (
              <div className="flex size-full flex-col items-center justify-center p-6 text-center">
                <div className="flex size-14 items-center justify-center rounded-2xl bg-sky-50 text-sky-600 dark:bg-sky-950/60 dark:text-sky-400 mb-4 shadow-xs">
                  <Globe className="size-7" />
                </div>
                <h3 className="text-base font-semibold text-neutral-900 dark:text-zinc-100 mb-1">
                  {t('tasks.previewDrawer.dormantTitle', { defaultValue: '预览环境当前未运行' })}
                </h3>
                <p className="max-w-md text-xs text-neutral-500 dark:text-zinc-400 mb-5 leading-relaxed">
                  {t('tasks.previewDrawer.dormantDesc', {
                    defaultValue: '预览容器尚未启动或已休眠（30 分钟无操作自动回收）。点击下方按钮即可一键启动环境并在此处实时预览。',
                  })}
                </p>
                {onStartPreview ? (
                  <button
                    type="button"
                    onClick={() => void handleStartFromDrawer()}
                    disabled={startBusy}
                    className="inline-flex items-center gap-2 rounded-xl bg-sky-600 px-4 py-2 text-xs font-semibold text-white shadow-sm hover:bg-sky-500 disabled:opacity-50 transition"
                  >
                    <Globe className={cn('size-4', startBusy && 'animate-spin')} />
                    {startBusy ? t('tasks.startingPreview', { defaultValue: '启动中…' }) : t('tasks.previewDrawer.startNow', { defaultValue: '🚀 立即启动预览环境' })}
                  </button>
                ) : null}
              </div>
            ) : targetUrl ? (
              <iframe
                key={iframeKey}
                ref={iframeRef}
                src={targetUrl}
                title={`preview-${taskId}`}
                className={cn('size-full border-none', isInspectingDOM && 'cursor-crosshair')}
                sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
              />
            ) : (
              <div className="flex size-full items-center justify-center text-sm text-neutral-400 dark:text-zinc-600">
                {t('tasks.previewDrawer.noUrl', { defaultValue: '无可用预览地址' })}
              </div>
            )}
          </div>

          {/* Docked Sidebar Mode */}
          {layoutMode === 'docked' && copilotOpen && !isInspectingDOM && (
            <div className="flex w-88 shrink-0 flex-col border-l border-neutral-200 bg-white shadow-lg dark:border-zinc-800 dark:bg-zinc-900 sm:w-96">
              <div className="flex h-11 items-center justify-between border-b border-neutral-100 px-3.5 dark:border-zinc-800">
                <div className="flex items-center gap-2">
                  <span className={cn('size-2 rounded-full', isStreaming || isBusy ? 'bg-amber-400 animate-pulse' : 'bg-emerald-500')} />
                  <span className="text-xs font-semibold text-neutral-800 dark:text-zinc-200">
                    {t('tasks.previewDrawer.copilotTitle', { defaultValue: '智能调优助手' })}
                  </span>
                  <span className="text-[10px] font-normal text-amber-700 bg-amber-50 dark:bg-amber-950/60 dark:text-amber-300 px-1.5 py-0.5 rounded border border-amber-200 dark:border-amber-800 shrink-0">
                    只读审查
                  </span>
                </div>
                <div className="flex items-center gap-1">
                  {/* DOM Picker Button */}
                  <button
                    type="button"
                    onClick={handleToggleInspect}
                    disabled={!previewOrigin}
                    title={!previewOrigin ? '预览源未配置或不匹配，已禁用元素选取' : (isInspectingDOM ? '取消选取 (Esc)' : '在页面上点击选择目标元素')}
                    className={cn(
                      'inline-flex items-center gap-1 rounded-lg px-2 py-1 text-[11px] font-medium transition',
                      isInspectingDOM
                        ? 'bg-sky-600 text-white animate-pulse'
                        : 'bg-sky-50 text-sky-700 hover:bg-sky-100 dark:bg-sky-950/50 dark:text-sky-300',
                      !previewOrigin && 'opacity-50 cursor-not-allowed hover:bg-sky-50 dark:hover:bg-sky-950/50'
                    )}
                  >
                    <Crosshair className="size-3" />
                    <span>{isInspectingDOM ? t('tasks.previewDrawer.inspectingActive', { defaultValue: '点击元素' }) : t('tasks.previewDrawer.inspectDom', { defaultValue: '选元素' })}</span>
                  </button>
                  {msgs.length > 0 && (
                    <button
                      type="button"
                      onClick={handleClearHistory}
                      title={t('tasks.previewDrawer.clearHistory', { defaultValue: '清空会话' })}
                      className="flex size-7 items-center justify-center rounded-lg text-neutral-400 hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                    >
                      <Trash2 className="size-3.5" />
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => handleSetLayoutMode('floating')}
                    title={t('tasks.previewDrawer.switchToFloating', { defaultValue: '切换为右下角悬浮窗' })}
                    className="flex size-7 items-center justify-center rounded-lg text-neutral-400 hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                  >
                    <Layers className="size-3.5" />
                  </button>
                  <button
                    type="button"
                    onClick={() => setCopilotOpen(false)}
                    title={t('common.close', { defaultValue: '关闭' })}
                    className="flex size-7 items-center justify-center rounded-lg text-neutral-400 hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                  >
                    <X className="size-3.5" />
                  </button>
                </div>
              </div>
              {renderCopilotBody()}
            </div>
          )}

          {/* Floating Overlay Mode */}
          {layoutMode === 'floating' && copilotOpen && !copilotMinimized && !isInspectingDOM && (
            <div className="absolute bottom-4 right-4 z-30 flex h-[540px] max-h-[calc(100%-32px)] w-88 sm:w-96 flex-col overflow-hidden rounded-2xl border border-neutral-200/90 bg-white/95 shadow-2xl backdrop-blur-md dark:border-zinc-700/80 dark:bg-zinc-900/95 transition-all">
              <div className="flex h-11 shrink-0 items-center justify-between border-b border-neutral-200/70 bg-neutral-50/80 px-3.5 dark:border-zinc-800 dark:bg-zinc-800/60">
                <div className="flex items-center gap-2 overflow-hidden">
                  <span className={cn('size-2 rounded-full shrink-0', isStreaming || isBusy ? 'bg-amber-400 animate-pulse' : 'bg-emerald-500')} />
                  <span className="truncate text-xs font-semibold text-neutral-800 dark:text-zinc-200">
                    {t('tasks.previewDrawer.copilotTitle', { defaultValue: '智能调优助手' })}
                  </span>
                  <span className="text-[10px] font-normal text-amber-700 bg-amber-50 dark:bg-amber-950/60 dark:text-amber-300 px-1.5 py-0.5 rounded border border-amber-200 dark:border-amber-800 shrink-0">
                    只读审查
                  </span>
                </div>
                <div className="flex items-center gap-1">
                  {/* DOM Picker Button */}
                  <button
                    type="button"
                    onClick={handleToggleInspect}
                    disabled={!previewOrigin}
                    title={!previewOrigin ? '预览源未配置或不匹配，已禁用元素选取' : (isInspectingDOM ? '取消选取 (Esc)' : '在页面上点击选择目标元素')}
                    className={cn(
                      'inline-flex items-center gap-1 rounded-lg px-2 py-0.5 text-[11px] font-medium transition',
                      isInspectingDOM
                        ? 'bg-sky-600 text-white animate-pulse'
                        : 'bg-sky-50 text-sky-700 hover:bg-sky-100 dark:bg-sky-950/50 dark:text-sky-300',
                      !previewOrigin && 'opacity-50 cursor-not-allowed hover:bg-sky-50 dark:hover:bg-sky-950/50'
                    )}
                  >
                    <Crosshair className="size-3" />
                    <span>{isInspectingDOM ? t('tasks.previewDrawer.inspectingActive', { defaultValue: '点击元素' }) : t('tasks.previewDrawer.inspectDom', { defaultValue: '选元素' })}</span>
                  </button>
                  {msgs.length > 0 && (
                    <button
                      type="button"
                      onClick={handleClearHistory}
                      title={t('tasks.previewDrawer.clearHistory', { defaultValue: '清空会话' })}
                      className="flex size-6 items-center justify-center rounded-md text-neutral-400 hover:bg-neutral-200/60 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                    >
                      <Trash2 className="size-3.5" />
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => handleSetLayoutMode('docked')}
                    title={t('tasks.previewDrawer.switchToDocked', { defaultValue: '切换为靠右分屏' })}
                    className="flex size-6 items-center justify-center rounded-md text-neutral-400 hover:bg-neutral-200/60 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                  >
                    <PanelRight className="size-3.5" />
                  </button>
                  <button
                    type="button"
                    onClick={() => setCopilotMinimized(true)}
                    title={t('tasks.previewDrawer.minimize', { defaultValue: '最小化' })}
                    className="flex size-6 items-center justify-center rounded-md text-neutral-400 hover:bg-neutral-200/60 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                  >
                    <Minus className="size-3.5" />
                  </button>
                  <button
                    type="button"
                    onClick={() => setCopilotOpen(false)}
                    title={t('common.close', { defaultValue: '关闭' })}
                    className="flex size-6 items-center justify-center rounded-md text-neutral-400 hover:bg-neutral-200/60 hover:text-neutral-700 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                  >
                    <X className="size-3.5" />
                  </button>
                </div>
              </div>
              {renderCopilotBody()}
            </div>
          )}

          {/* Inspecting DOM Header Banner */}
          {isInspectingDOM && (
            <div className="pointer-events-none absolute top-4 left-1/2 -translate-x-1/2 z-40 flex items-center gap-3 rounded-full border border-sky-400/40 bg-slate-900/95 px-5 py-2 text-xs font-semibold text-white shadow-2xl backdrop-blur-md animate-in fade-in zoom-in duration-150">
              <span className="relative flex size-2.5 items-center justify-center">
                <span className={cn('size-2 rounded-full', inspectorConnected ? 'bg-sky-400' : 'bg-amber-400 animate-pulse')} />
                <span className={cn('absolute size-3.5 rounded-full animate-ping', inspectorConnected ? 'bg-sky-400/50' : 'bg-amber-400/50')} />
              </span>
              <span>
                {inspectorConnected
                  ? t('tasks.previewDrawer.inspectingBanner', { defaultValue: '🎯 请在页面上点击需要修改的目标元素' })
                  : t('tasks.previewDrawer.connectingInspector', { defaultValue: '🎯 正在激活页面选取模式…' })}
              </span>
              {!inspectorConnected && (
                <button
                  type="button"
                  onClick={handleReload}
                  className="pointer-events-auto rounded-full bg-amber-500/25 px-2.5 py-0.5 text-[11px] font-medium text-amber-200 hover:bg-amber-500/40 transition cursor-pointer"
                  title="未检测到页面响应？点击重新加载预览页面"
                >
                  刷新重试
                </button>
              )}
              <button
                type="button"
                onClick={handleStopInspect}
                className="pointer-events-auto rounded-full bg-white/15 px-3 py-1 text-[11px] font-medium text-white/90 hover:bg-white/25 transition cursor-pointer"
              >
                {t('common.cancel', { defaultValue: '取消 (Esc)' })}
              </button>
            </div>
          )}

          {/* Classic Bottom-Right Floating Pill Badge */}
          {(!copilotOpen || copilotMinimized) && !isInspectingDOM && (
            <button
              type="button"
              onClick={() => {
                setCopilotOpen(true)
                setCopilotMinimized(false)
              }}
              title={t('tasks.previewDrawer.expandCopilot', { defaultValue: '展开智能调优助手 (⌘K)' })}
              className="group absolute bottom-4 right-4 z-30 flex items-center gap-2 rounded-full border border-sky-300/40 bg-slate-900/90 px-3.5 py-2 text-xs font-semibold text-white shadow-2xl backdrop-blur-md transition-all hover:scale-105 active:scale-95 hover:bg-slate-800 dark:border-sky-700/50"
            >
              <span className="relative flex size-2.5 items-center justify-center">
                <span
                  className={cn(
                    'size-2.5 rounded-full transition-colors',
                    isStreaming || isBusy ? 'bg-amber-400 animate-pulse' : 'bg-emerald-400'
                  )}
                />
                {(isStreaming || isBusy) && (
                  <span className="absolute size-4 rounded-full bg-amber-400/40 animate-ping" />
                )}
              </span>
              <span>{t('tasks.previewDrawer.copilotBtn', { defaultValue: '调优助手' })}</span>
              <kbd className="hidden sm:inline-block rounded bg-white/15 px-1.5 py-0.5 text-[10px] font-mono text-white/80">
                ⌘K
              </kbd>
            </button>
          )}
        </div>
      </div>
    </div>,
    document.body
  )
}

