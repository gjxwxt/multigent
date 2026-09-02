import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { ExternalLink, GitBranch, Globe, Play, RefreshCw, X } from 'lucide-react'
import { WorkflowBoard } from '../../components/workflow/WorkflowBoard'
import { ConversationLog } from '../../components/ui/ConversationLog'
import { PlaceholderCard } from '../../components/ui/PlaceholderCard'
import {
  WorkflowRuntimePanel,
  activeWorkflowStepInstance,
  isOptionalTerminalReviewDecision,
  isTerminal,
  isWorkflowStepOpen,
  startableAgentName,
  statusColor,
  taskIdentityLabel,
  workflowHistoryRecords,
  type RunRow,
  type TaskRow,
  type TaskWorkflowData,
} from '../../components/task/TaskModals'
import { apiFetch, apiPost } from '../../lib/api'
import { canOperateAgent, useAuth } from '../../lib/auth'
import { DesignGateFlow } from '../../components/design/DesignGateFlow'
import { cn } from '../../lib/cn'
import { useFormatDateTime } from '../../lib/format-datetime'
import { useApiJson } from '../../lib/use-api'
import { useWorkspaceAccess } from '../../lib/workspace-access'
import { showToast } from '../../components/ui/Toast'
import { isAtBottom, stickToBottom } from '../../utils/stickToBottom'

type SafeUser = { username: string; displayName?: string; email?: string }
type ProjectMember = { name: string; model?: string; avatar?: string }
type LogData = { content: string; truncated: boolean }
type LiveLogData = { content: string; path: string; finished: boolean }

const silentNotFound = [404]
const silentForbidden = [403]
const staticSilentForbiddenOptions = { silentStatuses: silentForbidden }
const FOLLOW_POLL_MS = 3500

