import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type MouseEvent, type PointerEvent, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import ReactMarkdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { ClipboardCopy, ExternalLink, GitPullRequest, Globe, Info, MessageSquare, Pencil, Play, RotateCw, Send, Trash2, X } from 'lucide-react'
import { cn } from '../../lib/cn'
import { apiDelete, apiFetch, apiPost, apiPut } from '../../lib/api'
import { copyTextToClipboard } from '../../lib/clipboard'
import { useFormatDateTime } from '../../lib/format-datetime'
import { useApiJson } from '../../lib/use-api'
import { useAuth } from '../../lib/auth'
import { formatGoDuration, taskElapsedLabel } from '../../lib/task-duration'
import { showToast } from '../ui/Toast'
import { WorkflowBoard, type WorkflowBranchInstance, type WorkflowDefinition, type WorkflowField, type WorkflowRun, type WorkflowStep, type WorkflowStepEvent, type WorkflowStepInstance } from '../workflow/WorkflowBoard'
import { overlayDismissProps } from '../ui/overlay'
import { DesignGateFlow } from '../design/DesignGateFlow'
import { isImeComposing } from '../../utils/ime'

export type TaskRow = {
  id: string
  project: string
  agent: string
  title: string
  type?: string
  priority: number
  status: string
  statusGroup: string
  archived: boolean
  assignee?: string
  assigneeLabel?: string
  description?: string
  prompt?: string
  summary?: string
  labels: string[]
  parentId?: string
  position: number
  createdBy?: string
  createdByLabel?: string
  createdAt: string
  updatedAt: string
  startedAt?: string
  finishedAt?: string
  dueDate?: string
  estimateDuration?: string
  hasWorkflow?: boolean
  hasWorktree?: boolean
  hasPreview?: boolean
  baseBranch?: string
  baseCommit?: string
  branchName?: string
  worktreeDir?: string
}

export type TaskOption = { id: string; title: string; project?: string }

export type RunRow = {
  project: string; agent: string; kind: string; status: string
  startedAt: string; finishedAt: string; model?: string
  taskId?: string; taskTitle?: string; logPath?: string
  runtimeRunId?: string; logText?: string
  inputTokens?: number; outputTokens?: number; cacheReadTokens?: number
  errorMsg?: string; command?: string
  sessionId?: string
}

export type TaskWorkflowData = { definition: WorkflowDefinition; run: WorkflowRun; steps: WorkflowStepInstance[]; branches?: WorkflowBranchInstance[]; history?: WorkflowStepEvent[]; docTitles?: Record<string, string> }
type SafeUser = { username: string; displayName?: string; email?: string }
type ProjectMember = { name: string; model?: string; avatar?: string }
type DocPreview = { id: string; title?: string; content?: string; updatedAt?: string }
type DocPreviewMeta = { docId?: string; title?: string; author?: string; createdAt?: string; tags?: string[] }
export type WorkflowRecord = WorkflowStepEvent | WorkflowStepInstance
const silentNotFound = [404]
const WorkflowDocTitleContext = createContext<Map<string, string>>(new Map())

// Restore real line breaks for descriptions whose upstream author stored literal
// "\n" / "\r\n" / "\t" sequences (typically agent-generated text that survived a
// JSON round-trip). Without this, ReactMarkdown renders the backslash sequences
// verbatim instead of as paragraph / line breaks.
function unescapeBreaks(s: string): string {
  return s.replace(/\\r\\n/g, '\n').replace(/\\n/g, '\n').replace(/\\t/g, '\t')
}

export function isOptionalTerminalReviewDecision(step?: WorkflowStep, definition?: WorkflowDefinition) {
  if (!step || step.type !== 'human_review') return false
  const outgoing = (definition?.edges ?? []).filter((edge) => edge.from === step.id)
  return outgoing.length <= 1
}

export const STATUS_KEYS = ['pending', 'in_progress', 'awaiting_confirmation', 'blocked', 'done_success', 'done_failed', 'cancelled'] as const
export type TaskStatus = typeof STATUS_KEYS[number]

export const statusColor: Record<string, string> = {
  pending: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  in_progress: 'bg-sky-100 text-sky-800 dark:bg-sky-900/40 dark:text-sky-300',
  awaiting_confirmation: 'bg-violet-100 text-violet-800 dark:bg-violet-900/40 dark:text-violet-300',
  blocked: 'bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-300',
  done_success: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  done_failed: 'bg-red-100 text-red-800 dark:bg-red-900/40 dark:text-red-300',
  cancelled: 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-500',
}

export const priorityLabel: Record<number, { text: string; cls: string }> = {
  0: { text: 'P0', cls: 'text-red-600 dark:text-red-400' },
  1: { text: 'P1', cls: 'text-amber-600 dark:text-amber-400' },
  2: { text: 'P2', cls: 'text-sky-600 dark:text-sky-400' },
  3: { text: 'P3', cls: 'text-neutral-400 dark:text-zinc-500' },
}

export function isTerminal(s: string) {
  return s === 'done_success' || s === 'done_failed' || s === 'cancelled'
}

export function isWorkflowStepOpen(status?: string) {
  const normalized = String(status || '').trim()
  return normalized === '' || normalized === 'pending' || normalized === 'running' || normalized === 'in_progress'
}

export function taskIdentityLabel(value?: string, label?: string) {
  return label || value || '—'
}

const fieldCls =
  'w-full rounded-lg border border-neutral-300 bg-white px-3 py-1.5 text-sm text-neutral-900 outline-none transition-colors focus:border-sky-400 dark:border-zinc-600 dark:bg-zinc-800 dark:text-zinc-100'

/* ── Edit modal ─── */

export function EditTaskModal({ task, taskOptions = [], onClose, onSaved }: { task: TaskRow; taskOptions?: TaskOption[]; onClose: () => void; onSaved: () => void }) {
  const { t } = useTranslation()
  const [title, setTitle] = useState(task.title)
  const [description, setDescription] = useState(task.description ?? '')
  const [status, setStatus] = useState(task.status)
  const [priority, setPriority] = useState(task.priority)
  const [taskType, setTaskType] = useState(task.type ?? '')
  const [summary, setSummary] = useState(task.summary ?? '')
  const [labelsStr, setLabelsStr] = useState((task.labels ?? []).join(', '))
  const [dueDate, setDueDate] = useState(task.dueDate ?? '')
  const [parentId, setParentId] = useState(task.parentId ?? '')
  const [estimateDuration, setEstimateDuration] = useState(task.estimateDuration ?? '')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const parentChoices = taskOptions.filter((o) => o.id !== task.id)

  const showSummary = isTerminal(status)
  const changed = title !== task.title || description !== (task.description ?? '') ||
    status !== task.status || priority !== task.priority || taskType !== (task.type ?? '') ||
    summary !== (task.summary ?? '') || labelsStr !== (task.labels ?? []).join(', ') ||
    dueDate !== (task.dueDate ?? '') || parentId !== (task.parentId ?? '') ||
    estimateDuration !== (task.estimateDuration ?? '')

  async function onSave() {
    setErr(null)
    setBusy(true)
    try {
      const body: Record<string, unknown> = { project: task.project, agent: task.agent, id: task.id }
      if (title !== task.title) body.title = title
      if (description !== (task.description ?? '')) body.description = description
      if (status !== task.status) body.status = status
      if (priority !== task.priority) body.priority = priority
      if (taskType !== (task.type ?? '')) body.type = taskType
      if (summary !== (task.summary ?? '')) body.summary = summary
      if (labelsStr !== (task.labels ?? []).join(', ')) {
        body.labels = labelsStr.split(',').map(l => l.trim()).filter(Boolean)
      }
      if (dueDate !== (task.dueDate ?? '')) body.dueDate = dueDate || ''
      if (parentId !== (task.parentId ?? '')) body.parentId = parentId || ''
      if (estimateDuration !== (task.estimateDuration ?? '')) body.estimateDuration = estimateDuration || ''
      await apiPut('/api/v1/tasks/update', body)
      onSaved()
      onClose()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 p-4" {...overlayDismissProps(() => !busy && onClose())}>
      <div className="max-h-[85vh] w-full max-w-lg overflow-y-auto rounded-xl border border-neutral-200 bg-white shadow-lg dark:border-zinc-700 dark:bg-zinc-900 animate-scale-in" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b border-neutral-200 px-5 py-3 dark:border-zinc-700">
          <h2 className="text-base font-semibold text-neutral-900 dark:text-zinc-100">{t('tasks.edit')}</h2>
          <button type="button" onClick={onClose} className="rounded-md p-1 text-neutral-400 hover:bg-neutral-100 dark:text-zinc-500 dark:hover:bg-zinc-800"><X className="size-4" /></button>
        </div>
        <div className="space-y-3 px-5 py-4">
          <div className="font-mono text-xs text-neutral-400 dark:text-zinc-500">{task.id}</div>
          <label className="block text-sm">
            <span className="text-neutral-600 dark:text-zinc-400">{t('forms.title')}</span>
            <input value={title} onChange={(e) => setTitle(e.target.value)} className={cn(fieldCls, 'mt-1')} />
          </label>
          <label className="block text-sm">
            <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.description')}</span>
            <textarea value={description} onChange={(e) => setDescription(e.target.value)} rows={3} className={cn(fieldCls, 'mt-1 resize-y')} />
          </label>
          <div className="grid grid-cols-2 gap-3">
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.filterStatus')}</span>
              <select value={status} onChange={(e) => setStatus(e.target.value)} className={cn(fieldCls, 'mt-1')}>
                {STATUS_KEYS.map((s) => <option key={s} value={s}>{t(`tasks.status.${s}`)}</option>)}
              </select>
            </label>
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('forms.priority')}</span>
              <select value={priority} onChange={(e) => setPriority(Number(e.target.value))} className={cn(fieldCls, 'mt-1')}>
                {[0, 1, 2, 3].map((p) => <option key={p} value={p}>P{p} — {t(`forms.priorityLabel.${p}`)}</option>)}
              </select>
            </label>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('forms.type')}</span>
              <select value={taskType} onChange={(e) => setTaskType(e.target.value)} className={cn(fieldCls, 'mt-1')}>
                {['chore', 'feature', 'bug', 'review', 'triage', 'test', 'research'].map((ty) => <option key={ty} value={ty}>{t(`forms.taskType.${ty}`, { defaultValue: ty })}</option>)}
              </select>
            </label>
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.dueDate')}</span>
              <input type="date" value={dueDate} onChange={(e) => setDueDate(e.target.value)} className={cn(fieldCls, 'mt-1')} />
            </label>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.estimateDuration')}</span>
              <input value={estimateDuration} onChange={(e) => setEstimateDuration(e.target.value)} placeholder="30m" className={cn(fieldCls, 'mt-1')} />
              <p className="mt-0.5 text-xs text-neutral-400 dark:text-zinc-500">{t('tasks.estimateDurationHint')}</p>
            </label>
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.parentTask')}</span>
              {parentChoices.length > 0 ? (
                <select value={parentId} onChange={(e) => setParentId(e.target.value)} className={cn(fieldCls, 'mt-1')}>
                  <option value="">{t('tasks.parentTaskNone')}</option>
                  {parentChoices.map((o) => (
                    <option key={o.id} value={o.id}>{o.title} ({o.id})</option>
                  ))}
                </select>
              ) : (
                <input value={parentId} onChange={(e) => setParentId(e.target.value)} placeholder="t-..." className={cn(fieldCls, 'mt-1 font-mono text-xs')} />
              )}
            </label>
          </div>
          <label className="block text-sm">
            <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.labels')}</span>
            <input value={labelsStr} onChange={(e) => setLabelsStr(e.target.value)} placeholder={t('tasks.labelsHint')} className={cn(fieldCls, 'mt-1')} />
          </label>
          {showSummary && (
            <label className="block text-sm">
              <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.summary')}</span>
              <textarea value={summary} onChange={(e) => setSummary(e.target.value)} rows={4} placeholder={t('tasks.summaryPlaceholder')} className={cn(fieldCls, 'mt-1')} />
              {task.createdBy && (
                <p className="mt-1 text-xs text-neutral-400 dark:text-zinc-500">
                  {t('tasks.willNotifyCreator', { creator: taskIdentityLabel(task.createdBy, task.createdByLabel) })}
                </p>
              )}
            </label>
          )}
          {err && <p className="text-sm text-red-600 dark:text-red-400">{err}</p>}
          <div className="flex justify-end gap-2 pt-1">
            <button type="button" onClick={onClose} disabled={busy} className="rounded-lg border border-neutral-300 px-3 py-1.5 text-sm dark:border-zinc-600">{t('forms.cancel')}</button>
            <button type="button" onClick={() => void onSave()} disabled={busy || !changed} className="rounded-lg bg-sky-600 px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50">{busy ? t('forms.saving') : t('forms.save')}</button>
          </div>
        </div>
      </div>
    </div>
  )
}

/* ── Detail modal ─── */