export default function ProjectTaskFollowPage() {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()
  const { user } = useAuth()
  const { canAdmin } = useWorkspaceAccess()
  const { projectId = '', taskId = '' } = useParams<{ projectId: string; taskId: string }>()
  const [reloadKey, setReloadKey] = useState(0)
  const [pollKey, setPollKey] = useState(0)
  const [reviewComments, setReviewComments] = useState('')
  const [reviewOutputs, setReviewOutputs] = useState<Record<string, string>>({})
  const [reviewBusy, setReviewBusy] = useState<string | null>(null)
  const [reviewErr, setReviewErr] = useState<string | null>(null)
  const [missingReviewField, setMissingReviewField] = useState<string | null>(null)
  const [startBusy, setStartBusy] = useState(false)
  const [optimisticStartedAt, setOptimisticStartedAt] = useState<string | null>(null)
  const [handoffStartedAt, setHandoffStartedAt] = useState<string | null>(null)
  const [showLiveTail, setShowLiveTail] = useState(false)
  const [stepTransition, setStepTransition] = useState<{ from?: string; to: string; at: number } | null>(null)
  const activeStepRef = useRef<string | null>(null)
  const sidePanelRef = useRef<HTMLElement>(null)
  const liveSectionRef = useRef<HTMLElement>(null)
  const liveOutputRef = useRef<HTMLDivElement>(null)
  // Follow-mode flags: heartbeat updates may only pull the view to the
  // bottom while the reader is already there (tracked from scroll events).
  const outputStickRef = useRef(true)
  const panelStickRef = useRef(true)

  const refresh = useCallback(() => setReloadKey((value) => value + 1), [])
  const pollRefresh = useCallback(() => setPollKey((value) => value + 1), [])
  const dataReloadKey = reloadKey + pollKey
  const tasksState = useApiJson<TaskRow[]>(
    projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}/tasks?scope=all` : null,
    dataReloadKey,
    { keepPreviousDataOnReload: true },
  )
  const workflowState = useApiJson<TaskWorkflowData>(
    projectId && taskId ? `/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(taskId)}/workflow` : null,
    dataReloadKey,
    { silentStatuses: silentNotFound, keepPreviousDataOnReload: true },
  )
  const usersState = useApiJson<SafeUser[]>('/api/v1/users', 0, staticSilentForbiddenOptions)
  const membersState = useApiJson<ProjectMember[]>(
    projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}/agents` : null,
    0,
    staticSilentForbiddenOptions,
  )

  const task = tasksState.status === 'ok' ? (tasksState.data ?? []).find((item) => item.id === taskId) : undefined
  const rawWorkflowData = workflowState.status === 'ok' ? workflowState.data : null
  const rawActiveStep = rawWorkflowData
    ? rawWorkflowData.definition.steps.find((step) => step.id === rawWorkflowData.run.activeStepId)
    : undefined
  const rawActiveInstance = rawWorkflowData ? activeWorkflowStepInstance(rawWorkflowData) : undefined
  const rawIsAgentStep = rawActiveInstance?.actorType === 'agent' || rawActiveStep?.type === 'agent_task'
  const optimisticRunStartedAt = optimisticStartedAt
  const displayTask = useMemo(() => {
    if (!task || task.status === 'in_progress' || isTerminal(task.status)) return task
    if (!optimisticRunStartedAt) return task
    const startedAt = rawActiveInstance?.startedAt || optimisticRunStartedAt || task.startedAt || new Date().toISOString()
    return { ...task, status: 'in_progress', startedAt, updatedAt: task.updatedAt || startedAt }
  }, [optimisticRunStartedAt, rawActiveInstance?.startedAt, task])
  const workflowData = useMemo(
    () => withRunningActiveStep(rawWorkflowData, displayTask, projectId, optimisticRunStartedAt, Boolean(handoffStartedAt && rawIsAgentStep && task?.status === 'in_progress')),
    [displayTask, handoffStartedAt, optimisticRunStartedAt, projectId, rawIsAgentStep, rawWorkflowData, task?.status],
  )
  const activeInstance = workflowData ? activeWorkflowStepInstance(workflowData) : undefined
  const activeStep = workflowData
    ? workflowData.definition.steps.find((step) => step.id === (activeInstance?.stepId || workflowData.run.activeStepId))
    : undefined
  const isCurrentAgentStep = activeInstance?.actorType === 'agent' || activeStep?.type === 'agent_task'
  const currentTaskAgent = displayTask ? startableAgentName(displayTask) : null
  const currentStepAgent = isCurrentAgentStep
    ? agentNameFromActor(projectId, activeInstance?.actorId) || workflowActorAgentForStep(workflowData?.run.actorBindings, activeStep)
    : null
  const startAgent = currentStepAgent || (isCurrentAgentStep ? currentTaskAgent : null)
  const activeStepRunning = isCurrentAgentStep && isRunningStepStatus(activeInstance?.status)
  const runsState = useApiJson<{ runs: RunRow[] }>(
    isCurrentAgentStep && projectId ? `/api/v1/telemetry/runs?allTime=1&project=${encodeURIComponent(projectId)}&limit=200` : null,
    dataReloadKey,
    { keepPreviousDataOnReload: true },
  )
  const runs = runsState.status === 'ok' ? (runsState.data.runs ?? []) : []
  const activeRun = useMemo(
    () => isCurrentAgentStep ? findActiveRun(runs, taskId, projectId, activeInstance?.actorId, activeInstance?.startedAt) : null,
    [activeInstance?.actorId, activeInstance?.startedAt, isCurrentAgentStep, projectId, runs, taskId],
  )
  const activeRunRunning = Boolean(activeRun && isRunningRunStatus(activeRun.status))
  const taskRunning = displayTask?.status === 'in_progress'
  const effectiveStepRunning = Boolean(activeStepRunning && (taskRunning || activeRunRunning))
  const displayStatus = activeRunRunning || effectiveStepRunning ? 'in_progress' : (displayTask?.status || '')
  const shouldPollActiveRun = Boolean(isCurrentAgentStep && (effectiveStepRunning || taskRunning || activeRunRunning))
  const visibleWorkflowData = useMemo(
    () => withIdleAgentStepPending(workflowData, displayTask, projectId, Boolean(activeRunRunning)),
    [activeRunRunning, displayTask, projectId, workflowData],
  )
  const visibleActiveInstance = visibleWorkflowData ? activeWorkflowStepInstance(visibleWorkflowData) : undefined
  const visibleWorkflowRecords = visibleWorkflowData ? workflowHistoryRecords(visibleWorkflowData) : []
  const shouldPollLocalLiveLog = Boolean(shouldPollActiveRun && startAgent && !activeRun?.runtimeRunId)
  const localLiveLogAgent = shouldPollLocalLiveLog && startAgent ? startAgent : null
  const liveLogState = useApiJson<LiveLogData>(
    localLiveLogAgent ? `/api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(localLiveLogAgent)}/live-log` : null,
    dataReloadKey,
    { keepPreviousDataOnReload: true },
  )
  const logState = useApiJson<LogData>(
    isCurrentAgentStep && !shouldPollActiveRun && activeRun?.logPath ? `/api/v1/telemetry/log?path=${encodeURIComponent(activeRun.logPath)}` : null,
    dataReloadKey,
    { keepPreviousDataOnReload: true },
  )
  const liveLogContent = liveLogState.status === 'ok' ? liveLogState.data.content : ''
  const remoteLogContent = activeRun?.logText || ''
  const liveLogFinished = liveLogState.status === 'ok' && (liveLogState.data.finished || isLiveLogTerminal(liveLogContent))

  const actorLabels = useMemo(() => {
    const labels = new Map<string, string>()
    if (usersState.status === 'ok') {
      for (const item of usersState.data ?? []) {
        labels.set(item.username, item.displayName || item.email || item.username)
      }
    }
    if (membersState.status === 'ok') {
      for (const member of membersState.data ?? []) {
        if (!member.name) continue
        if (member.model === 'human') {
          labels.set(member.name, labels.get(member.name) || member.name)
        } else {
          labels.set(`${projectId}/${member.name}`, member.name)
        }
      }
    }
    if (displayTask?.assignee) labels.set(displayTask.assignee, taskIdentityLabel(displayTask.assignee, displayTask.assigneeLabel))
    return labels
  }, [displayTask?.assignee, displayTask?.assigneeLabel, membersState, projectId, usersState])
  const agentParticipants = useMemo(() => {
    const participants = new Map<string, { name: string; avatar?: string }>()
    if (membersState.status !== 'ok') return participants
    for (const member of membersState.data ?? []) {
      if (!member.name || member.model === 'human') continue
      const participant = { name: member.name, avatar: member.avatar }
      participants.set(member.name, participant)
      participants.set(`${projectId}/${member.name}`, participant)
    }
    return participants
  }, [membersState, projectId])
  const activeAgentParticipant = useMemo(() => {
    const name = startAgent || activeRun?.agent || t('tasks.noActiveRun')
    const participant = agentParticipants.get(name) || agentParticipants.get(agentNameFromActor(projectId, name) || '')
    return participant || { name: agentNameFromActor(projectId, name) || name }
  }, [activeRun?.agent, agentParticipants, projectId, startAgent, t])
  const liveOutputRunning = Boolean(
    shouldPollActiveRun &&
    !remoteLogContent.trim() &&
    displayStatus === 'in_progress' &&
    (!liveLogState || liveLogState.status !== 'ok' || !liveLogFinished),
  )

  const isFailedOrCancelled = displayTask?.status === 'done_failed' || displayTask?.status === 'cancelled'
  const canStart = Boolean(
    displayTask &&
    startAgent &&
    displayStatus !== 'in_progress' &&
    (!isTerminal(displayTask.status) || isFailedOrCancelled) &&
    (canAdmin || canOperateAgent(user, projectId, startAgent)),
  )
  const canReview = Boolean(activeStep?.type === 'human_review' && isWorkflowStepOpen(activeInstance?.status) && !isTerminal(displayTask?.status || ''))
  const isDesignGate = Boolean(canReview && activeStep?.config?.designGate === 'true')
  const [designGateOpen, setDesignGateOpen] = useState(false)
  const [designGateStage, setDesignGateStage] = useState<'choose' | 'review'>('choose')
  // Probe for an existing design session so the follow page can offer a
  // one-click "open canvas" shortcut next to the source chooser. The status
  // endpoint re-mints signed URLs on every call, so no start roundtrip is
  // needed — if it reports a project, the review modal can recover directly.
  const [designSession, setDesignSession] = useState<{ projectId: string } | null>(null)
  useEffect(() => {
    if (!isDesignGate) {
      setDesignSession(null)
      return
    }
    let alive = true
    apiFetch<{ projectId?: string }>(
      `/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(taskId || '')}/design/status`,
      { silentStatuses: [401, 403, 404, 500, 502, 503, 504] },
    )
      .then((d) => {
        if (alive) setDesignSession(d?.projectId ? { projectId: d.projectId } : null)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [isDesignGate, projectId, taskId, designGateOpen])

  useEffect(() => {
    if (!shouldPollActiveRun) return
    const timer = window.setInterval(pollRefresh, FOLLOW_POLL_MS)
    return () => window.clearInterval(timer)
  }, [pollRefresh, shouldPollActiveRun])

  useEffect(() => {
    activeStepRef.current = null
    setHandoffStartedAt(null)
    setOptimisticStartedAt(null)
  }, [taskId])

  useEffect(() => {
    const activeStepID = rawWorkflowData?.run.activeStepId || null
    if (!activeStepID) return
    const previousStepID = activeStepRef.current
    activeStepRef.current = activeStepID
    if (!previousStepID || previousStepID === activeStepID) return
    setStepTransition({ from: previousStepID, to: activeStepID, at: Date.now() })
    if (rawIsAgentStep && rawActiveInstance?.status === 'running' && task?.status === 'in_progress') {
      setHandoffStartedAt(new Date().toISOString())
    }
  }, [rawActiveInstance?.status, rawIsAgentStep, rawWorkflowData?.run.activeStepId, task])

  useEffect(() => {
    if (!stepTransition) return
    const timer = window.setTimeout(() => setStepTransition(null), 1200)
    return () => window.clearTimeout(timer)
  }, [stepTransition])

  useEffect(() => {
    if (!optimisticStartedAt && !handoffStartedAt) return
    if (!task || task.status === 'in_progress' || isTerminal(task.status)) {
      setOptimisticStartedAt(null)
      setHandoffStartedAt(null)
    }
  }, [handoffStartedAt, optimisticStartedAt, task, task?.status])

  useEffect(() => {
    if (!handoffStartedAt || task?.status === 'in_progress') return
    const timer = window.setTimeout(() => {
      setHandoffStartedAt(null)
      refresh()
    }, 20000)
    return () => window.clearTimeout(timer)
  }, [handoffStartedAt, refresh, task?.status])

  useEffect(() => {
    setReviewComments('')
    setReviewOutputs({})
    setReviewErr(null)
    // New step/run: re-engage follow mode so the fresh output is visible.
    outputStickRef.current = true
    panelStickRef.current = true
  }, [activeStep?.id, activeInstance?.id])

  useEffect(() => {
    if (!liveOutputRunning) {
      setShowLiveTail(false)
      return
    }
    if (!liveLogContent.trim()) {
      setShowLiveTail(true)
      return
    }
    setShowLiveTail(false)
    const delay = Math.min(2200, Math.max(900, liveLogContent.length * 8))
    const timer = window.setTimeout(() => setShowLiveTail(true), delay)
    return () => window.clearTimeout(timer)
  }, [liveLogContent, liveOutputRunning])

  // Inner live-output: follow the tail on heartbeat updates, but only while
  // the reader is at the bottom — scrolling up to re-read must not be hijacked.
  // The latest message also types out over ~0.8s after each poll, so keep
  // re-sticking on a short interval while the run is streaming.
  useEffect(() => {
    if (!shouldPollActiveRun || !liveOutputRef.current) return
    const outputEl = liveOutputRef.current
    const scroll = () => {
      if (outputStickRef.current) stickToBottom(outputEl)
    }
    scroll()
    const raf = window.requestAnimationFrame(scroll)
    const timer = window.setTimeout(scroll, 80)
    const tail = liveOutputRunning ? window.setInterval(scroll, 300) : null
    return () => {
      window.cancelAnimationFrame(raf)
      window.clearTimeout(timer)
      if (tail) window.clearInterval(tail)
    }
  }, [activeRun?.logPath, activeRun?.sessionId, activeStep?.id, liveLogContent, liveOutputRunning, remoteLogContent, shouldPollActiveRun])

  // Outer side panel: reveal the live-output section when a new run/step
  // starts, and keep following content updates while the reader stays parked
  // at the bottom — otherwise the growing live-output box slides below the
  // panel fold and its newest lines are never seen. Once the reader scrolls
  // up (panelStickRef false) heartbeat updates must not yank the panel.
  useEffect(() => {
    if (!shouldPollActiveRun) return
    const panelEl = sidePanelRef.current
    if (panelEl && panelStickRef.current) stickToBottom(panelEl)
  }, [activeRun?.sessionId, activeRun?.logPath, activeStep?.id, liveLogContent, remoteLogContent, shouldPollActiveRun])

  async function startCurrentAgent() {
    if (!displayTask || !startAgent || !canStart) return
    setStartBusy(true)
    setOptimisticStartedAt(new Date().toISOString())
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(displayTask.project)}/tasks/${encodeURIComponent(displayTask.id)}/start`, {}, { suppressToast: true })
      refresh()
      window.setTimeout(refresh, 1000)
    } catch (err) {
      setOptimisticStartedAt(null)
      const msg = err instanceof Error ? err.message : String(err)
      if (msg.includes('already running') || msg.includes('scheduler_wakeup_failed') || msg.includes('conflict') || msg.includes('忙碌') || msg.includes('正在')) {
        showToast(t('tasks.agentAlreadyRunning', { defaultValue: '智能体已在后台执行该任务中，正在处理…' }), 'info')
        refresh()
      } else {
        showToast(msg || t('apiErrors.bad_request'), 'error')
      }
    } finally {
      setStartBusy(false)
    }
  }

  async function submitWorkflowReview(decision?: string) {
    await submitWorkflowReviewWithOutputs(reviewOutputs, decision)
  }

  // Review submit that merges caller-supplied outputs (the design gate flows
  // in approved_design_* fields) on top of the form state.
  async function submitWorkflowReviewWithOutputs(extraOutputs: Record<string, string>, decision?: string) {
    if (!displayTask || !activeStep) return
    setReviewErr(null)
    setMissingReviewField(null)
    const merged = { ...reviewOutputs, ...Object.fromEntries(Object.entries(extraOutputs).filter(([, v]) => String(v ?? '').trim() !== '')) }
    const normalizedDecision = normalizeReviewDecision(decision || merged.decision || '')
    setReviewBusy(normalizedDecision || 'submit')
    const outputs: Record<string, string> = Object.fromEntries(Object.entries(merged).map(([key, value]) => [key, String(value ?? '').trim()]))
    if (normalizedDecision) outputs.decision = normalizedDecision
    const outputFieldNames = (activeStep.outputFields ?? []).map((field) => field.name).filter(Boolean)
    const comments = (outputs.comments ?? reviewComments).trim()
    if (outputFieldNames.includes('comments')) outputs.comments = comments
    const decisionOptional = isOptionalTerminalReviewDecision(activeStep, workflowData?.definition)
    // Optional fields never block submit: left empty, the server backfills
    // them from the deterministic draft rules.
    const missingField = (activeStep.outputFields ?? [])
      .filter((field) => field.name && !field.optional)
      .map((field) => field.name)
      .find((name) => !(name === 'decision' && decisionOptional) && !String(outputs[name] ?? '').trim())
    if (missingField) {
      setMissingReviewField(missingField)
      const msg = `${t('forms.fillRequired')} ${missingField}`
      setReviewErr(msg)
      setReviewBusy(null)
      // Throw so awaited callers (design gate submit) keep their modal open and
      // can surface the block — resolving silently made the gate look approved.
      throw new Error(msg)
    }
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(displayTask.project)}/tasks/${encodeURIComponent(displayTask.id)}/workflow/review`, {
        decision: normalizedDecision,
        comments,
        outputs,
      })
      setReviewComments('')
      setReviewOutputs({})
      setHandoffStartedAt(new Date().toISOString())
      refresh()
      window.setTimeout(refresh, 300)
      window.setTimeout(refresh, 800)
      window.setTimeout(refresh, 1600)
    } catch (e) {
      setReviewErr(e instanceof Error ? e.message : String(e))
      throw e
    } finally {
      setReviewBusy(null)
    }
  }

  const previewState = useApiJson<{ taskId: string; type: string; status: string; url: string; previewToken?: string }>(
    projectId && taskId ? `/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(taskId)}/preview` : null,
    5000,
  )
  const [previewStarting, setPreviewStarting] = useState(false)

  const preview = previewState.status === 'ok' ? previewState.data : null

  return (
    <div className="fixed inset-0 z-[70] flex flex-col bg-neutral-50 text-neutral-900 dark:bg-zinc-950 dark:text-zinc-100">
      <header className="flex h-14 shrink-0 items-center justify-between border-b border-neutral-200/80 bg-white/90 px-4 backdrop-blur-md dark:border-zinc-800 dark:bg-zinc-950/90">
        <div className="min-w-0 pr-4">
          <div className="flex items-center gap-2 text-xs text-neutral-400 dark:text-zinc-500">
            <Link to={`/projects/${encodeURIComponent(projectId)}/tasks`} className="hover:text-sky-700 dark:hover:text-sky-400">
              {t('projectNav.tasks')}
            </Link>
            <span>/</span>
            <span>{t('tasks.follow')}</span>
          </div>
          <h1 className="truncate text-sm font-semibold text-neutral-900 dark:text-zinc-100">{displayTask?.title || taskId}</h1>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {(() => {
            const isFeatureBranch = Boolean(displayTask?.branchName || displayTask?.worktreeDir)
            const taskBranch = displayTask?.branchName || (displayTask?.worktreeDir ? `feature/${displayTask.id}` : displayTask?.baseBranch || 'main')
            return (
              <div className="hidden sm:inline-flex items-center gap-1.5 font-mono text-xs text-sky-800 bg-sky-50 dark:bg-sky-950/60 dark:text-sky-300 px-2.5 py-1 rounded-lg border border-sky-200/80 dark:border-sky-800 shadow-xs">
                {isFeatureBranch ? (
                  <>
                    <span className="flex items-center gap-1 font-semibold text-sky-700 dark:text-sky-300">
                      {displayTask?.baseBranch || 'main'}
                      {displayTask?.baseCommit ? (
                        <span className="text-[11px] font-normal text-sky-600/80 dark:text-sky-400/80">({displayTask.baseCommit.slice(0, 7)})</span>
                      ) : null}
                    </span>
                    <span className="text-sky-400 dark:text-sky-600 font-bold select-none">──►</span>
                    <span className="truncate max-w-[220px]" title={taskBranch}>
                      {taskBranch}
                    </span>
                  </>
                ) : (
                  <span className="flex items-center gap-1 font-semibold text-sky-700 dark:text-sky-300">
                    <GitBranch className="size-3.5 text-sky-600 dark:text-sky-400" />
                    <span>{taskBranch}</span>
                  </span>
                )}
              </div>
            )
          })()}
          {preview && (preview.status === 'running' || preview.type !== 'cli') && (
            <div className="flex items-center">
              {preview.status === 'running' ? (
                <a
                  href={preview.previewToken ? `${preview.url}?pvt=${encodeURIComponent(preview.previewToken)}` : preview.url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1.5 rounded-lg border border-emerald-500/30 bg-emerald-50 px-2.5 py-1.5 text-xs font-semibold text-emerald-700 shadow-sm transition hover:bg-emerald-100 dark:border-emerald-500/30 dark:bg-emerald-950/40 dark:text-emerald-300"
                >
                  <span className="size-2 animate-pulse rounded-full bg-emerald-500" />
                  打开实时预览 ↗
                </a>
              ) : (
                <button
                  type="button"
                  onClick={async () => {
                    setPreviewStarting(true)
                    try {
                      const inst = await apiPost<{ previewToken?: string }>(`/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(taskId)}/preview/start`, {})
                      const tokenQS = inst?.previewToken ? `?pvt=${encodeURIComponent(inst.previewToken)}` : ''
                      window.open(`/preview/${encodeURIComponent(taskId)}/${tokenQS}`, '_blank')
                    } finally {
                      setPreviewStarting(false)
                    }
                  }}
                  disabled={previewStarting}
                  className={cn(
                    'inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs font-semibold transition',
                    canReview
                      ? 'border-sky-500 bg-sky-600 text-white shadow-sm hover:bg-sky-700 ring-2 ring-sky-400/40 dark:bg-sky-600 dark:text-white'
                      : 'border-sky-600/30 bg-sky-50 text-sky-700 hover:bg-sky-100 dark:border-sky-500/30 dark:bg-sky-950/40 dark:text-sky-300',
                  )}
                >
                  <Globe className={cn('size-3.5', previewStarting && 'animate-spin')} />
                  {previewStarting ? '启动中…' : canReview ? '打开审核预览 ↗' : '启动实时预览'}
                </button>
              )}
            </div>
          )}
          {displayTask && (
            <span className={cn('rounded-full px-2.5 py-1 text-xs font-medium', statusColor[displayStatus] ?? statusColor.pending)}>
              {displayStatus === 'in_progress' && <span className="mr-1.5 inline-block size-1.5 animate-pulse rounded-full bg-current align-middle" />}
              {t(`tasks.status.${displayStatus}`, { defaultValue: displayStatus })}
            </span>
          )}
          <button
            type="button"
            onClick={refresh}
            title={t('common.refresh')}
            className="rounded-lg border border-neutral-200 bg-white p-1.5 text-neutral-500 hover:bg-neutral-50 hover:text-neutral-800 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
          >
            <RefreshCw className="size-4" />
          </button>
          <Link to={`/projects/${encodeURIComponent(projectId)}/tasks`} className="rounded-lg border border-neutral-200 bg-white p-1.5 text-neutral-500 hover:bg-neutral-50 hover:text-neutral-800 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100">
            <X className="size-4" />
          </Link>
        </div>
      </header>

      <main className="grid min-h-0 flex-1 grid-cols-[minmax(0,1fr)_420px] gap-0 overflow-hidden">
        <section className="min-w-0 border-r border-neutral-200/80 bg-white dark:border-zinc-800 dark:bg-zinc-950">
          {visibleWorkflowData ? (
            <WorkflowBoard key={`${visibleWorkflowData.run.id}:${visibleWorkflowData.run.activeStepId || ''}`} definition={visibleWorkflowData.definition} run={visibleWorkflowData.run} instances={visibleWorkflowData.steps} branches={visibleWorkflowData.branches} focusActive fill hideInspector />
          ) : workflowState.status === 'loading' ? (
            <CenteredLoading label={t('tasks.followLoadingWorkflow')} />
          ) : (
            <div className="flex h-full items-center justify-center p-8">
              <PlaceholderCard title={t('tasks.noWorkflowTitle')}>
                <p>{t('tasks.noWorkflowBody')}</p>
              </PlaceholderCard>
            </div>
          )}
        </section>

        <aside ref={sidePanelRef} onScroll={(e) => { panelStickRef.current = isAtBottom(e.currentTarget) }} className="min-w-0 overflow-y-auto overflow-x-hidden bg-white dark:bg-zinc-950">
          {stepTransition && (
            <div className="sticky top-0 z-10 border-b border-sky-100 bg-sky-50/95 px-4 py-2 text-xs font-medium text-sky-700 shadow-sm backdrop-blur-sm dark:border-sky-900/50 dark:bg-sky-950/80 dark:text-sky-300">
              <div className="flex items-center gap-2">
                <span className="size-1.5 rounded-full bg-sky-500" />
                <span>{t('tasks.followHandoff', { defaultValue: 'Moving to the next step…' })}</span>
              </div>
            </div>
          )}
          <div className="border-b border-neutral-200/80 px-4 py-3 dark:border-zinc-800">
            <p className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('tasks.followCurrent')}</p>
            <h2 className="mt-1 text-base font-semibold text-neutral-900 dark:text-zinc-100">{activeStep?.title || t('workflows.detail.notSpecified')}</h2>
            <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-neutral-500 dark:text-zinc-500">
              {visibleActiveInstance?.actorId && <span>{t('tasks.colAssignee')}: {actorLabels.get(visibleActiveInstance.actorId) || visibleActiveInstance.actorId}</span>}
              {visibleActiveInstance?.status && <span>{t('api.taskColStatus')}: {t(`workflows.stepStatus.${visibleActiveInstance.status}`, { defaultValue: visibleActiveInstance.status })}</span>}
              {activeRun?.startedAt && <span>{t('tasks.startedAt')}: {fmt(activeRun.startedAt)}</span>}
            </div>
            {isCurrentAgentStep && (
              <div className="mt-3 flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => void startCurrentAgent()}
                  disabled={!canStart || startBusy || displayStatus === 'in_progress'}
                  className={cn(
                    'rounded-lg border px-3 py-2 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40',
                    isFailedOrCancelled
                      ? 'border-amber-600 bg-amber-50 text-amber-800 hover:bg-amber-100 dark:border-amber-500 dark:bg-amber-950/40 dark:text-amber-300'
                      : 'border-sky-600 bg-white text-sky-700 hover:bg-sky-50 dark:border-sky-500 dark:bg-zinc-900 dark:text-sky-400 dark:hover:bg-zinc-800'
                  )}
                >
                  <span className="inline-flex items-center gap-1.5">
                    {isFailedOrCancelled ? (
                      <RefreshCw className={cn('size-3.5', (startBusy || displayStatus === 'in_progress') && 'animate-spin')} />
                    ) : (
                      <Play className={cn('size-3.5', (startBusy || displayStatus === 'in_progress') && 'animate-pulse')} />
                    )}
                    {displayStatus === 'in_progress' ? t('tasks.status.in_progress') : startBusy ? t('tasks.starting') : isFailedOrCancelled ? '重新执行任务' : t('tasks.startCurrentAgent')}
                  </span>
                </button>
                {startAgent && <span className="font-mono text-xs text-neutral-400 dark:text-zinc-500">{startAgent}</span>}
              </div>
            )}
          </div>

          {visibleWorkflowData && (
            <>
              {/* The gate replaces only the decision editor, never the context:
                  inputs and previous step outputs stay visible like any other
                  step; the chooser card below is the sole submit entry. */}
              <WorkflowRuntimePanel
                step={activeStep}
                instance={visibleActiveInstance}
                steps={visibleWorkflowData.definition.steps}
                records={visibleWorkflowRecords}
                runs={runs}
                taskID={taskId}
                project={projectId}
                actorLabels={actorLabels}
                canReview={canReview && !isDesignGate}
                hideHeader
                reviewOutputs={reviewOutputs}
                reviewComments={reviewComments}
                reviewBusy={reviewBusy}
                reviewErr={reviewErr}
                missingReviewField={missingReviewField}
                docTitles={visibleWorkflowData.docTitles}
                onChangeOutput={(name, value) => {
                  if (missingReviewField === name && String(value ?? '').trim()) setMissingReviewField(null)
                  setReviewOutputs((current) => ({ ...current, [name]: value }))
                  if (name === 'comments') setReviewComments(value)
                }}
                onChangeComments={(value) => {
                  if (missingReviewField === 'comments' && value.trim()) setMissingReviewField(null)
                  setReviewComments(value)
                }}
                // reviewErr/missingReviewField are rendered by the panel itself;
                // swallow the rejection so it doesn't surface as unhandled.
                onSubmitReview={(decision) => submitWorkflowReview(decision).catch(() => {})}
              />
              {isDesignGate && (
                <section className="mx-4 mb-4 rounded-xl border border-neutral-200 bg-white px-4 py-4 dark:border-zinc-700 dark:bg-zinc-950">
                  <p className="text-sm text-neutral-700 dark:text-zinc-300">{activeStep?.description}</p>
                  <p className="mt-1 text-xs text-neutral-500 dark:text-zinc-400">
                    {t('designGate.openHint', { defaultValue: '打开设计来源选择弹窗；在弹窗内确认前不会流转。' })}
                  </p>
                  <div className="mt-3 flex flex-wrap items-center gap-2">
                    <button
                      type="button"
                      onClick={() => {
                        setDesignGateStage('choose')
                        setDesignGateOpen(true)
                      }}
                      className="rounded-lg border border-sky-600 bg-white px-3 py-2 text-sm font-medium text-sky-700 hover:bg-sky-50 dark:border-sky-500 dark:bg-zinc-900 dark:text-sky-400 dark:hover:bg-zinc-800"
                    >
                      {t('designGate.open', { defaultValue: '选择设计方案' })}
                    </button>
                    {designSession && (
                      <button
                        type="button"
                        onClick={() => {
                          setDesignGateStage('review')
                          setDesignGateOpen(true)
                        }}
                        className="inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white hover:bg-sky-700"
                      >
                        <ExternalLink className="size-3.5" />
                        {t('designGate.openCanvas', { defaultValue: '打开设计画布' })}
                      </button>
                    )}
                  </div>
                </section>
              )}
            </>
          )}

          {designGateOpen && activeStep && displayTask && (
            <DesignGateFlow
              project={displayTask.project}
              taskID={displayTask.id}
              taskTitle={displayTask.title}
              busy={Boolean(reviewBusy)}
              initialStage={designGateStage}
              submitReview={async (outputs, decision) => {
                await submitWorkflowReviewWithOutputs(outputs, decision)
                setDesignGateOpen(false)
              }}
              onClose={() => setDesignGateOpen(false)}
            />
          )}

          {isCurrentAgentStep && (
            <section ref={liveSectionRef} className="border-t border-neutral-200/80 px-4 py-4 dark:border-zinc-800">
              <div className="mb-3 flex items-center justify-between gap-2">
                <div>
                  <p className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('tasks.followLiveOutput')}</p>
                  <h3 className="mt-1 text-sm font-semibold text-neutral-900 dark:text-zinc-100">{shouldPollActiveRun && startAgent ? `${startAgent} · ${t('tasks.status.in_progress')}` : activeRun ? `${activeRun.agent} · ${activeRun.status}` : t('tasks.noActiveRun')}</h3>
                </div>
                {activeRun?.sessionId && <span className="font-mono text-[11px] text-neutral-400 dark:text-zinc-500">{activeRun.sessionId.slice(0, 8)}…</span>}
              </div>
              <div ref={liveOutputRef} onScroll={(e) => { outputStickRef.current = isAtBottom(e.currentTarget) }} className="max-h-[calc(100dvh-25rem)] min-h-72 overflow-y-auto overscroll-contain pr-1 transition-opacity duration-300">
                {remoteLogContent ? (
                  <div className="pb-16">
                    <ConversationLog
                      content={remoteLogContent}
                      mode="chat"
                      assistant={activeAgentParticipant}
                      animateLatest={isRunningRunStatus(activeRun?.status)}
                      toolDisplay="compact"
                    />
                  </div>
                ) : shouldPollLocalLiveLog && liveLogState.status === 'ok' ? (
                  <div className="pb-16">
                    {liveLogContent ? (
                      <ConversationLog
                        content={liveLogContent}
                        mode="chat"
                        assistant={activeAgentParticipant}
                        animateLatest
                        toolDisplay="compact"
                        emptyFallback={null}
                      />
                    ) : null}
                    {showLiveTail && <LiveOutputTail participant={activeAgentParticipant} />}
                  </div>
                ) : shouldPollActiveRun && activeRun?.runtimeRunId ? (
                  <LiveOutputTail participant={activeAgentParticipant} />
                ) : shouldPollLocalLiveLog && liveLogState.status === 'loading' ? (
                  <CenteredLoading label={t('tasks.followLoadingOutput')} compact />
                ) : activeRun?.logPath && logState.status === 'ok' ? (
                  <div className="pb-16">
                    <ConversationLog content={logState.data.content} mode="chat" assistant={activeAgentParticipant} toolDisplay="compact" />
                  </div>
                ) : activeRun?.logPath && logState.status === 'loading' ? (
                  <CenteredLoading label={t('tasks.followLoadingOutput')} compact />
                ) : (
                  <p className="py-8 text-center text-sm text-neutral-400 dark:text-zinc-500">{t('tasks.followNoOutput')}</p>
                )}
              </div>
            </section>
          )}
        </aside>
      </main>
    </div>
  )
}

function CenteredLoading({ label, compact = false }: { label: string; compact?: boolean }) {
  return (
    <div className={cn('flex items-center justify-center gap-2 text-sm text-neutral-500 dark:text-zinc-500', compact ? 'py-10' : 'h-full')}>
      <div className="size-4 animate-spin rounded-full border-2 border-neutral-300 border-t-sky-600 dark:border-zinc-600 dark:border-t-sky-400" />
      <span>{label}</span>
    </div>
  )
}

function LiveOutputTail({ participant }: { participant: { name: string; avatar?: string } }) {
  const initial = [...(participant.name || 'A').trim()][0]?.toUpperCase() || 'A'
  return (
    <div className="mt-4 flex items-start gap-2.5 pl-0.5" aria-label="running">
      <div className="flex size-6 shrink-0 items-center justify-center overflow-hidden rounded-full bg-neutral-100 text-[11px] font-semibold text-sky-700 dark:bg-zinc-800 dark:text-sky-400">
        {participant.avatar ? <img src={participant.avatar} alt="" className="size-full object-cover" /> : initial}
      </div>
      <div className="min-w-0">
        <p className="mb-1 text-xs font-medium text-sky-700 dark:text-sky-400">{participant.name}</p>
        <div className="w-40 rounded-lg bg-neutral-50 px-3.5 py-3 dark:bg-zinc-900/70">
          <div className="space-y-2">
            <span className="block h-2 w-28 animate-pulse rounded-full bg-neutral-200/80 dark:bg-zinc-700/70" />
            <span className="block h-2 w-16 animate-pulse rounded-full bg-sky-200/80 dark:bg-sky-800/60" />
          </div>
        </div>
      </div>
    </div>
  )
}