export function TaskDetailModal({ task, onClose, onEdit, onMutated, canEdit = true }: { task: TaskRow; onClose: () => void; onEdit: (r: TaskRow) => void; onMutated?: () => void; canEdit?: boolean }) {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()

  const prio = priorityLabel[task.priority] ?? priorityLabel[2]
  const sCls = statusColor[task.status] ?? statusColor.pending
  const [workflowVersion, setWorkflowVersion] = useState(0)
  const [reviewComments, setReviewComments] = useState('')
  const [reviewOutputs, setReviewOutputs] = useState<Record<string, string>>({})
  const [reviewBusy, setReviewBusy] = useState<string | null>(null)
  const [reviewErr, setReviewErr] = useState<string | null>(null)
  const [missingReviewField, setMissingReviewField] = useState<string | null>(null)
  const [assigneeEditing, setAssigneeEditing] = useState(false)
  const [assigneeDraft, setAssigneeDraft] = useState(task.assignee || `${task.project}/${task.agent}`)
  const [assigneeBusy, setAssigneeBusy] = useState(false)
  const [assigneeErr, setAssigneeErr] = useState<string | null>(null)
  const [startBusy, setStartBusy] = useState(false)
  const [previewStarting, setPreviewStarting] = useState(false)

  const runsQuery = `/api/v1/telemetry/runs?allTime=1&project=${encodeURIComponent(task.project)}`
  const runsState = useApiJson<{ runs: RunRow[] }>(runsQuery, 0)
  const matchingRun = useMemo(() => {
    if (runsState.status !== 'ok' || !runsState.data?.runs) return null
    return runsState.data.runs.find((r) => r.taskId === task.id) ?? null
  }, [runsState, task.id])

  const workflowState = useApiJson<TaskWorkflowData>(
    `/api/v1/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/workflow`,
    workflowVersion,
    { silentStatuses: silentNotFound },
  )
  const previewState = useApiJson<{ taskId: string; type: string; status: string; url: string; previewToken?: string }>(
    task?.project && task?.id ? `/api/v1/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/preview` : null,
    workflowVersion,
    { silentStatuses: silentNotFound },
  )
  const preview = previewState.status === 'ok' ? previewState.data : null
  const usersState = useApiJson<SafeUser[]>('/api/v1/users', 0)
  const membersState = useApiJson<ProjectMember[]>(`/api/v1/projects/${encodeURIComponent(task.project)}/agents`, 0)
  const actorLabels = useMemo(() => {
    const labels = new Map<string, string>()
    if (usersState.status === 'ok') {
      for (const user of usersState.data ?? []) {
        labels.set(user.username, user.displayName || user.email || user.username)
      }
    }
    if (task.assignee && task.assigneeLabel) labels.set(task.assignee, task.assigneeLabel)
    if (task.createdBy && task.createdByLabel) labels.set(task.createdBy, task.createdByLabel)
    return labels
  }, [task.assignee, task.assigneeLabel, task.createdBy, task.createdByLabel, usersState])
  const assigneeOptions = useMemo(() => {
    const labels = new Map<string, string>()
    if (membersState.status === 'ok') {
      for (const member of membersState.data ?? []) {
        if (!member.name) continue
        if (member.model === 'human') {
          labels.set(member.name, actorLabels.get(member.name) || member.name)
        } else {
          labels.set(`${task.project}/${member.name}`, member.name)
        }
      }
    }
    const current = task.assignee || `${task.project}/${task.agent}`
    labels.set(current, taskIdentityLabel(current, task.assigneeLabel))
    return Array.from(labels.entries()).map(([value, label]) => ({ value, label }))
  }, [actorLabels, membersState, task.agent, task.assignee, task.assigneeLabel, task.project])
  const activeWorkflowInst = workflowState.status === 'ok'
    ? activeWorkflowStepInstance(workflowState.data)
    : undefined
  const activeWorkflowStep = workflowState.status === 'ok'
    ? workflowState.data.definition.steps.find((step) => step.id === (activeWorkflowInst?.stepId || workflowState.data.run.activeStepId))
    : undefined
  const workflowRecords = workflowState.status === 'ok'
    ? workflowHistoryRecords(workflowState.data)
    : []
  const canReviewWorkflow = Boolean(activeWorkflowStep?.type === 'human_review' && isWorkflowStepOpen(activeWorkflowInst?.status) && !isTerminal(task.status))
  const isDesignGate = Boolean(canReviewWorkflow && activeWorkflowStep?.config?.designGate === 'true')
  const [designGateOpen, setDesignGateOpen] = useState(false)
  const [designGateStage, setDesignGateStage] = useState<'choose' | 'review'>('choose')
  const [designSession, setDesignSession] = useState<{ projectId: string } | null>(null)
  useEffect(() => {
    if (!isDesignGate) {
      setDesignSession(null)
      return
    }
    // Mirror the follow page: probe for an existing design session so the
    // modal can offer a one-click "open canvas" shortcut next to the chooser.
    let alive = true
    apiFetch<{ projectId?: string }>(
      `/api/v1/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/design/status`,
      { silentStatuses: [401, 403, 404, 500, 502, 503, 504] },
    )
      .then((d) => {
        if (alive) setDesignSession(d?.projectId ? { projectId: d.projectId } : null)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [isDesignGate, task.project, task.id, designGateOpen])
  const startAgentName = startableAgentName(task)
  const isFailedOrCancelled = task.status === 'done_failed' || task.status === 'cancelled'
  const canStartAgent = Boolean(startAgentName && (task.status === 'pending' || isFailedOrCancelled))

  useEffect(() => {
    setReviewComments('')
    setReviewOutputs({})
    setReviewErr(null)
    setMissingReviewField(null)
  }, [activeWorkflowStep?.id])

  useEffect(() => {
    setAssigneeDraft(task.assignee || `${task.project}/${task.agent}`)
    setAssigneeEditing(false)
    setAssigneeErr(null)
  }, [task.agent, task.assignee, task.project])

  async function saveAssignee(nextValue: string) {
    const previous = task.assignee || `${task.project}/${task.agent}`
    const next = nextValue.trim()
    if (!next || next === (task.assignee || `${task.project}/${task.agent}`)) {
      setAssigneeDraft(previous)
      return
    }
    setAssigneeDraft(next)
    setAssigneeBusy(true)
    setAssigneeErr(null)
    try {
      await apiPut('/api/v1/tasks/update', {
        project: task.project,
        agent: task.agent,
        id: task.id,
        assignee: next,
      })
      setAssigneeEditing(false)
      onMutated?.()
    } catch (e) {
      setAssigneeDraft(previous)
      setAssigneeErr(e instanceof Error ? e.message : String(e))
    } finally {
      setAssigneeBusy(false)
    }
  }

  async function submitWorkflowReview(decision?: string) {
    await submitWorkflowReviewWithOutputs(reviewOutputs, decision)
  }

  // Review submit that merges caller-supplied outputs (the design gate flows
  // in approved_design_* fields) on top of the form state.
  async function submitWorkflowReviewWithOutputs(extraOutputs: Record<string, string>, decision?: string) {
    setReviewErr(null)
    setMissingReviewField(null)
    const merged = { ...reviewOutputs, ...Object.fromEntries(Object.entries(extraOutputs).filter(([, v]) => String(v ?? '').trim() !== '')) }
    const normalizedDecision = normalizeReviewDecision(decision || merged.decision || '')
    setReviewBusy(normalizedDecision || 'submit')
    const outputs: Record<string, string> = Object.fromEntries(Object.entries(merged).map(([key, value]) => [key, String(value ?? '').trim()]))
    if (normalizedDecision) outputs.decision = normalizedDecision
    const outputFieldNames = (activeWorkflowStep?.outputFields ?? []).map((field) => field.name).filter(Boolean)
    const comments = (outputs.comments ?? reviewComments).trim()
    if (outputFieldNames.includes('comments')) outputs.comments = comments
    const decisionOptional = isOptionalTerminalReviewDecision(activeWorkflowStep, workflowState.status === 'ok' ? workflowState.data.definition : undefined)
    // Optional fields never block submit: left empty, the server backfills
    // them from the deterministic draft rules.
    const missingField = (activeWorkflowStep?.outputFields ?? [])
      .filter((field) => field.name && !field.optional)
      .map((field) => field.name)
      .find((name) => !(name === 'decision' && decisionOptional) && !String(outputs[name] ?? '').trim())
    if (missingField) {
      setMissingReviewField(missingField)
      setReviewErr(`${t('forms.fillRequired')} ${missingField}`)
      setReviewBusy(null)
      return
    }
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/workflow/review`, {
        decision: normalizedDecision,
        comments,
        outputs,
      })
      setReviewComments('')
      setReviewOutputs({})
      setWorkflowVersion((v) => v + 1)
      onMutated?.()
    } catch (e) {
      setReviewErr(e instanceof Error ? e.message : String(e))
      throw e
    } finally {
      setReviewBusy(null)
    }
  }

  async function startCurrentAssignee() {
    if (!startAgentName || (isTerminal(task.status) && !isFailedOrCancelled)) return
    setStartBusy(true)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/start`, {}, { suppressToast: true })
      onMutated?.()
      window.setTimeout(() => onMutated?.(), 800)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      if (msg.includes('already running') || msg.includes('scheduler_wakeup_failed') || msg.includes('conflict') || msg.includes('忙碌') || msg.includes('正在')) {
        showToast(t('tasks.agentAlreadyRunning', { defaultValue: '智能体已在后台执行该任务中，正在处理…' }), 'info')
      } else {
        showToast(msg || t('apiErrors.bad_request'), 'error')
      }
    } finally {
      setStartBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center pt-[3vh]">
      <div className="absolute inset-0 bg-black/30 backdrop-blur-[2px] animate-fade-in dark:bg-black/50" onClick={onClose} />
      <div className="relative w-full max-w-4xl max-h-[94vh] flex flex-col overflow-hidden rounded-xl border border-neutral-200/80 bg-white shadow-2xl animate-scale-in dark:border-zinc-700/80 dark:bg-zinc-900">
        <div className="flex items-center justify-between border-b border-neutral-200/80 px-5 py-3 dark:border-zinc-700/60">
          <div className="flex items-center gap-3 min-w-0">
            <span className={cn('shrink-0 rounded-full px-2.5 py-0.5 text-[11px] font-semibold', sCls)}>{t(`tasks.status.${task.status}`, { defaultValue: task.status })}</span>
            <span className={cn('shrink-0 text-[11px] font-bold', prio.cls)}>{prio.text}</span>
            <span className="truncate text-sm font-medium text-neutral-900 dark:text-zinc-100">{task.title}</span>
          </div>
          <div className="flex items-center gap-1.5 shrink-0">
            {(() => {
              const taskBranch = task.branchName || (task.worktreeDir ? `feature/${task.id}` : null)
              if (!taskBranch) return null
              return (
                <div className="hidden sm:inline-flex items-center gap-1 font-mono text-[11px] text-sky-800 bg-sky-50 dark:bg-sky-950/60 dark:text-sky-300 px-2 py-0.5 rounded-md border border-sky-200/80 dark:border-sky-800 shadow-xs">
                  <span className="font-semibold text-sky-700 dark:text-sky-300">
                    {task.baseBranch || 'main'}
                    {task.baseCommit ? (
                      <span className="text-[10px] font-normal text-sky-600/80 dark:text-sky-400/80">({task.baseCommit.slice(0, 7)})</span>
                    ) : null}
                  </span>
                  <span className="text-sky-400 dark:text-sky-600 font-bold select-none">──►</span>
                  <span className="truncate max-w-[150px]" title={taskBranch}>
                    {taskBranch}
                  </span>
                </div>
              )
            })()}
            {preview && preview.type !== 'cli' && (
              <div className="flex items-center">
                {preview.status === 'running' ? (
                  <a
                    href={preview.previewToken ? `${preview.url}?pvt=${encodeURIComponent(preview.previewToken)}` : preview.url}
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex items-center gap-1 rounded-md border border-emerald-500/30 bg-emerald-50 px-2 py-1 text-xs font-semibold text-emerald-700 shadow-sm transition hover:bg-emerald-100 dark:border-emerald-500/30 dark:bg-emerald-950/40 dark:text-emerald-300"
                  >
                    <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" />
                    {t('tasks.openPreview', { defaultValue: '打开实时预览 ↗' })}
                  </a>
                ) : (
                  <button
                    type="button"
                    onClick={async () => {
                      setPreviewStarting(true)
                      try {
                        const inst = await apiPost<{ previewToken?: string }>(`/api/v1/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/preview/start`, {})
                        const tokenQS = inst?.previewToken ? `?pvt=${encodeURIComponent(inst.previewToken)}` : ''
                        window.open(`/preview/${encodeURIComponent(task.id)}/${tokenQS}`, '_blank')
                        setWorkflowVersion((v) => v + 1)
                      } finally {
                        setPreviewStarting(false)
                      }
                    }}
                    disabled={previewStarting}
                    className="inline-flex items-center gap-1 rounded-md border border-sky-600/30 bg-sky-50 px-2 py-1 text-xs font-semibold text-sky-700 transition hover:bg-sky-100 dark:border-sky-500/30 dark:bg-sky-950/40 dark:text-sky-300"
                  >
                    <Globe className={cn('size-3.5', previewStarting && 'animate-spin')} />
                    {previewStarting ? t('tasks.startingPreview', { defaultValue: '启动中…' }) : t('tasks.startPreview', { defaultValue: '启动实时预览' })}
                  </button>
                )}
              </div>
            )}
            {task.hasWorkflow && (
              <Link
                to={`/projects/${encodeURIComponent(task.project)}/tasks/${encodeURIComponent(task.id)}/follow`}
                className="rounded-md px-2 py-1 text-xs font-medium text-sky-700 transition-colors hover:bg-sky-50 dark:text-sky-400 dark:hover:bg-zinc-800"
                title={t('tasks.follow')}
              >
                {t('tasks.follow')}
              </Link>
            )}
            {canEdit && (
              <button
                type="button"
                onClick={() => void startCurrentAssignee()}
                disabled={!canStartAgent || startBusy}
                title={!startAgentName ? t('tasks.startRequiresAgent') : isFailedOrCancelled ? t('tasks.retryTask', { defaultValue: '重新执行任务' }) : t('tasks.start')}
                className={cn(
                  'rounded-md p-1 transition-colors disabled:cursor-not-allowed disabled:opacity-35',
                  isFailedOrCancelled
                    ? 'text-amber-600 hover:bg-amber-50 dark:text-amber-400 dark:hover:bg-amber-950/30'
                    : 'text-neutral-400 enabled:hover:bg-sky-50 enabled:hover:text-sky-700 dark:text-zinc-500 dark:enabled:hover:bg-zinc-800 dark:enabled:hover:text-sky-400'
                )}
              >
                {isFailedOrCancelled ? (
                  <RotateCw className={cn('size-4', startBusy && 'animate-spin')} strokeWidth={1.8} />
                ) : (
                  <Play className={cn('size-4', startBusy && 'animate-pulse')} strokeWidth={1.8} />
                )}
              </button>
            )}
            {canEdit && (
              <button type="button" onClick={() => onEdit(task)} className="rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-500 dark:hover:bg-zinc-800" title={t('tasks.edit')}>
                <Pencil className="size-4" strokeWidth={1.8} />
              </button>
            )}
            <button type="button" onClick={onClose} className="rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-500 dark:hover:bg-zinc-800">
              <X className="size-4" strokeWidth={2} />
            </button>
          </div>
        </div>

        <div className="shrink-0 grid grid-cols-2 gap-x-6 gap-y-2.5 border-b border-neutral-100 px-5 py-3 text-sm dark:border-zinc-700/40 sm:grid-cols-3">
          <InfoCell label="ID"><span className="font-mono text-xs">{task.id}</span></InfoCell>
          <InfoCell label={t('tasks.colProject')}><span className="font-mono">{task.project}</span></InfoCell>
          <InfoCell label={t('tasks.colAssignee')}>
            <div className="space-y-1" title={task.assignee || `${task.project}/${task.agent}`}>
              {canEdit && !isTerminal(task.status) && assigneeEditing ? (
                <select
                  value={assigneeDraft}
                  onChange={(event) => void saveAssignee(event.target.value)}
                  onBlur={() => {
                    setAssigneeDraft(task.assignee || `${task.project}/${task.agent}`)
                    setAssigneeEditing(false)
                  }}
                  onKeyDown={(event) => {
                    if (event.key === 'Escape') {
                      setAssigneeDraft(task.assignee || `${task.project}/${task.agent}`)
                      setAssigneeEditing(false)
                    }
                  }}
                  autoFocus
                  disabled={assigneeBusy || membersState.status === 'loading'}
                  className="max-w-full rounded-lg border border-neutral-300 bg-white px-2 py-1 text-sm text-neutral-900 outline-none focus:border-sky-400 disabled:opacity-60 dark:border-zinc-600 dark:bg-zinc-800 dark:text-zinc-100"
                >
                  {assigneeOptions.map((option) => (
                    <option key={option.value} value={option.value}>{option.label}</option>
                  ))}
                </select>
              ) : (
                <button
                  type="button"
                  disabled={!canEdit || isTerminal(task.status)}
                  onClick={() => {
                    if (!canEdit || isTerminal(task.status)) return
                    setAssigneeDraft(task.assignee || `${task.project}/${task.agent}`)
                    setAssigneeEditing(true)
                  }}
                  className="-ml-1 block max-w-full truncate rounded-md px-1 py-0.5 text-left text-neutral-800 transition-colors enabled:hover:bg-neutral-100 enabled:hover:text-sky-700 disabled:cursor-default dark:text-zinc-200 dark:enabled:hover:bg-zinc-800 dark:enabled:hover:text-sky-400"
                >
                  {compactTaskIdentityLabel(task.assignee || `${task.project}/${task.agent}`, task.assigneeLabel)}
                </button>
              )}
              {membersState.status === 'loading' && <p className="text-xs text-neutral-400 dark:text-zinc-500">{t('api.loading')}</p>}
              {assigneeBusy && <p className="text-xs text-neutral-400 dark:text-zinc-500">{t('forms.saving')}</p>}
              {assigneeErr && <p className="text-xs text-red-600 dark:text-red-400">{assigneeErr}</p>}
            </div>
          </InfoCell>
          <InfoCell label={t('forms.type')}>{task.type ? t(`forms.taskType.${task.type}`, { defaultValue: task.type }) : '—'}</InfoCell>
          <InfoCell label={t('api.taskColUpdated')}>{fmt(task.updatedAt)}</InfoCell>
          {(task.estimateDuration || taskElapsedLabel(task)) && (
            <InfoCell label={`${t('tasks.estimateDuration')} / ${t('tasks.elapsed')}`}>
              <span className="tabular-nums">
                {task.estimateDuration ? formatGoDuration(task.estimateDuration) : '—'} / {taskElapsedLabel(task) ?? '—'}
              </span>
            </InfoCell>
          )}
          {task.startedAt && <InfoCell label={t('tasks.startedAt')}>{fmt(task.startedAt)}</InfoCell>}
          {task.finishedAt && <InfoCell label={t('tasks.finishedAt')}>{fmt(task.finishedAt)}</InfoCell>}
          {task.dueDate && <InfoCell label={t('tasks.dueDate')}><span className="tabular-nums">{task.dueDate}</span></InfoCell>}
          {task.createdBy && <InfoCell label={t('tasks.createdBy')}><span title={task.createdBy}>{taskIdentityLabel(task.createdBy, task.createdByLabel)}</span></InfoCell>}
          {task.parentId && <InfoCell label={t('tasks.parentTask')}><span className="font-mono text-xs">{task.parentId}</span></InfoCell>}
          {task.labels && task.labels.length > 0 && (
            <InfoCell label={t('tasks.labels')}>
              <div className="flex flex-wrap gap-1">
                {task.labels.map(l => (
                  <span key={l} className="rounded-full bg-indigo-100 px-2 py-0.5 text-[11px] font-medium text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400">{l}</span>
                ))}
              </div>
            </InfoCell>
          )}
          {matchingRun && (
            <>
              <InfoCell label={t('runs.model')}><span className="font-mono">{matchingRun.model ?? '—'}</span></InfoCell>
              <InfoCell label={t('runs.colTok')}>
                <span className="tabular-nums">{fmtNum((matchingRun.inputTokens ?? 0) + (matchingRun.outputTokens ?? 0) + (matchingRun.cacheReadTokens ?? 0))} tok</span>
              </InfoCell>
              {matchingRun.sessionId && (
                <InfoCell label={t('runs.sessionLabel')}>
                  <div className="flex items-center gap-1">
                    <span className="font-mono text-xs text-emerald-700 dark:text-emerald-400" title={matchingRun.sessionId}>{matchingRun.sessionId.slice(0, 8)}…</span>
                    <CopyRunResumeCmd model={matchingRun.model} sessionId={matchingRun.sessionId} agent={matchingRun.agent} project={matchingRun.project} />
                  </div>
                </InfoCell>
              )}
              <InfoCell label={t('tasks.executionRecord')}>
                <a
                  href={`/projects/${encodeURIComponent(matchingRun.project)}/runs?agent=${encodeURIComponent(`${matchingRun.project}/${matchingRun.agent}`)}`}
                  target="_blank"
                  rel="noreferrer"
                  className="text-sm font-medium text-sky-700 hover:text-sky-800 dark:text-sky-400 dark:hover:text-sky-300"
                  title={matchingRun.logPath || matchingRun.sessionId || undefined}
                >
                  {t('tasks.viewExecutionRecord')}
                </a>
              </InfoCell>
            </>
          )}
        </div>

        <div className="flex-1 min-h-0 overflow-y-auto">
        {workflowState.status === 'ok' && (
          <div className="border-b border-neutral-100 px-5 py-4 dark:border-zinc-700/40">
            <div className="mb-3 flex items-center justify-between gap-3">
              <div>
                <span className="text-xs font-semibold uppercase tracking-wider text-sky-500 dark:text-sky-400">{t('workflows.taskWorkflow')}</span>
                <h3 className="mt-1 text-base font-semibold text-neutral-900 dark:text-zinc-100">{workflowState.data.definition.name}</h3>
              </div>
              <span className="rounded-full bg-sky-100 px-2.5 py-1 text-xs font-medium text-sky-700 dark:bg-sky-900/50 dark:text-sky-300">
                {t(`workflows.stepStatus.${workflowState.data.run.status}`, { defaultValue: workflowState.data.run.status })}
              </span>
            </div>
            <div className="grid min-h-[520px] gap-4">
              <WorkflowBoard definition={workflowState.data.definition} run={workflowState.data.run} instances={workflowState.data.steps} branches={workflowState.data.branches} focusActive compact />
              <div className="flex min-h-0 flex-col rounded-xl border border-neutral-200 bg-white dark:border-zinc-700 dark:bg-zinc-950">
                {isDesignGate ? (
                  <div className="flex min-h-0 flex-1 flex-col p-4">
                    <p className="text-sm text-neutral-700 dark:text-zinc-300">{activeWorkflowStep?.description}</p>
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
                  </div>
                ) : (
                  <WorkflowRuntimePanel
                    step={activeWorkflowStep}
                    instance={activeWorkflowInst}
                    steps={workflowState.data.definition.steps}
                    records={workflowRecords}
                    runs={runsState.status === 'ok' ? runsState.data.runs ?? [] : []}
                    taskID={task.id}
                    project={task.project}
                    actorLabels={actorLabels}
                    canReview={canReviewWorkflow}
                    docTitles={workflowState.data.docTitles}
                    reviewOutputs={reviewOutputs}
                    reviewComments={reviewComments}
                    reviewBusy={reviewBusy}
                    reviewErr={reviewErr}
                    missingReviewField={missingReviewField}
                    onChangeOutput={(name, value) => {
                      if (missingReviewField === name && String(value ?? '').trim()) setMissingReviewField(null)
                      setReviewOutputs((current) => ({ ...current, [name]: value }))
                      if (name === 'comments') setReviewComments(value)
                    }}
                    onChangeComments={(value) => {
                      if (missingReviewField === 'comments' && value.trim()) setMissingReviewField(null)
                      setReviewComments(value)
                    }}
                    onSubmitReview={(decision) => void submitWorkflowReview(decision)}
                  />
                )}
              </div>
            </div>
          </div>
        )}
        {workflowState.status === 'error' && (workflowState.error as { status?: number }).status !== 404 && (
          <div className="border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
            <p className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
              {workflowState.error.message}
            </p>
          </div>
        )}

        {task.description && (
          <div className="border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
            <span className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('tasks.description')}</span>
            <div className="mt-1.5 text-sm text-neutral-700 dark:text-zinc-300">
              <div className="prose prose-sm max-w-none dark:prose-invert"><ReactMarkdown remarkPlugins={[remarkGfm]}>{unescapeBreaks(task.description)}</ReactMarkdown></div>
            </div>
          </div>
        )}

        {task.prompt && !isWorkflowRuntimePrompt(task.prompt) && (
          <div className="border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
            <span className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('forms.prompt')}</span>
            <div className="mt-1.5 rounded-lg bg-neutral-50 p-3 text-sm text-neutral-700 dark:bg-zinc-800/50 dark:text-zinc-300">
              <div className="prose prose-sm max-w-none dark:prose-invert"><ReactMarkdown remarkPlugins={[remarkGfm]}>{unescapeBreaks(task.prompt)}</ReactMarkdown></div>
            </div>
          </div>
        )}

        {task.summary && (
          <div className="border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
            <span className="text-xs font-semibold uppercase tracking-wider text-emerald-500 dark:text-emerald-400">{t('tasks.summary')}</span>
            <div className="mt-1.5 rounded-lg bg-emerald-50 p-3 text-sm text-neutral-700 dark:bg-emerald-900/20 dark:text-zinc-300">
              <div className="prose prose-sm max-w-none dark:prose-invert"><ReactMarkdown remarkPlugins={[remarkGfm]}>{unescapeBreaks(task.summary)}</ReactMarkdown></div>
            </div>
          </div>
        )}

        <TaskCommentsSection project={task.project} agent={task.agent} taskId={task.id} />
        </div>
      </div>
      {designGateOpen && activeWorkflowStep && (
        <DesignGateFlow
          project={task.project}
          taskID={task.id}
          taskTitle={task.title}
          busy={Boolean(reviewBusy)}
          initialStage={designGateStage}
          submitReview={async (outputs, decision) => {
            await submitWorkflowReviewWithOutputs(outputs, decision)
            setDesignGateOpen(false)
          }}
          onClose={() => setDesignGateOpen(false)}
        />
      )}
    </div>
  )
}