function findActiveRun(runs: RunRow[], taskID: string, projectID: string, actorID?: string, minStartedAt?: string): RunRow | null {
  const actorAgent = agentNameFromActor(projectID, actorID)
  const preferredAgent = actorAgent
  if (!preferredAgent) return null
  const minTime = minStartedAt ? Date.parse(minStartedAt) : 0
  const candidates = runs
    .filter((run) => run.taskId === taskID)
    .filter((run) => run.agent === preferredAgent || `${run.project}/${run.agent}` === preferredAgent)
    .filter((run) => !minTime || Date.parse(run.startedAt || '') >= minTime)
    .sort((a, b) => Date.parse(b.startedAt || '') - Date.parse(a.startedAt || ''))
  if (candidates.length > 0) return candidates[0]
  return null
}

function isRunningRunStatus(status?: string): boolean {
  const normalized = String(status || '').trim().toLowerCase()
  return normalized === 'running' || normalized === 'in_progress'
}

function isRunningStepStatus(status?: string): boolean {
  const normalized = String(status || '').trim().toLowerCase()
  return normalized === 'running' || normalized === 'in_progress'
}

function isLiveLogTerminal(content: string): boolean {
  return content.includes('=== exit code:') ||
    content.includes('=== finished:') ||
    content.includes('exec complete') ||
    content.includes('"type":"chat_done"') ||
    content.includes('agent exited with error') ||
    content.includes('status : done_success') ||
    content.includes('status : done_failed') ||
    content.includes('status  : done_success') ||
    content.includes('status  : done_failed')
}