/* ── Helpers ── */

function InfoCell({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <span className="text-xs font-medium text-neutral-400 dark:text-zinc-500">{label}</span>
      <div className="text-neutral-800 dark:text-zinc-200">{children}</div>
    </div>
  )
}

export function workflowHistoryRecords(data: TaskWorkflowData): WorkflowRecord[] {
  const history = (data.history ?? [])
    .filter((item) => workflowRecordHasPayload(item))
    .sort((a, b) => workflowRecordTimestamp(a) - workflowRecordTimestamp(b))
  if (history.length > 0) return history
  return data.steps
    .filter((item) => item.stepId !== data.run.activeStepId && workflowRecordHasPayload(item))
    .sort((a, b) => workflowRecordTimestamp(a) - workflowRecordTimestamp(b))
}

export function activeWorkflowStepInstance(data: TaskWorkflowData): WorkflowStepInstance | undefined {
  const activeStepID = data.run.activeStepId
  if (!activeStepID) {
    return data.steps
      .filter((item) => workflowRecordHasPayload(item))
      .sort((a, b) => workflowRecordTimestamp(b) - workflowRecordTimestamp(a))[0]
  }
  const candidates = data.steps
    .filter((item) => item.stepId === activeStepID)
    .sort((a, b) => workflowRecordTimestamp(b) - workflowRecordTimestamp(a))
  return candidates.find((item) => isWorkflowStepOpen(item.status)) ?? candidates[0]
}