function withRunningActiveStep(
  data: TaskWorkflowData | null,
  task: TaskRow | undefined,
  projectID: string,
  optimisticStartedAt: string | null,
  allowActorMismatch = false,
): TaskWorkflowData | null {
  if (!data || !task || task.status !== 'in_progress' || !data.run.activeStepId) return data
  const current = activeWorkflowStepInstance(data)
  const currentStep = data.definition.steps.find((step) => step.id === data.run.activeStepId)
  if (current?.status === 'completed' || current?.status === 'done_success' || current?.status === 'done_failed') return data
  if (current?.actorType === 'human' || currentStep?.type === 'human_review') return data

  const taskAgent = startableAgentName(task) || task.agent
  const actorAgent = agentNameFromActor(projectID, current?.actorId || currentStep?.actorRole)
  if (!allowActorMismatch && actorAgent && taskAgent && actorAgent !== taskAgent) return data

  const startedAt = current?.startedAt || task.startedAt || optimisticStartedAt || new Date().toISOString()
  return {
    ...data,
    run: {
      ...data.run,
      status: data.run.status === 'completed' ? data.run.status : 'running',
      updatedAt: task.updatedAt || data.run.updatedAt,
    },
    steps: data.steps.map((step) => {
      if (step.stepId !== data.run.activeStepId) return step
      return {
        ...step,
        status: 'running',
        startedAt,
        updatedAt: task.updatedAt || step.updatedAt,
      }
    }),
  }
}