export function startableAgentName(task: TaskRow): string | null {
  const assignee = (task.assignee || `${task.project}/${task.agent}`).trim()
  const prefix = `${task.project}/`
  if (!assignee.startsWith(prefix)) return null
  const agent = assignee.slice(prefix.length).trim()
  return agent || null
}

function workflowRecordHasPayload(record: WorkflowRecord) {
  return Boolean(
    record.inputArtifact ||
    record.outputArtifact ||
    record.summary ||
    hasWorkflowValues(record.inputValues) ||
    hasWorkflowValues(record.outputValues),
  )
}

function workflowRecordTimestamp(record: WorkflowRecord) {
  const raw = record.finishedAt || record.startedAt || ('updatedAt' in record ? record.updatedAt : record.createdAt)
  const ts = Date.parse(raw || '')
  return Number.isFinite(ts) ? ts : 0
}

/* ── Task Comments Section ── */

type TaskCommentRow = {
  id: string
  taskId: string
  author: string
  body: string
  createdAt: string
}

function TaskCommentsSection({ project, agent, taskId }: { project: string; agent: string; taskId: string }) {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()
  const { user } = useAuth()
  const [body, setBody] = useState('')
  const [busy, setBusy] = useState(false)
  const [ver, setVer] = useState(0)

  const url = `/api/v1/tasks/${encodeURIComponent(project)}/${encodeURIComponent(agent)}/${encodeURIComponent(taskId)}/comments`
  const state = useApiJson<TaskCommentRow[]>(url, ver)
  const comments = state.status === 'ok' ? state.data ?? [] : []

  const reload = useCallback(() => setVer((v) => v + 1), [])

  async function handleAdd() {
    if (!body.trim()) return
    setBusy(true)
    try {
      await apiPost(url, { author: user?.username ?? 'human', body: body.trim() })
      setBody('')
      reload()
    } finally {
      setBusy(false)
    }
  }

  async function handleDelete(commentId: string) {
    await apiDelete(`${url}/${encodeURIComponent(commentId)}`)
    reload()
  }

  return (
    <div className="shrink-0 border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
      <div className="flex items-center gap-1.5 mb-2">
        <MessageSquare className="size-3.5 text-neutral-400 dark:text-zinc-500" strokeWidth={1.8} />
        <span className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('tasks.comments')} ({comments.length})</span>
      </div>

      {comments.length > 0 && (
        <div className="space-y-2 mb-3 max-h-56 overflow-y-auto">
          {comments.map((c) => (
            <div key={c.id} className="group rounded-lg bg-neutral-50 px-3 py-2 dark:bg-zinc-800/50">
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs text-neutral-500 dark:text-zinc-500">
                  <span className="font-semibold text-neutral-700 dark:text-zinc-300">@{c.author}</span>
                  <span>{fmt(c.createdAt)}</span>
                </div>
                {(user?.role === 'admin' || user?.username === c.author) && (
                  <button type="button" onClick={() => void handleDelete(c.id)} className="opacity-0 group-hover:opacity-100 rounded p-0.5 text-neutral-400 hover:text-red-500 transition dark:text-zinc-600 dark:hover:text-red-400" title={t('forms.delete')}>
                    <Trash2 className="size-3" />
                  </button>
                )}
              </div>
              <div className="mt-1 text-sm text-neutral-700 dark:text-zinc-300 prose prose-sm max-w-none dark:prose-invert">
                <ReactMarkdown remarkPlugins={[remarkGfm]}>{c.body}</ReactMarkdown>
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="flex gap-2">
        <input
          value={body}
          onChange={(e) => setBody(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey && !isImeComposing(e)) { e.preventDefault(); void handleAdd() } }}
          placeholder={t('tasks.commentPlaceholder')}
          className="flex-1 rounded-lg border border-neutral-300 bg-white px-3 py-1.5 text-sm outline-none transition-colors focus:border-sky-400 dark:border-zinc-600 dark:bg-zinc-800 dark:text-zinc-100"
          disabled={busy}
        />
        <button type="button" onClick={() => void handleAdd()} disabled={busy || !body.trim()} className="rounded-lg bg-sky-600 px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50">
          <Send className="size-3.5" />
        </button>
      </div>
    </div>
  )
}

export function WorkflowRuntimePanel({
  step,
  instance,
  steps,
  records,
  runs,
  taskID,
  project,
  actorLabels,
  canReview,
  hideHeader = false,
  reviewOutputs,
  reviewComments,
  reviewBusy,
  reviewErr,
  missingReviewField,
  onChangeOutput,
  onChangeComments,
  onSubmitReview,
  docTitles,
}: {
  step?: WorkflowStep
  instance?: WorkflowStepInstance
  steps: WorkflowStep[]
  records: WorkflowRecord[]
  runs: RunRow[]
  taskID: string
  project?: string
  actorLabels: Map<string, string>
  canReview: boolean
  hideHeader?: boolean
  reviewOutputs: Record<string, string>
  reviewComments: string
  reviewBusy: string | null
  reviewErr: string | null
  missingReviewField?: string | null
  onChangeOutput: (name: string, value: string) => void
  onChangeComments: (value: string) => void
  onSubmitReview: (decision?: string) => void
  docTitles?: Record<string, string>
}) {
  const { t } = useTranslation()
  const docTitleMap = useMemo(() => {
    const map = new Map<string, string>()
    for (const [id, title] of Object.entries(docTitles ?? {})) {
      if (id.trim() && title.trim()) map.set(id, title.trim())
    }
    return map
  }, [docTitles])
  const parsedOutputValues = parseWorkflowArtifact(instance?.outputArtifact || instance?.summary || '')
  const parsedInputValues = parseWorkflowArtifact(instance?.inputArtifact || '')
  const outputValues = hasWorkflowValues(instance?.outputValues) ? instance!.outputValues! : parsedOutputValues
  const inputValues = hasWorkflowValues(instance?.inputValues) ? instance!.inputValues! : parsedInputValues
  const hasStructuredInput = Object.keys(inputValues).length > 0
  const stepByIDMap = useMemo(() => new Map(steps.map((item) => [item.id, item])), [steps])
  const actorLabel = workflowActorLabel(instance?.actorType, instance?.actorId, actorLabels)
  const [draftApplied, setDraftApplied] = useState<Record<string, string>>({})
  const [draftBusy, setDraftBusy] = useState(false)
  const draftLoadedFor = useRef<string | null>(null)

  // Pull rule-computed prefill when the review form first opens for a step.
  // Only fills empty fields, tags each with its source, never touches decision.
  const loadReviewDraft = useCallback(async () => {
    if (!canReview || !project || !taskID || !step || step.type !== 'human_review' || !instance || !isWorkflowStepOpen(instance.status)) return
    const key = `${project}/${taskID}/${step.id}`
    if (draftLoadedFor.current === key) return
    draftLoadedFor.current = key
    setDraftBusy(true)
    try {
      // await before first setState keeps the effect body free of synchronous
      // state updates (cascading-render lint rule).
      const data = await apiFetch<{ fields: Array<{ name: string; value: string; source: string; sourceLabel: string; generated: boolean }> }>(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/workflow/review/draft`,
      )
      let appliedAny = false
      for (const f of data?.fields ?? []) {
        if (!f.generated || !f.value) continue
        if (f.name === 'comments') {
          if (!reviewComments.trim()) {
            onChangeComments(f.value)
            setDraftApplied((prev) => ({ ...prev, comments: f.sourceLabel }))
            appliedAny = true
          }
        } else if (!(reviewOutputs[f.name] ?? '').trim()) {
          onChangeOutput(f.name, f.value)
          setDraftApplied((prev) => ({ ...prev, [f.name]: f.sourceLabel }))
          appliedAny = true
        }
      }
      if (!appliedAny) draftLoadedFor.current = null
    } catch {
      // Draft is best-effort; a silent failure leaves the form empty.
      draftLoadedFor.current = null
    } finally {
      setDraftBusy(false)
    }
  }, [canReview, project, taskID, step, instance, reviewComments, reviewOutputs, onChangeComments, onChangeOutput])

  useEffect(() => {
    const timer = window.setTimeout(() => { void loadReviewDraft() }, 0)
    return () => window.clearTimeout(timer)
  }, [loadReviewDraft])

  if (!step) {
    return (
      <div className="p-4 text-sm text-neutral-500 dark:text-zinc-500">{t('workflows.detail.notSpecified')}</div>
    )
  }

  const decisionField = (step.outputFields ?? []).find((field) => field.name === 'decision')
  const decisionOptions = decisionField ? workflowDecisionOptions(decisionField.description) : []
  const usesDefaultReviewButtons = !decisionField || decisionOptions.length === 0 || isStandardReviewDecisionOptions(decisionOptions)
  const editableOutputFields = (step.outputFields ?? []).filter((field) => field.name !== 'decision')
  const reviewInputClass = (fieldName: string) => cn(
    'mt-1 w-full rounded-lg border bg-white px-3 py-2 text-sm text-neutral-900 outline-none dark:bg-zinc-900 dark:text-zinc-100',
    missingReviewField === fieldName
      ? 'border-red-400 focus:border-red-500 dark:border-red-500'
      : 'border-neutral-300 focus:border-sky-400 dark:border-zinc-700',
  )
  const hasInput = Boolean(instance?.inputArtifact?.trim()) || (step.inputFields ?? []).length > 0
  const showReadonlyOutput = !canReview && !isWorkflowStepOpen(instance?.status) && (
    Object.keys(outputValues).length > 0 ||
    Boolean(instance?.outputArtifact?.trim() || instance?.summary?.trim())
  )

  const isQASignoff = Boolean(step && (step.id === 'qa_signoff' || step.id.includes('qa_signoff')))
  const qaMatrixItems = useMemo(() => {
    if (!isQASignoff) return []
    const raw = inputValues['risk_coverage_matrix']
    if (!raw) return []
    try {
      const p = JSON.parse(raw)
      if (Array.isArray(p)) return p
      if (p && Array.isArray(p.items)) return p.items
      if (p && Array.isArray(p.matrix)) return p.matrix
      return []
    } catch {
      return []
    }
  }, [isQASignoff, inputValues])

  const highRiskFailedItems = useMemo(() => {
    return qaMatrixItems.filter((item: any) => String(item.risk_level).toLowerCase() === 'high' && String(item.status).toLowerCase() === 'failed')
  }, [qaMatrixItems])

  const highRiskUnpassedItems = useMemo(() => {
    return qaMatrixItems.filter((item: any) => String(item.risk_level).toLowerCase() === 'high' && String(item.status).toLowerCase() !== 'passed' && String(item.status).toLowerCase() !== 'failed')
  }, [qaMatrixItems])

  return (
    <WorkflowDocTitleContext.Provider value={docTitleMap}>
    <div className="flex min-h-0 flex-1 flex-col">
      {!hideHeader && (
        <div className="shrink-0 border-b border-neutral-100 px-4 py-3 dark:border-zinc-800">
          <p className="text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('workflows.detail.currentStep')}</p>
          <h4 className="mt-1 text-sm font-semibold text-neutral-900 dark:text-zinc-100">{step.title}</h4>
          <p className="mt-1 text-xs text-neutral-500 dark:text-zinc-500">
            {t(`workflows.stepTypes.${step.type}`, { defaultValue: step.type })} · {actorLabel}
          </p>
        </div>
      )}
      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto p-4">
        {step.description && (
          <WorkflowPanelBlock title={t('workflows.detail.goal')}>
            <p className="text-sm text-neutral-700 dark:text-zinc-300">{step.description}</p>
          </WorkflowPanelBlock>
        )}

        {hasInput && (
          <WorkflowPanelBlock title={t('workflows.detail.input')}>
            <WorkflowFieldList fields={step.inputFields ?? []} values={inputValues} project={project} taskID={taskID} />
            {instance?.inputArtifact && !hasStructuredInput && <WorkflowArtifact value={instance.inputArtifact} />}
          </WorkflowPanelBlock>
        )}

        {records.length > 0 && (
          <WorkflowPanelBlock title={t('workflows.detail.previousOutputs')}>
            <div className="space-y-3">
              {records.map((item) => {
                const itemStep = stepByIDMap.get(item.stepId)
                const run = workflowRunForRecord(runs, taskID, item)
                return (
                  <WorkflowRecordCard
                    key={item.id}
                    record={item}
                    step={itemStep}
                    run={run}
                    actorLabels={actorLabels}
                    project={project}
                    taskID={taskID}
                  />
                )
              })}
            </div>
          </WorkflowPanelBlock>
        )}

        {(canReview || showReadonlyOutput) && (
          <WorkflowPanelBlock title={t('workflows.detail.output')}>
            {canReview ? (
              <div className="space-y-3">
              {editableOutputFields.length === 0 && (
                <textarea
                  value={reviewComments}
                  onChange={(e) => onChangeComments(e.target.value)}
                  rows={4}
                  placeholder={t('workflows.review.commentsPlaceholder')}
                  className={cn(reviewInputClass('comments'), 'resize-y')}
                />
              )}
              {decisionField && !usesDefaultReviewButtons && (
                <label className="block">
                  <WorkflowFieldTitle fieldName={decisionField.name} description={decisionField.description} required />
                  <select
                    value={reviewOutputs.decision || ''}
                    onChange={(e) => onChangeOutput('decision', e.target.value)}
                    className={reviewInputClass('decision')}
                  >
                    <option value="">{t('workflows.review.selectDecision')}</option>
                    {decisionOptions.map((item) => (
                      <option key={item} value={item}>{workflowDecisionLabel(item)}</option>
                    ))}
                  </select>
                </label>
              )}
              {editableOutputFields.map((field) => {
                const isCommentsField = field.name === 'comments'
                const prUrl = (inputValues['pr_url'] || '').trim()
                const isHttpPr = Boolean(prUrl && prUrl.toLowerCase() !== 'none' && (prUrl.startsWith('http://') || prUrl.startsWith('https://')))
                const draftSource = draftApplied[isCommentsField ? 'comments' : field.name]

                return (
                  <label key={field.name} className="block">
                    <div className="flex items-center justify-between gap-2 mb-1">
                      <div className="flex items-center gap-1.5 min-w-0">
                        <WorkflowFieldTitle fieldName={field.name} description={field.description} required={!field.optional} />
                        {draftSource && (
                          <span
                            title={t('workflows.review.draftSource', { defaultValue: '草稿来源' }) + ': ' + draftSource}
                            className="shrink-0 rounded-full border border-amber-300/70 bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:border-amber-500/40 dark:bg-amber-950/40 dark:text-amber-300"
                          >
                            {t('workflows.review.draftBadge', { defaultValue: '草稿' })}
                          </span>
                        )}
                      </div>
                      <div className="flex items-center gap-2 shrink-0">
                        {isCommentsField && isHttpPr && (
                          <a
                            href={prUrl}
                            target="_blank"
                            rel="noreferrer"
                            className="inline-flex items-center gap-1 text-xs font-semibold text-sky-600 hover:text-sky-700 hover:underline dark:text-sky-400"
                          >
                            <GitPullRequest className="size-3.5" />
                            <span>{t('tasks.openPullRequest', { defaultValue: '打开 Pull Request ↗' })}</span>
                          </a>
                        )}
                        <button
                          type="button"
                          onClick={() => {
                            draftLoadedFor.current = null
                            setDraftApplied((prev) => {
                              const next = { ...prev }
                              delete next[isCommentsField ? 'comments' : field.name]
                              return next
                            })
                            void loadReviewDraft()
                          }}
                          disabled={draftBusy}
                          title={t('workflows.review.regenerateDraft', { defaultValue: '重新生成草稿' })}
                          className="inline-flex items-center gap-1 text-xs text-neutral-400 hover:text-sky-600 disabled:opacity-50 dark:text-zinc-500 dark:hover:text-sky-400"
                        >
                          <RotateCw className={cn('size-3', draftBusy && 'animate-spin')} />
                        </button>
                      </div>
                    </div>
                    <textarea
                      value={field.name === 'comments' ? reviewComments : reviewOutputs[field.name] || ''}
                      onChange={(e) => onChangeOutput(field.name, e.target.value)}
                      rows={field.name === 'comments' ? 3 : 2}
                      placeholder={field.optional ? t('workflows.review.optionalAutoRef', { defaultValue: '可留空：自动引用上游产物（ⓘ 查看说明）' }) : field.name}
                      className={cn(reviewInputClass(field.name), 'resize-y')}
                    />
                  </label>
                )
              })}
              {isQASignoff && highRiskFailedItems.length > 0 && (
                <div className="rounded-lg border border-red-300 bg-red-50 p-3 text-xs text-red-800 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-300 font-medium">
                  ⚠️ {t('workflows.qa.highRiskFailedAlert', { defaultValue: '存在高风险测试失败项，准入主干已被系统硬阻断。请点击「需要修改」打回修复。' })}
                </div>
              )}
              {isQASignoff && highRiskUnpassedItems.length > 0 && (
                <div className="rounded-lg border border-amber-200 bg-amber-50/60 p-3 dark:border-amber-900/40 dark:bg-amber-950/30">
                  <p className="text-xs font-semibold text-amber-800 dark:text-amber-300">
                    ⚠️ {t('workflows.qa.highRiskWaiverRequired', { defaultValue: '存在未执行或阻塞的高风险项，必须逐项填写特批豁免理由后方可批准：' })}
                  </p>
                  <div className="mt-2 space-y-2">
                    {highRiskUnpassedItems.map((item: any) => {
                      let currentWaivers: Record<string, string> = {}
                      try {
                        currentWaivers = JSON.parse(reviewOutputs['manual_waivers'] || '{}')
                      } catch {}
                      const itemId = item.item_id || 'unspecified'
                      return (
                        <div key={itemId} className="rounded border border-amber-200/80 bg-white p-2 text-xs dark:border-zinc-700 dark:bg-zinc-900">
                          <div className="flex items-center justify-between font-mono font-semibold text-neutral-800 dark:text-zinc-200">
                            <span>{itemId} ({item.status})</span>
                            <span className="text-[10px] text-amber-700 dark:text-amber-400 font-sans">需人工豁免</span>
                          </div>
                          {item.acceptance_criteria && (
                            <p className="mt-0.5 text-xs text-neutral-600 dark:text-zinc-400 font-sans">{item.acceptance_criteria}</p>
                          )}
                          <input
                            type="text"
                            value={currentWaivers[itemId] || ''}
                            onChange={(e) => {
                              const next = { ...currentWaivers, [itemId]: e.target.value }
                              onChangeOutput('manual_waivers', JSON.stringify(next))
                            }}
                            placeholder={t('workflows.qa.waiverPlaceholder', { defaultValue: '填写该高风险项的人工豁免/验证理由（必填）' })}
                            className="mt-1.5 w-full rounded border border-neutral-300 bg-white px-2 py-1 text-xs text-neutral-900 outline-none focus:border-amber-500 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-100"
                          />
                        </div>
                      )
                    })}
                  </div>
                </div>
              )}
              {reviewErr && <p className="text-sm text-red-600 dark:text-red-400">{reviewErr}</p>}
              <div className="flex justify-end gap-2">
                {usesDefaultReviewButtons ? (
                  <>
                    <button type="button" onClick={() => onSubmitReview('request_changes')} disabled={Boolean(reviewBusy)} className="rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm font-medium text-neutral-600 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800">
                      {reviewBusy === 'request_changes' ? t('forms.working') : t('workflows.review.requestChanges')}
                    </button>
                    <button
                      type="button"
                      onClick={() => onSubmitReview('approve')}
                      disabled={Boolean(reviewBusy) || (isQASignoff && highRiskFailedItems.length > 0)}
                      title={isQASignoff && highRiskFailedItems.length > 0 ? t('workflows.qa.highRiskBlockedTitle', { defaultValue: '存在高风险失败项，禁止准入' }) : undefined}
                      className="rounded-lg border border-sky-600 bg-white px-3 py-2 text-sm font-medium text-sky-700 hover:bg-sky-50 disabled:opacity-50 dark:border-sky-500 dark:bg-zinc-900 dark:text-sky-400 dark:hover:bg-zinc-800"
                    >
                      {reviewBusy === 'approve' ? t('forms.working') : t('workflows.review.approve')}
                    </button>
                  </>
                ) : (
                  <button type="button" onClick={() => onSubmitReview()} disabled={Boolean(reviewBusy)} className="rounded-lg border border-sky-600 bg-white px-3 py-2 text-sm font-medium text-sky-700 hover:bg-sky-50 disabled:opacity-50 dark:border-sky-500 dark:bg-zinc-900 dark:text-sky-400 dark:hover:bg-zinc-800">
                    {reviewBusy ? t('forms.working') : t('workflows.review.submit')}
                  </button>
                )}
              </div>
              </div>
            ) : (
              <>
                <WorkflowFieldList fields={step.outputFields ?? []} values={outputValues} project={project} taskID={taskID} />
                {Object.keys(outputValues).length > 0
                  ? null
                  : <WorkflowValuesOrArtifact values={instance?.outputValues} fields={step.outputFields} artifact={instance?.outputArtifact || instance?.summary} />}
              </>
            )}
          </WorkflowPanelBlock>
        )}
      </div>
    </div>
    </WorkflowDocTitleContext.Provider>
  )
}

function workflowActorLabel(actorType?: string, actorId?: string, labels?: Map<string, string>) {
  const id = actorId?.trim()
  if (!id) return '-'
  const label = labels?.get(id) || id
  if (actorType === 'human') return label
  return compactTaskIdentityLabel(id, label)
}

function compactTaskIdentityLabel(value?: string, label?: string) {
  const display = taskIdentityLabel(value, label)
  if (label && label !== value) return display
  const raw = value || display
  const slash = raw.lastIndexOf('/')
  return slash >= 0 ? raw.slice(slash + 1) : display
}

function workflowRunForRecord(runs: RunRow[], taskID: string, record: WorkflowRecord): RunRow | null {
  if (record.actorType !== 'agent' || !record.actorId) return null
  const candidates = runs.filter((run) => run.taskId === taskID && (`${run.project}/${run.agent}` === record.actorId || run.agent === record.actorId))
  if (candidates.length === 0) return null
  const started = Date.parse(record.startedAt || '')
  const finished = Date.parse(record.finishedAt || '')
  const hasWindow = Number.isFinite(started) || Number.isFinite(finished)
  if (!hasWindow) return candidates[0] ?? null
  return candidates
    .map((run) => {
      const runStarted = Date.parse(run.startedAt || '')
      const runFinished = Date.parse(run.finishedAt || '')
      const target = Number.isFinite(finished) ? finished : started
      const runTarget = Number.isFinite(runFinished) ? runFinished : runStarted
      const distance = Number.isFinite(runTarget) ? Math.abs(runTarget - target) : Number.MAX_SAFE_INTEGER
      return { run, distance }
    })
    .sort((a, b) => a.distance - b.distance)[0]?.run ?? null
}

function WorkflowRecordCard({ record, step, run, actorLabels, project, taskID }: { record: WorkflowRecord; step?: WorkflowStep; run: RunRow | null; actorLabels: Map<string, string>; project?: string; taskID?: string }) {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()
  const [open, setOpen] = useState(false)
  const inputValues = hasWorkflowValues(record.inputValues) ? record.inputValues! : parseWorkflowArtifact(record.inputArtifact || '')
  const outputValues = hasWorkflowValues(record.outputValues) ? record.outputValues! : parseWorkflowArtifact(record.outputArtifact || record.summary || '')
  const hasInput = hasWorkflowValues(inputValues) || Boolean(record.inputArtifact?.trim())
  const hasOutput = hasWorkflowValues(outputValues) || Boolean(record.outputArtifact?.trim() || record.summary?.trim())
  const time = record.finishedAt || record.startedAt || ('updatedAt' in record ? record.updatedAt : record.createdAt)
  const inputCount = Object.keys(inputValues).filter((key) => String(inputValues[key] ?? '').trim()).length
  const outputCount = Object.keys(outputValues).filter((key) => String(outputValues[key] ?? '').trim()).length
  const isDesignConfirm = Boolean(String(outputValues['approved_design_project_id'] ?? '').trim())

  return (
    <div className={cn('overflow-hidden rounded-lg border transition-colors', open ? 'border-sky-200 bg-white dark:border-sky-900/70 dark:bg-zinc-950' : 'border-neutral-100 bg-neutral-50 dark:border-zinc-800 dark:bg-zinc-900')}>
      <div className="flex items-stretch">
        <button
          type="button"
          onClick={() => setOpen((value) => !value)}
          className="min-w-0 flex-1 px-3 py-2.5 text-left transition-colors hover:bg-neutral-100 dark:hover:bg-zinc-800/80"
          aria-expanded={open}
        >
          <div className="flex min-w-0 items-start gap-2">
            <span className={cn('mt-0.5 inline-flex size-5 shrink-0 items-center justify-center rounded-full text-xs font-semibold transition-colors', open ? 'bg-sky-100 text-sky-700 dark:bg-sky-900/50 dark:text-sky-300' : 'bg-white text-neutral-400 dark:bg-zinc-950 dark:text-zinc-500')}>
              {open ? '−' : '+'}
            </span>
            <div className="min-w-0 flex-1">
              <p className="truncate text-sm font-semibold text-neutral-800 dark:text-zinc-100">{step?.title || record.stepId}</p>
              <p className="mt-0.5 truncate text-xs text-neutral-500 dark:text-zinc-500">
                {workflowActorLabel(record.actorType, record.actorId, actorLabels)} · {t(`workflows.stepStatus.${record.status}`, { defaultValue: record.status })}
                {time ? ` · ${fmt(time)}` : ''}
              </p>
              <div className="mt-1.5 flex flex-wrap gap-1.5">
                {isDesignConfirm && (
                  <span className="rounded-full border border-sky-200 bg-sky-50 px-2 py-0.5 text-[11px] font-medium text-sky-700 dark:border-sky-900/60 dark:bg-sky-950/60 dark:text-sky-300">
                    {t('designGate.title', { defaultValue: '设计确认' })}
                  </span>
                )}
                {hasInput && <WorkflowPayloadBadge label={t('workflows.detail.input')} count={inputCount} />}
                {hasOutput && <WorkflowPayloadBadge label={t('workflows.detail.output')} count={outputCount} />}
              </div>
            </div>
          </div>
        </button>
        <div className="flex shrink-0 items-start px-2 py-2">
          <WorkflowRunLink run={run} />
        </div>
      </div>
      {open && <div className="space-y-3 border-t border-neutral-100 p-3 dark:border-zinc-800">
        {hasInput && (
          <div>
            <p className="mb-1.5 text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('workflows.detail.input')}</p>
            <WorkflowValuesOrArtifact values={inputValues} fields={step?.inputFields} artifact={hasWorkflowValues(inputValues) ? undefined : record.inputArtifact} />
          </div>
        )}
        {hasOutput && (
          <div>
            <p className="mb-1.5 text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{t('workflows.detail.output')}</p>
            <WorkflowValuesOrArtifact values={outputValues} fields={step?.outputFields} artifact={hasWorkflowValues(outputValues) ? undefined : record.outputArtifact || record.summary} />
            {isDesignConfirm && project && taskID && (
              <div className="mt-2 border-t border-neutral-100 pt-2 dark:border-zinc-800">
                <DesignLaunchLink project={project} taskID={taskID} />
              </div>
            )}
          </div>
        )}
      </div>}
    </div>
  )
}

/**
 * Minted on view: design links carry a signed odt token with a 4h TTL, so the
 * URL is never persisted — the record card fetches a fresh one from
 * design/status each time it is expanded.
 */
function DesignLaunchLink({ project, taskID }: { project: string; taskID: string }) {
  const { t } = useTranslation()
  const [state, setState] = useState<'loading' | 'ok' | 'error'>('loading')
  const [url, setUrl] = useState('')
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => {
    let cancelled = false
    setState('loading')
    apiFetch<{ launchUrl?: string }>(
      `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/status`,
      { silentStatuses: [404, 500, 502, 503, 504] },
    )
      .then((data) => {
        if (cancelled) return
        if (data?.launchUrl) {
          setUrl(data.launchUrl)
          setState('ok')
        } else {
          setState('error')
        }
      })
      .catch(() => {
        if (!cancelled) setState('error')
      })
    return () => {
      cancelled = true
    }
  }, [project, taskID, reloadKey])

  if (state === 'loading') {
    return <p className="text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.designGate.approved.linkLoading', { defaultValue: '正在获取设计链接…' })}</p>
  }
  if (state === 'error') {
    return (
      <button
        type="button"
        onClick={() => setReloadKey((key) => key + 1)}
        className="text-xs text-neutral-500 transition-colors hover:text-neutral-700 dark:text-zinc-500 dark:hover:text-zinc-300"
      >
        {t('workflows.designGate.approved.linkUnavailable', { defaultValue: '设计链接获取失败，点击重试。' })}
      </button>
    )
  }
  return (
    <a
      href={url}
      target="_blank"
      rel="noreferrer"
      className="inline-flex items-center gap-1.5 text-xs font-medium text-sky-700 transition-colors hover:text-sky-800 dark:text-sky-400 dark:hover:text-sky-300"
    >
      <ExternalLink className="size-3.5" />
      {t('workflows.designGate.approved.view', { defaultValue: '查看设计' })}
    </a>
  )
}

function WorkflowPayloadBadge({ label, count }: { label: string; count: number }) {
  return (
    <span className="rounded-full border border-neutral-200 bg-white px-2 py-0.5 text-[11px] font-medium text-neutral-500 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-400">
      {count > 0 ? `${label} ${count}` : label}
    </span>
  )
}

function WorkflowRunLink({ run }: { run: RunRow | null }) {
  const { t } = useTranslation()
  if (!run) return null
  const href = `/projects/${encodeURIComponent(run.project)}/runs?agent=${encodeURIComponent(`${run.project}/${run.agent}`)}`
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className="shrink-0 rounded-md px-2 py-1 text-[11px] font-medium text-sky-700 hover:bg-sky-50 dark:text-sky-400 dark:hover:bg-sky-900/20"
      title={run.logPath || run.sessionId || undefined}
      onClick={(e) => e.stopPropagation()}
    >
      {t('tasks.viewExecutionRecord')}
    </a>
  )
}

function isWorkflowRuntimePrompt(prompt?: string) {
  return Boolean(prompt?.trimStart().startsWith('Continue this workflow task from the current active step.'))
}

function WorkflowValuesOrArtifact({ values, fields, artifact, compact = false }: { values?: Record<string, string>; fields?: WorkflowField[]; artifact?: string; compact?: boolean }) {
  const { t } = useTranslation()
  if (values && Object.keys(values).length > 0) {
    return <WorkflowValueMap values={values} fields={fields} compact={compact} />
  }
  if (artifact) {
    return <WorkflowArtifact value={artifact} compact={compact} />
  }
  return <p className="text-sm text-neutral-400 dark:text-zinc-600">{t('workflows.detail.notSpecified')}</p>
}

function hasWorkflowValues(values?: Record<string, string>) {
  return Boolean(values && Object.values(values).some((value) => String(value ?? '').trim()))
}

function RiskCoverageMatrixTable({ json }: { json: string }) {
  const { t } = useTranslation()
  const items = useMemo(() => {
    try {
      const parsed = JSON.parse(json)
      if (Array.isArray(parsed)) return parsed
      if (parsed && Array.isArray(parsed.items)) return parsed.items
      if (parsed && Array.isArray(parsed.matrix)) return parsed.matrix
      return null
    } catch {
      return null
    }
  }, [json])

  if (!items || items.length === 0) {
    return <WorkflowValueText value={json} />
  }

  return (
    <div className="mt-2 overflow-x-auto rounded-lg border border-neutral-200 dark:border-zinc-800">
      <table className="w-full text-left text-xs">
        <thead className="border-b border-neutral-200 bg-neutral-50 font-semibold text-neutral-700 dark:border-zinc-800 dark:bg-zinc-900/60 dark:text-zinc-300">
          <tr>
            <th className="px-3 py-2">{t('workflows.qa.itemId', { defaultValue: '验收项 / ID' })}</th>
            <th className="px-3 py-2">{t('workflows.qa.riskLevel', { defaultValue: '风险等级' })}</th>
            <th className="px-3 py-2">{t('workflows.qa.status', { defaultValue: '状态' })}</th>
            <th className="px-3 py-2">{t('workflows.qa.type', { defaultValue: '验证方式' })}</th>
            <th className="px-3 py-2">{t('workflows.qa.evidence', { defaultValue: '证据 / 未覆盖原因' })}</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-neutral-100 dark:divide-zinc-800/60 bg-white dark:bg-zinc-950">
          {items.map((item: any, idx: number) => {
            const risk = String(item.risk_level || 'low').toLowerCase()
            const status = String(item.status || 'not_run').toLowerCase()
            const isHigh = risk === 'high'
            return (
              <tr key={item.item_id || idx} className={cn(isHigh && status !== 'passed' ? 'bg-red-50/40 dark:bg-red-950/20' : undefined)}>
                <td className="px-3 py-2 align-top font-medium text-neutral-900 dark:text-zinc-100">
                  <div className="font-mono font-semibold text-[11px] text-sky-700 dark:text-sky-400">{item.item_id || `#${idx + 1}`}</div>
                  {item.acceptance_criteria && (
                    <div className="mt-0.5 text-xs text-neutral-600 dark:text-zinc-400 font-normal leading-relaxed">{item.acceptance_criteria}</div>
                  )}
                </td>
                <td className="px-3 py-2 align-top whitespace-nowrap">
                  <span className={cn(
                    'inline-block px-1.5 py-0.5 rounded text-[10px] font-semibold',
                    risk === 'high' ? 'bg-red-100 text-red-700 dark:bg-red-950/60 dark:text-red-300' :
                    risk === 'medium' ? 'bg-amber-100 text-amber-700 dark:bg-amber-950/60 dark:text-amber-300' :
                    'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400'
                  )}>
                    {risk.toUpperCase()}
                  </span>
                </td>
                <td className="px-3 py-2 align-top whitespace-nowrap">
                  <span className={cn(
                    'inline-block px-1.5 py-0.5 rounded text-[10px] font-medium',
                    status === 'passed' ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-300' :
                    status === 'failed' ? 'bg-red-100 text-red-700 dark:bg-red-950/60 dark:text-red-300' :
                    status === 'blocked' ? 'bg-orange-100 text-orange-700 dark:bg-orange-950/60 dark:text-orange-300' :
                    status === 'waived' ? 'bg-purple-100 text-purple-700 dark:bg-purple-950/60 dark:text-purple-300' :
                    'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400'
                  )}>
                    {status}
                  </span>
                </td>
                <td className="px-3 py-2 align-top whitespace-nowrap text-neutral-500 dark:text-zinc-400">
                  {item.execution_type || 'automated'}
                </td>
                <td className="px-3 py-2 align-top text-neutral-700 dark:text-zinc-300 max-w-xs break-words">
                  {item.evidence || item.uncovered_reason || '-'}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function WorkflowValueMap({ values, fields = [], compact = false }: { values: Record<string, string>; fields?: WorkflowField[]; compact?: boolean }) {
  const entries = Object.entries(values).filter(([, value]) => String(value ?? '').trim())
  const fieldByName = new Map(fields.map((field) => [field.name, field]))
  if (entries.length === 0) return null
  return (
    <div className={cn('space-y-2', compact ? 'max-h-56 overflow-y-auto' : '')}>
      {entries.map(([key, value]) => {
        const field = fieldByName.get(key)
        return (
          <div key={key} className="rounded-lg border border-neutral-100 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-950">
            <WorkflowFieldTitle fieldName={key} description={field?.description} />
            <div className="mt-2 rounded-md bg-neutral-50 px-3 py-2 break-words text-sm leading-relaxed text-neutral-800 dark:bg-zinc-900 dark:text-zinc-200">
              {key === 'risk_coverage_matrix' ? (
                <RiskCoverageMatrixTable json={String(value)} />
              ) : (
                <WorkflowValueText value={String(value)} />
              )}
            </div>
          </div>
        )
      })}
    </div>
  )
}

function WorkflowPanelBlock({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section>
      <h5 className="mb-2 text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">{title}</h5>
      {children}
    </section>
  )
}

function WorkflowFieldList({
  fields,
  values,
  project,
  taskID,
}: {
  fields: WorkflowField[]
  values: Record<string, string>
  project?: string
  taskID?: string
}) {
  const { t } = useTranslation()
  if (fields.length === 0) return null
  return (
    <div className="mb-2 space-y-2">
      {fields.map((field) => {
        const val = values[field.name]
        const isUrlField = (field.name === 'pr_url' || field.name === 'preview_url') && val && val.toLowerCase() !== 'none'
        const isHttpUrl = isUrlField && (val.startsWith('http://') || val.startsWith('https://') || val.startsWith('/preview/'))
        const isSnapshotField = (field.name === 'approved_design_snapshot_path' || field.name === 'approved_design_preview_url') && Boolean(val && val.toLowerCase() !== 'none' && project && taskID)

        return (
          <div key={field.name} className="rounded-lg border border-neutral-100 bg-white p-2.5 dark:border-zinc-800 dark:bg-zinc-950">
            <div className="flex items-center justify-between gap-2">
              <WorkflowFieldTitle fieldName={field.name} description={field.description} />
              <div className="flex items-center gap-2">
                {isHttpUrl && (
                  <a
                    href={val}
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex items-center gap-1 font-sans text-xs font-semibold text-sky-600 hover:text-sky-700 dark:text-sky-400"
                  >
                    <ExternalLink className="size-3" />
                    <span>{t('tasks.openDirectly', { defaultValue: '直接打开' })}</span>
                  </a>
                )}
                {isSnapshotField && (
                  <a
                    href={`/api/v1/projects/${encodeURIComponent(project!)}/tasks/${encodeURIComponent(taskID!)}/design/snapshot/`}
                    target="_blank"
                    rel="noreferrer"
                    className="inline-flex items-center gap-1 font-sans text-xs font-semibold text-violet-600 hover:text-violet-700 dark:text-violet-400"
                  >
                    <ExternalLink className="size-3" />
                    <span>{t('designGate.viewSnapshot', { defaultValue: '预览冻结设计快照' })}</span>
                  </a>
                )}
              </div>
            </div>
            {val && (
              <div className="mt-1.5 break-words text-sm text-neutral-800 dark:text-zinc-200">
                {field.name === 'risk_coverage_matrix' ? (
                  <RiskCoverageMatrixTable json={val} />
                ) : (
                  <WorkflowValueText value={val} />
                )}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

function WorkflowFieldTitle({ fieldName, description, required = false }: { fieldName: string; description?: string; required?: boolean }) {
  const raw = description?.trim()
  // Short visible label: the text before the first colon/period (gate copy is
  // written as "标签：完整指引"). The full description moves into the info-icon
  // tooltip so long gate guidance doesn't crowd the review dialog.
  const headMatch = raw ? raw.match(/^([^：:。．]{2,28})[：:。．]/) : null
  const label = headMatch ? headMatch[1].trim() : fieldName
  const showInfo = Boolean(raw && raw !== label)
  return (
    <div className="flex items-center gap-1.5">
      <p className="text-sm font-semibold leading-snug text-neutral-800 dark:text-zinc-200" title={fieldName}>
        {label}
        {required && <span className="ml-1 text-red-500">*</span>}
      </p>
      {showInfo && (
        <span className="group/info relative inline-flex shrink-0 cursor-help text-neutral-400 hover:text-neutral-600 dark:text-zinc-500 dark:hover:text-zinc-300">
          <Info className="size-3.5" />
          {/* hidden (not invisible): a hidden-but-laid-out tooltip still
              occupies overflow space, which gave the narrow side panel a
              permanent horizontal scrollbar. */}
          <span className="pointer-events-none hidden absolute left-0 top-full z-50 mt-1 w-80 max-w-[min(20rem,80vw)] whitespace-pre-wrap rounded-lg border border-neutral-200 bg-white p-2.5 text-xs leading-relaxed text-neutral-700 shadow-lg group-hover/info:block dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300">
            {raw}
          </span>
        </span>
      )}
      {raw && (
        <span className="font-mono text-[10px] uppercase tracking-wide text-neutral-400 dark:text-zinc-600">{fieldName}</span>
      )}
    </div>
  )
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

function workflowDecisionOptions(description?: string) {
  const text = String(description || '')
  const match = /(?:决策|決策|decision|请选择|請選擇|选择|選擇)\s*[:：]\s*([^。.;；]+)/i.exec(text)
  if (!match) return []
  return match[1]
    .split(/[、,，/|]|(?:\s+or\s+)|(?:\s+或\s+)|(?:\s+或者\s+)/i)
    .map((item) => item.trim())
    .filter(Boolean)
}

function isStandardReviewDecisionOptions(options: string[]) {
  const normalized = new Set(options.map((item) => normalizeReviewDecision(item)).filter(Boolean))
  return normalized.size === 2 && normalized.has('approve') && normalized.has('request_changes')
}

function workflowDecisionLabel(value: string) {
  return value.replaceAll('_', ' ')
}

let docPreviewZIndexCounter = 90

function nextDocPreviewZIndex() {
  docPreviewZIndexCounter += 1
  return docPreviewZIndexCounter
}

function WorkflowValueText({ value }: { value: string }) {
  const text = String(value ?? '')
  // The frozen design-gate snapshot inlines the approved page HTML (up to
  // 512KiB). Rendering that through the markdown view is unreadable; show a
  // compact receipt instead — the design card link is the way to view it.
  if (text.trimStart().startsWith('<!DOCTYPE') || text.trimStart().startsWith('<!doctype') || text.trimStart().startsWith('<html')) {
    const sizeKB = Math.max(1, Math.round(new Blob([text]).size / 1024))
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-neutral-500 dark:text-zinc-400">
        <span className="inline-block size-1.5 shrink-0 rounded-full bg-emerald-500" />
        HTML · {sizeKB} KB（已冻结快照）
      </span>
    )
  }
  const structured = parseWorkflowStructuredValue(text)
  if (structured !== null) {
    return <WorkflowStructuredValue value={structured} />
  }
  return <WorkflowPlainText value={text} />
}

type WorkflowStructuredJSON = string | number | boolean | null | WorkflowStructuredJSON[] | { [key: string]: WorkflowStructuredJSON }

function parseWorkflowStructuredValue(text: string): WorkflowStructuredJSON | null {
  const trimmed = text.trim()
  if (!trimmed || (!trimmed.startsWith('[') && !trimmed.startsWith('{'))) return null
  try {
    const parsed = JSON.parse(trimmed) as unknown
    if (Array.isArray(parsed)) return parsed as WorkflowStructuredJSON
    if (parsed && typeof parsed === 'object') return parsed as WorkflowStructuredJSON
  } catch {
    return null
  }
  return null
}

function WorkflowStructuredValue({ value }: { value: WorkflowStructuredJSON }) {
  if (Array.isArray(value)) {
    if (value.length === 0) return <span className="text-neutral-400 dark:text-zinc-500">[]</span>
    return (
      <ol className="space-y-1.5 pl-5 text-sm leading-relaxed [counter-reset:item]">
        {value.map((item, index) => (
          <li key={index} className="list-decimal pl-1 marker:text-xs marker:font-semibold marker:text-neutral-400 dark:marker:text-zinc-500">
            <WorkflowStructuredValue value={normalizeWorkflowJSON(item)} />
          </li>
        ))}
      </ol>
    )
  }
  if (value && typeof value === 'object') {
    const entries = Object.entries(value).filter(([, item]) => item !== undefined)
    if (entries.length === 0) return <span className="text-neutral-400 dark:text-zinc-500">{'{}'}</span>
    return (
      <div className="space-y-2">
        {entries.map(([key, item]) => (
          <div key={key} className="rounded-md border border-neutral-200/70 bg-white px-2.5 py-2 dark:border-zinc-700/70 dark:bg-zinc-950/60">
            <p className="mb-1 font-mono text-[11px] font-semibold text-neutral-400 dark:text-zinc-500">{key}</p>
            <WorkflowStructuredValue value={normalizeWorkflowJSON(item)} />
          </div>
        ))}
      </div>
    )
  }
  if (value === null) return <span className="text-neutral-400 dark:text-zinc-500">null</span>
  return <WorkflowPlainText value={String(value)} />
}

function normalizeWorkflowJSON(value: unknown): WorkflowStructuredJSON {
  if (Array.isArray(value)) return value.map(normalizeWorkflowJSON)
  if (value && typeof value === 'object') {
    const out: Record<string, WorkflowStructuredJSON> = {}
    for (const [key, item] of Object.entries(value)) out[key] = normalizeWorkflowJSON(item)
    return out
  }
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean' || value === null) return value
  return String(value)
}

function WorkflowPlainText({ value }: { value: string }) {
  const text = prepareWorkflowMarkdownValue(String(value ?? ''))
  return (
    <div className="prose prose-sm max-w-none text-neutral-800 prose-p:my-1.5 prose-ul:my-1.5 prose-ol:my-1.5 prose-li:my-0.5 prose-strong:text-neutral-900 dark:prose-invert dark:text-zinc-200 dark:prose-strong:text-zinc-100">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={workflowValueMarkdownComponents}>{text}</ReactMarkdown>
    </div>
  )
}

const workflowValueMarkdownComponents: Components = {
  p: ({ children }) => <p className="whitespace-pre-wrap leading-relaxed">{children}</p>,
  img: ({ src, alt }) => (
    <img src={src} alt={alt ?? ''} className="max-w-full rounded-lg shadow-sm" loading="lazy" referrerPolicy="no-referrer" />
  ),
  a: ({ href, children }) => {
    const rawHref = String(href || '')
    if (rawHref.startsWith('#doc-')) {
      const docID = decodeURIComponent(rawHref.slice('#doc-'.length))
      return <DocIDLink docID={docID} />
    }
    if (rawHref.startsWith('http://') || rawHref.startsWith('https://')) {
      return (
        <a
          href={rawHref}
          target="_blank"
          rel="noreferrer"
          className="font-medium text-sky-700 underline decoration-sky-300 underline-offset-2 hover:text-sky-800 dark:text-sky-400 dark:decoration-sky-700 dark:hover:text-sky-300"
          onClick={(e) => e.stopPropagation()}
        >
          {children}
        </a>
      )
    }
    return <a href={rawHref}>{children}</a>
  },
  code: ({ children }) => <code className="rounded bg-neutral-100 px-1 py-0.5 text-[0.86em] text-neutral-700 dark:bg-zinc-800 dark:text-zinc-200">{children}</code>,
}

const documentPreviewMarkdownComponents: Components = {
  img: ({ src, alt }) => (
    <img src={src} alt={alt ?? ''} className="max-w-full rounded-lg shadow-sm" loading="lazy" referrerPolicy="no-referrer" />
  ),
}

function prepareWorkflowMarkdownValue(value: string) {
  return linkWorkflowDocIDs(autolinkBareURLs(formatWorkflowReadableText(unescapeBreaks(value))))
}

// GFM autolink literals extend through any non-whitespace character, so a
// bare URL directly followed by CJK text or full-width punctuation
// (e.g. "...b072）。改动（与批准方案...") swallows the whole sentence into one
// link. Pre-wrap bare URLs in an explicit ASCII-bounded autolink so the link
// stops at the URL charset and trailing sentence punctuation stays plain text.
function autolinkBareURLs(value: string) {
  return value.replace(
    /https?:\/\/[A-Za-z0-9\-._~:/?#[\]!$&'()*+,;=%@]+/g,
    (match, offset: number, full: string) => {
      const before = offset > 0 ? full[offset - 1] : ''
      const after = full[offset + match.length] ?? ''
      // Already inside an explicit <...> autolink, a markdown link
      // destination, or an inline code span — leave it untouched.
      if (before === '<' || before === '(' || before === '[' || before === '`') return match
      if (after === '>' || after === '`') return match
      let url = match
      let trailing = ''
      // Trailing ASCII punctuation almost never belongs to the URL; an
      // unbalanced closing paren belongs to the surrounding sentence.
      for (;;) {
        const last = url[url.length - 1]
        if (last && /[.,;:!?'"]/.test(last)) {
          trailing = last + trailing
          url = url.slice(0, -1)
        } else if (last === ')' && (url.match(/\(/g) || []).length < (url.match(/\)/g) || []).length) {
          trailing = last + trailing
          url = url.slice(0, -1)
        } else {
          break
        }
      }
      if (!url) return match
      return `<${url}>${trailing}`
    },
  )
}

function formatWorkflowReadableText(value: string) {
  return value
    .replace(/([;；])\s*(?=\d+[)）])/g, '$1\n')
    .replace(/([。.!?！？])\s*(?=\(\d+[)）]|\d+[)）])/g, '$1\n')
    .replace(/([;；])\s*(?=[（(]\d+[)）])/g, '$1\n')
    .replace(/([。.!?！？])\s*(?=[（(]\d+[)）])/g, '$1\n')
}

function linkWorkflowDocIDs(value: string) {
  return value.replace(/\bdoc-\d{8}-[a-z0-9]+\b/gi, (docID, offset, fullText) => {
    const previous = fullText.slice(Math.max(0, offset - 8), offset)
    if (/]\($/.test(previous) || /https?:\/\/\S*$/i.test(previous)) return docID
    return `[${docID}](#doc-${encodeURIComponent(docID)})`
  })
}

function DocIDLink({ docID }: { docID: string }) {
  const { t } = useTranslation()
  const docTitles = useContext(WorkflowDocTitleContext)
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [doc, setDoc] = useState<DocPreview | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [dragOffset, setDragOffset] = useState({ x: 0, y: 0 })
  const [windowSize, setWindowSize] = useState({ width: 768, height: 560 })
  const [zoom, setZoom] = useState(1)
  const [zIndex, setZIndex] = useState(90)
  const [maximized, setMaximized] = useState(false)
  const dragRef = useRef({ active: false, startX: 0, startY: 0, originX: 0, originY: 0 })
  const resizeRef = useRef({ active: false, startX: 0, startY: 0, width: 768, height: 560 })
  const title = docTitles.get(docID)
  const parsedDoc = useMemo(() => parseDocPreviewContent(doc?.content || '', docID), [doc?.content, docID])
  const docMeta = parsedDoc.meta
  const bodyContent = parsedDoc.body
  const label = doc?.title || docMeta.title || title || docID

  const bringToFront = useCallback(() => {
    setZIndex(nextDocPreviewZIndex())
  }, [])

  useEffect(() => {
    if (!open) return
    function onMove(event: globalThis.PointerEvent) {
      const dragState = dragRef.current
      if (dragState.active) {
        const maxX = Math.max(80, window.innerWidth / 2 - 80)
        const maxY = Math.max(80, window.innerHeight / 2 - 80)
        setDragOffset({
          x: clampNumber(dragState.originX + event.clientX - dragState.startX, -maxX, maxX),
          y: clampNumber(dragState.originY + event.clientY - dragState.startY, -maxY, maxY),
        })
        return
      }
      const resizeState = resizeRef.current
      if (resizeState.active) {
        setWindowSize({
          width: clampNumber(resizeState.width + event.clientX - resizeState.startX, 520, Math.max(520, window.innerWidth - 48)),
          height: clampNumber(resizeState.height + event.clientY - resizeState.startY, 420, Math.max(420, window.innerHeight - 48)),
        })
      }
    }
    function onUp() {
      dragRef.current.active = false
      resizeRef.current.active = false
      document.body.style.userSelect = ''
    }
    window.addEventListener('pointermove', onMove)
    window.addEventListener('pointerup', onUp)
    window.addEventListener('pointercancel', onUp)
    return () => {
      window.removeEventListener('pointermove', onMove)
      window.removeEventListener('pointerup', onUp)
      window.removeEventListener('pointercancel', onUp)
      document.body.style.userSelect = ''
    }
  }, [open])

  async function openPreview(e: MouseEvent<HTMLAnchorElement>) {
    e.preventDefault()
    e.stopPropagation()
    bringToFront()
    setOpen(true)
    if (doc || loading) return
    setLoading(true)
    setErr(null)
    try {
      const res = await apiFetch<DocPreview>(`/api/v1/docs/${encodeURIComponent(docID)}?content=true`, { suppressToast: true })
      setDoc(res)
    } catch (error) {
      setErr(error instanceof Error ? error.message : String(error))
    } finally {
      setLoading(false)
    }
  }

  function beginDrag(e: PointerEvent<HTMLDivElement>) {
    if (e.button !== 0) return
    if (maximized) return
    if ((e.target as HTMLElement).closest('a,button')) return
    bringToFront()
    dragRef.current = {
      active: true,
      startX: e.clientX,
      startY: e.clientY,
      originX: dragOffset.x,
      originY: dragOffset.y,
    }
    document.body.style.userSelect = 'none'
  }

  function changeZoom(delta: number) {
    setZoom((value) => clampNumber(Math.round((value + delta) * 100) / 100, 0.75, 1.4))
  }

  function beginResize(e: PointerEvent<HTMLDivElement>) {
    e.preventDefault()
    e.stopPropagation()
    if (maximized) return
    bringToFront()
    resizeRef.current = {
      active: true,
      startX: e.clientX,
      startY: e.clientY,
      width: windowSize.width,
      height: windowSize.height,
    }
    document.body.style.userSelect = 'none'
  }

  const modal = open && typeof document !== 'undefined'
    ? createPortal(
      <div className="pointer-events-none fixed inset-0 flex items-center justify-center p-4" style={{ zIndex }}>
        <div
          className="pointer-events-auto relative flex flex-col overflow-hidden rounded-xl border border-neutral-200 bg-white shadow-2xl ring-1 ring-black/5 dark:border-zinc-700 dark:bg-zinc-950 dark:ring-white/10"
          style={{
            width: maximized ? 'calc(100vw - 32px)' : windowSize.width,
            height: maximized ? 'calc(100vh - 32px)' : windowSize.height,
            transform: maximized ? 'translate(0, 0)' : `translate(${dragOffset.x}px, ${dragOffset.y}px)`,
            transformOrigin: 'center',
          }}
          onClick={(e) => e.stopPropagation()}
          onPointerDownCapture={bringToFront}
        >
          <div
            className={cn('flex shrink-0 items-start justify-between gap-4 border-b border-neutral-200 px-5 py-4 dark:border-zinc-800', maximized ? 'cursor-default' : 'cursor-move')}
            onPointerDown={beginDrag}
            onDoubleClick={(e) => {
              if ((e.target as HTMLElement).closest('a,button')) return
              bringToFront()
              setMaximized((value) => !value)
            }}
          >
            <div className="min-w-0">
              <h3 className="truncate text-base font-semibold text-neutral-900 dark:text-zinc-100">{label}</h3>
              <p className="mt-1 truncate font-mono text-[11px] text-neutral-400 dark:text-zinc-500">{docMeta.docId || docID}</p>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <a
                href={`/docs/${encodeURIComponent(docID)}`}
                target="_blank"
                rel="noreferrer"
                className="rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-sm font-medium text-neutral-500 hover:bg-neutral-50 hover:text-neutral-800 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
              >
                {t('docs.openFullDocument', { defaultValue: 'Open document' })}
              </a>
              <button type="button" onClick={() => setOpen(false)} className="rounded-md p-1.5 text-neutral-400 hover:bg-neutral-100 dark:text-zinc-500 dark:hover:bg-zinc-800" aria-label={t('common.close')}>
                <X className="size-4" />
              </button>
            </div>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
            {loading && <DocPreviewSkeleton />}
            {err && <p className="text-sm text-red-600 dark:text-red-400">{err}</p>}
            {!loading && !err && (
              <div className="prose prose-sm max-w-none dark:prose-invert">
                <DocPreviewMetaBar meta={docMeta} updatedAt={doc?.updatedAt} />
                <div style={{ transform: `scale(${zoom})`, transformOrigin: 'top left', width: `${100 / zoom}%` }}>
                  <ReactMarkdown remarkPlugins={[remarkGfm]} components={documentPreviewMarkdownComponents}>{bodyContent || t('docs.emptyContent', { defaultValue: 'No content.' })}</ReactMarkdown>
                </div>
              </div>
            )}
          </div>
          <div className="absolute bottom-3 right-3 flex items-center rounded-lg border border-neutral-200 bg-white/90 p-0.5 shadow-sm backdrop-blur dark:border-zinc-700 dark:bg-zinc-900/90">
            <button type="button" onClick={() => changeZoom(-0.1)} className="rounded-md px-2 py-1 text-xs font-semibold text-neutral-500 hover:bg-neutral-100 hover:text-neutral-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100">-</button>
            <button type="button" onClick={() => setZoom(1)} className="min-w-12 rounded-md px-2 py-1 text-xs font-medium text-neutral-500 hover:bg-neutral-100 hover:text-neutral-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100">{Math.round(zoom * 100)}%</button>
            <button type="button" onClick={() => changeZoom(0.1)} className="rounded-md px-2 py-1 text-xs font-semibold text-neutral-500 hover:bg-neutral-100 hover:text-neutral-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100">+</button>
          </div>
          {!maximized && (
            <div
              className="absolute bottom-0 right-0 size-5 cursor-nwse-resize rounded-br-xl bg-gradient-to-br from-transparent via-transparent to-neutral-300 dark:to-zinc-600"
              onPointerDown={beginResize}
              aria-hidden="true"
            />
          )}
        </div>
      </div>,
      document.body,
    )
    : null

  return (
    <>
      <a
                href={`/docs/${encodeURIComponent(docID)}`}
        className="font-medium text-sky-700 underline decoration-sky-300 underline-offset-2 hover:text-sky-800 dark:text-sky-400 dark:decoration-sky-700 dark:hover:text-sky-300"
        title={title ? docID : undefined}
        onClick={openPreview}
      >
        {label}
      </a>
      {modal}
    </>
  )
}

function parseDocPreviewContent(content: string, fallbackDocID: string): { meta: DocPreviewMeta; body: string } {
  const raw = unescapeBreaks(content || '').trimStart()
  if (!raw) return { meta: { docId: fallbackDocID }, body: '' }

  const frontmatter = /^---\s*\n([\s\S]*?)\n---\s*\n?/.exec(raw)
  if (frontmatter) {
    const meta = parseDocMetaLines(frontmatter[1].split(/\r?\n/), fallbackDocID)
    return { meta, body: raw.slice(frontmatter[0].length).trimStart() }
  }

  const lines = raw.split(/\r?\n/)
  const metaLines: string[] = []
  let cursor = 0
  for (; cursor < lines.length; cursor += 1) {
    const line = lines[cursor]
    const trimmed = line.trim()
    if (!trimmed) {
      if (metaLines.length > 0) {
        cursor += 1
        break
      }
      break
    }
    if (/^#{1,6}\s/.test(trimmed)) break
    if (!/^(doc_id|docid|id|title|author|created_at|createdAt|tags)\s*:/i.test(trimmed)) break
    metaLines.push(line)
  }

  if (metaLines.length === 0) return { meta: { docId: fallbackDocID }, body: raw }
  const meta = parseDocMetaLines(metaLines, fallbackDocID)
  const body = lines.slice(cursor).join('\n').trimStart()
  return { meta, body }
}

function parseDocMetaLines(lines: string[], fallbackDocID: string): DocPreviewMeta {
  const meta: DocPreviewMeta = { docId: fallbackDocID }
  for (const line of lines) {
    const match = /^\s*([A-Za-z_][\w-]*)\s*:\s*(.*?)\s*$/.exec(line)
    if (!match) continue
    const key = match[1].toLowerCase().replace(/_/g, '')
    const value = match[2].trim().replace(/^["']|["']$/g, '')
    if (!value) continue
    if (key === 'docid' || key === 'id') meta.docId = value
    if (key === 'title') meta.title = value
    if (key === 'author') meta.author = value
    if (key === 'createdat') meta.createdAt = value
    if (key === 'tags') meta.tags = parseDocMetaTags(value)
  }
  return meta
}

function parseDocMetaTags(value: string): string[] {
  const normalized = value.replace(/^\[/, '').replace(/\]$/, '')
  return normalized
    .split(/[,，]/)
    .map((item) => item.trim().replace(/^["']|["']$/g, ''))
    .filter(Boolean)
}

function DocPreviewMetaBar({ meta, updatedAt }: { meta: DocPreviewMeta; updatedAt?: string }) {
  const { t } = useTranslation()
  const items = [
    meta.author ? { label: t('docs.meta.author', { defaultValue: '作者' }), value: meta.author } : null,
    meta.createdAt ? { label: t('docs.meta.createdAt', { defaultValue: '创建时间' }), value: formatDocMetaDate(meta.createdAt) } : null,
    !meta.createdAt && updatedAt ? { label: t('docs.meta.updatedAt', { defaultValue: '更新时间' }), value: formatDocMetaDate(updatedAt) } : null,
  ].filter(Boolean) as Array<{ label: string; value: string }>

  if (items.length === 0 && (!meta.tags || meta.tags.length === 0)) return null
  return (
    <div className="not-prose mb-5 rounded-lg border border-neutral-200/70 bg-neutral-50/70 px-3.5 py-2.5 text-xs text-neutral-500 dark:border-zinc-800 dark:bg-zinc-900/50 dark:text-zinc-400">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        {items.map((item) => (
          <span key={item.label} className="inline-flex items-center gap-1.5">
            <span className="text-neutral-400 dark:text-zinc-500">{item.label}</span>
            <span className="font-medium text-neutral-700 dark:text-zinc-200">{item.value}</span>
          </span>
        ))}
        {meta.tags && meta.tags.length > 0 && (
          <span className="inline-flex min-w-0 flex-wrap items-center gap-1.5">
            <span className="text-neutral-400 dark:text-zinc-500">{t('docs.meta.tags', { defaultValue: '标签' })}</span>
            {meta.tags.map((tag) => (
              <span key={tag} className="rounded-full border border-neutral-200 bg-white px-2 py-0.5 text-[11px] text-neutral-600 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-300">
                {tag}
              </span>
            ))}
          </span>
        )}
      </div>
    </div>
  )
}

function formatDocMetaDate(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

function DocPreviewSkeleton() {
  return (
    <div className="space-y-5">
      <div className="space-y-2">
        <div className="h-5 w-2/5 animate-pulse rounded-md bg-neutral-200 dark:bg-zinc-800" />
        <div className="h-3 w-1/4 animate-pulse rounded-md bg-neutral-100 dark:bg-zinc-900" />
      </div>
      <div className="space-y-3">
        <div className="h-3.5 w-full animate-pulse rounded-md bg-neutral-100 dark:bg-zinc-900" />
        <div className="h-3.5 w-11/12 animate-pulse rounded-md bg-neutral-100 dark:bg-zinc-900" />
        <div className="h-3.5 w-4/5 animate-pulse rounded-md bg-neutral-100 dark:bg-zinc-900" />
      </div>
      <div className="rounded-lg border border-neutral-100 bg-neutral-50 p-4 dark:border-zinc-800 dark:bg-zinc-900/70">
        <div className="space-y-2.5">
          <div className="h-3 w-1/3 animate-pulse rounded-md bg-neutral-200 dark:bg-zinc-800" />
          <div className="h-3 w-full animate-pulse rounded-md bg-neutral-200 dark:bg-zinc-800" />
          <div className="h-3 w-5/6 animate-pulse rounded-md bg-neutral-200 dark:bg-zinc-800" />
          <div className="h-3 w-2/3 animate-pulse rounded-md bg-neutral-200 dark:bg-zinc-800" />
        </div>
      </div>
      <div className="grid grid-cols-2 gap-3">
        <div className="h-24 animate-pulse rounded-lg bg-neutral-100 dark:bg-zinc-900" />
        <div className="h-24 animate-pulse rounded-lg bg-neutral-100 dark:bg-zinc-900" />
      </div>
    </div>
  )
}

function clampNumber(value: number, min: number, max: number) {
  return Math.min(max, Math.max(min, value))
}

function WorkflowArtifact({ value, compact = false }: { value: string; compact?: boolean }) {
  return (
    <div className={cn('rounded-lg bg-neutral-50 p-3 text-sm text-neutral-700 dark:bg-zinc-900 dark:text-zinc-300', compact ? 'max-h-40 overflow-y-auto' : 'max-h-64 overflow-y-auto')}>
      <div className="prose prose-sm max-w-none dark:prose-invert">
        <ReactMarkdown remarkPlugins={[remarkGfm]} components={documentPreviewMarkdownComponents}>{unescapeBreaks(value)}</ReactMarkdown>
      </div>
    </div>
  )
}

function parseWorkflowArtifact(value: string): Record<string, string> {
  const out: Record<string, string> = {}
  const unescaped = unescapeBreaks(value)
  try {
    const parsed = JSON.parse(unescaped)
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      const record = parsed as Record<string, unknown>
      const inputs = record.inputs && typeof record.inputs === 'object' && !Array.isArray(record.inputs)
        ? record.inputs as Record<string, unknown>
        : undefined
      const outputs = record.outputs && typeof record.outputs === 'object' && !Array.isArray(record.outputs)
        ? record.outputs as Record<string, unknown>
        : undefined
      const wrappedValues = inputs && Object.keys(inputs).length > 0
        ? inputs
        : outputs && Object.keys(outputs).length > 0
          ? outputs
          : record
      for (const [key, raw] of Object.entries(wrappedValues as Record<string, unknown>)) {
        if (typeof raw === 'string') out[key] = raw
        else out[key] = JSON.stringify(raw)
      }
      return out
    }
  } catch {
    // Fall back to old field: value snippets for incomplete historical runs.
  }
  const lines = unescaped.split('\n')
  for (const line of lines) {
    const match = line.match(/^\s*(?:[-*]\s*)?`?([A-Za-z0-9_.-]+)`?\s*[:：]\s*(.+?)\s*$/)
    if (!match) continue
    out[match[1]] = match[2]
  }
  return out
}

function fmtNum(n: number): string {
  return n.toLocaleString()
}

function buildRunResumeCmd(model: string | undefined, sessionId: string, agent: string, project: string): string {
  const m = (model ?? '').toLowerCase()
  if (m.includes('claude')) return `claude --resume ${sessionId}`
  if (m.includes('codex'))  return `codex exec resume ${sessionId}`
  if (m.includes('gemini')) return `gemini --resume ${sessionId}`
  if (m.includes('cursor')) return `agent --resume ${sessionId}`
  return `# session: ${sessionId}  (agent: ${agent}, project: ${project})`
}

function CopyRunResumeCmd({ model, sessionId, agent, project }: { model?: string; sessionId: string; agent: string; project: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  function doCopy() {
    const cmd = buildRunResumeCmd(model, sessionId, agent, project)
    void copyTextToClipboard(cmd).then((ok) => {
      if (!ok) return
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }
  return (
    <button
      type="button"
      onClick={doCopy}
      title={t('schedule.copyResumeCmd')}
      className="shrink-0 rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-600 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
    >
      {copied
        ? <span className="text-[10px] font-medium text-emerald-600 dark:text-emerald-400">✓</span>
        : <ClipboardCopy className="size-3.5" strokeWidth={2} />}
    </button>
  )
}