function withIdleAgentStepPending(
  data: TaskWorkflowData | null,
  task: TaskRow | undefined,
  projectID: string,
  activeRunRunning: boolean,
): TaskWorkflowData | null {
  if (!data || !task || task.status === 'in_progress' || activeRunRunning || !data.run.activeStepId) return data
  const current = activeWorkflowStepInstance(data)
  const currentStep = data.definition.steps.find((step) => step.id === data.run.activeStepId)
  const isAgentStep = current?.actorType === 'agent' || currentStep?.type === 'agent_task'
  if (!isAgentStep || !isRunningStepStatus(current?.status)) return data
  const taskAgent = startableAgentName(task) || task.agent
  const actorAgent = agentNameFromActor(projectID, current?.actorId || currentStep?.actorRole)
  if (actorAgent && taskAgent && actorAgent !== taskAgent) return data
  return {
    ...data,
    steps: data.steps.map((step) => {
      if (step.stepId !== data.run.activeStepId) return step
      return {
        ...step,
        status: 'pending',
        startedAt: '',
      }
    }),
  }
}

function agentNameFromActor(projectID: string, actorID?: string): string | null {
	const raw = String(actorID || '').trim()
	if (!raw) return null
	const prefix = `${projectID}/`
	if (raw.startsWith(prefix)) return raw.slice(prefix.length) || null
	if (raw.includes('/')) return null
	return raw
}

function workflowActorAgentForStep(bindings?: Record<string, { type?: string; id?: string }>, step?: { id?: string; actorRole?: string }): string | null {
  if (!bindings || !step) return null
  for (const key of [step.id, step.actorRole]) {
    const binding = key ? bindings[key] : undefined
    if (binding?.type === 'agent' && binding.id?.trim()) return binding.id.trim()
  }
  return null
}

function normalizeReviewDecision(decision: string) {
  switch (decision.trim()) {
    case 'approved':
      return 'approve'
    case 'needs_changes':
      return 'request_changes'
    default:
      return decision.trim()
  }
}
