import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { Check, ChevronDown, FileText, GitBranch, Paperclip, RefreshCw, X } from 'lucide-react'
import { apiPost, apiPostForm } from '../../lib/api'
import { cn } from '../../lib/cn'
import { useApiJson } from '../../lib/use-api'
import type { TaskOption } from '../task/TaskModals'
import { overlayDismissProps } from '../ui/overlay'
import { showToast } from '../ui/Toast'
import { DeliveryModeNotice } from './DeliveryModeNotice'
import { AssetMentionPopover, type AssetCandidate, type AssetMentionPopoverHandle, type AssetRole } from './AssetMentionPopover'
import type { ProjectRemoteState } from '../../lib/delivery-mode'

type UploadedAsset = { id: string; displayName: string; currentSha: string; size?: number }

// One asset staged for binding to the task about to be created. role/required
// ride the creation request itself (body.assets) — a post-create bind loop
// races the workflow's synchronous start and the attention wakeup.
type PendingBinding = {
  fileId: string
  displayName: string
  role: AssetRole
  required: boolean
  source: 'upload' | 'library'
}

const MAX_TASK_ASSETS = 8

const TASK_TYPES = ['chore', 'feature', 'bug', 'review', 'triage', 'test', 'research'] as const
const TEMPLATE_VAR_RE = /\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}/g

type AgentOpt = { name: string; model?: string }

type ProjectAgentsOpt = { projectId: string; agents: AgentOpt[] }
type WorkflowBranchOpt = { id: string; title: string; actorRole?: string; workflow?: WorkflowOpt }
type WorkflowStepOpt = { id: string; type: string; title: string; actorRole?: string; branches?: WorkflowBranchOpt[]; config?: Record<string, string> }
type WorkflowOpt = { id: string; name: string; steps?: WorkflowStepOpt[]; edges?: unknown[] }
type WorkflowListResponse = { workflows: WorkflowOpt[] }
type TaskTemplateVariable = { name: string; description?: string; required?: boolean; default?: string }
type TaskTemplateOpt = {
  id: string
  name: string
  description?: string
  project?: string
  type?: string
  priority: number
  labels?: string[]
  titleTemplate: string
  descriptionTemplate?: string
  promptTemplate: string
  workflowDefinitionId?: string
  workflowActorBindings?: Record<string, ActorBinding>
  variables?: TaskTemplateVariable[]
}
type TaskTemplateListResponse = { templates: TaskTemplateOpt[] }
type PersonOpt = { username: string; displayName?: string; disabled?: boolean }
type UserListResponse = PersonOpt[]
type ActorBinding = { type: 'agent' | 'human'; id: string }

type Props = {
  projectId: string
  agents: AgentOpt[]
  allProjectsAgents?: ProjectAgentsOpt[]
  taskOptions?: TaskOption[]
  onCreated: () => void
}

const fieldCls =
  'mt-1 w-full rounded-lg border border-neutral-300 bg-white px-2.5 py-1.5 text-sm text-neutral-900 outline-none transition-colors focus:border-sky-400 dark:border-zinc-600 dark:bg-zinc-800 dark:text-zinc-100'

const MENTION_POPOVER_W = 320
const MENTION_POPOVER_H = 240

// Caret pixel coordinates inside a textarea: mirror the text into an offscreen
// div with identical typography and measure a sentinel span at the caret.
// offsetTop/Left include the copied padding, so the result is relative to the
// textarea's border box — which is what the absolutely-positioned popover
// (anchored to the textarea's wrapper) needs, minus scroll.
function computeMentionPos(el: HTMLTextAreaElement, caret: number): { left: number; top: number } {
  const div = document.createElement('div')
  const style = getComputedStyle(el)
  for (const prop of [
    'fontFamily', 'fontSize', 'fontWeight', 'lineHeight', 'letterSpacing', 'tabSize',
    'textIndent', 'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft',
    'borderWidth', 'boxSizing', 'width', 'whiteSpace', 'overflowWrap', 'wordWrap',
  ] as const) {
    div.style[prop] = style[prop]
  }
  div.style.position = 'absolute'
  div.style.visibility = 'hidden'
  div.style.height = 'auto'
  div.textContent = el.value.slice(0, caret)
  const sentinel = document.createElement('span')
  sentinel.textContent = el.value.slice(caret) || '.'
  div.appendChild(sentinel)
  el.parentElement?.appendChild(div)
  const caretLeft = sentinel.offsetLeft
  const caretTop = sentinel.offsetTop
  el.parentElement?.removeChild(div)

  const lineHeight = parseFloat(style.lineHeight) || 20
  let left = caretLeft - el.scrollLeft
  let top = caretTop - el.scrollTop + lineHeight + 2
  if (el.parentElement) {
    if (left + MENTION_POPOVER_W > el.parentElement.clientWidth) {
      left = Math.max(0, el.parentElement.clientWidth - MENTION_POPOVER_W)
    }
    // Not enough room below the caret → open above it.
    if (top + MENTION_POPOVER_H > el.parentElement.clientHeight && caretTop - el.scrollTop - MENTION_POPOVER_H >= 0) {
      top = caretTop - el.scrollTop - MENTION_POPOVER_H - 4
    }
  }
  return { left: Math.max(0, left), top: Math.max(0, top) }
}

export function CreateTaskDialog({ projectId: defaultProjectId, agents: defaultAgents, allProjectsAgents, taskOptions = [], onCreated }: Props) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [createMode, setCreateMode] = useState<'blank' | 'template'>('blank')
  const [selectedProject, setSelectedProject] = useState(defaultProjectId)
  const [agent, setAgent] = useState('')
  const [title, setTitle] = useState('')
  const [description, setDescription] = useState('')
  const [prompt, setPrompt] = useState('')
  const [taskType, setTaskType] = useState<string>('chore')
  const [priority, setPriority] = useState(2)
  const [assignee, setAssignee] = useState('')
  const [labelsStr, setLabelsStr] = useState('')
  const [dueDate, setDueDate] = useState('')
  const [parentId, setParentId] = useState('')
  const [estimateDuration, setEstimateDuration] = useState('')
  const [workflowDefinitionId, setWorkflowDefinitionId] = useState('')
  const [actorBindings, setActorBindings] = useState<Record<string, ActorBinding>>({})
  const [taskTemplateId, setTaskTemplateId] = useState('')
  const [templateInputs, setTemplateInputs] = useState<Record<string, string>>({})
  const [baseBranch, setBaseBranch] = useState('main')
  const [branchName, setBranchName] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  // Project assets staged for binding to the task about to be created. Files
  // upload into the project library immediately; the bindings are sent with
  // the creation request (body.assets) so the task never starts without them.
  const [taskAssets, setTaskAssets] = useState<PendingBinding[]>([])
  const [assetsBusy, setAssetsBusy] = useState(false)
  const assetInputRef = useRef<HTMLInputElement>(null)
  const promptInputRef = useRef<HTMLTextAreaElement>(null)
  // @-mention state: the caret offset where "@" was typed, and the query
  // string tracked after it. Null popover = closed.
  const [mention, setMention] = useState<{ at: number; query: string } | null>(null)
  const [mentionPos, setMentionPos] = useState<{ left: number; top: number }>({ left: 0, top: 0 })
  const [libraryAssets, setLibraryAssets] = useState<AssetCandidate[]>([])
  const composingRef = useRef(false)
  const mentionPopRef = useRef<AssetMentionPopoverHandle>(null)

  const multiProject = Boolean(allProjectsAgents && allProjectsAgents.length > 1)
  const workflowPath = open ? '/api/v1/workflows' : null
  const workflowsState = useApiJson<WorkflowListResponse>(workflowPath, 0)
  const templatesState = useApiJson<TaskTemplateListResponse>(
    open && selectedProject ? `/api/v1/projects/${encodeURIComponent(selectedProject)}/task-templates` : null,
    0,
  )
  const [branchRefreshKey, setBranchRefreshKey] = useState(0)
  const [branchRefreshing, setBranchRefreshing] = useState(false)
  const branchesState = useApiJson<BranchesResponse>(
    open && selectedProject ? `/api/v1/projects/${encodeURIComponent(selectedProject)}/branches` : null,
    branchRefreshKey,
  )
  const usersState = useApiJson<UserListResponse>(open ? '/api/v1/users' : null, 0)
  // Project asset library for the @-mention picker. Non-archived files only:
  // archived files cannot be bound (the bind endpoint refuses them).
  const [libraryRefreshKey, setLibraryRefreshKey] = useState(0)
  const libraryState = useApiJson<{ id: string; displayName: string; currentSha: string; size?: number; archivedAt?: string }[]>(
    open && selectedProject ? `/api/v1/projects/${encodeURIComponent(selectedProject)}/assets` : null,
    libraryRefreshKey,
  )
  useEffect(() => {
    if (libraryState.status === 'ok') {
      setLibraryAssets(libraryState.data.filter((f) => !f.archivedAt))
    }
  }, [libraryState])
  // Only the derived fields are read here. remoteProvider / remoteConnection on
  // the same payload are client-writable and must never drive this decision.
  const projectRemoteState = useApiJson<ProjectRemoteState>(
    open && selectedProject ? `/api/v1/projects/${encodeURIComponent(selectedProject)}` : null,
    0,
  )
  const workflows = workflowsState.status === 'ok' ? workflowsState.data.workflows : []
  const taskTemplates = templatesState.status === 'ok' ? templatesState.data.templates : []
  const availableBranches = useMemo(() => {
    if (branchesState.status === 'ok' && Array.isArray(branchesState.data.branches) && branchesState.data.branches.length > 0) {
      return branchesState.data.branches
    }
    return ['main']
  }, [branchesState])
  const branchInfos = useMemo(() => {
    if (branchesState.status !== 'ok' || !Array.isArray(branchesState.data.branchInfos)) return []
    return branchesState.data.branchInfos
  }, [branchesState])
  const refreshBranches = async () => {
    if (branchRefreshing || !selectedProject) return
    setBranchRefreshing(true)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(selectedProject)}/branches/refresh`, {})
    } catch {
      // Refresh is best-effort; the list stays usable with local refs.
    }
    setBranchRefreshKey((k) => k + 1)
    setBranchRefreshing(false)
  }
  const people = usersState.status === 'ok' ? usersState.data.filter((p) => !p.disabled) : []
  const selectedWorkflow = workflows.find((wf) => wf.id === workflowDefinitionId)
  const selectedTemplate = taskTemplates.find((template) => template.id === taskTemplateId)

  const currentAgents = useMemo(() => {
    if (!allProjectsAgents) return defaultAgents
    return allProjectsAgents.find((p) => p.projectId === selectedProject)?.agents ?? []
  }, [allProjectsAgents, selectedProject, defaultAgents])

  const currentAgentActors = useMemo(() => currentAgents.filter((a) => a.model !== 'human'), [currentAgents])
  const currentHumanMembers = useMemo(() => currentAgents.filter((a) => a.model === 'human'), [currentAgents])

  const allAgentAssignees = useMemo(() => {
    if (!allProjectsAgents) return defaultAgents.filter((a) => a.model !== 'human').map((a) => ({ projectId: defaultProjectId, name: a.name }))
    return allProjectsAgents.flatMap((p) => p.agents.filter((a) => a.model !== 'human').map((a) => ({ projectId: p.projectId, name: a.name })))
  }, [allProjectsAgents, defaultAgents, defaultProjectId])

  const humanAssignees = useMemo(() => {
    const byID = new Map<string, { id: string; label: string }>()
    for (const member of currentHumanMembers) {
      if (!member.name) continue
      byID.set(member.name, { id: member.name, label: member.name })
    }
    for (const person of people) {
      if (!person.username) continue
      byID.set(person.username, {
        id: person.username,
        label: person.displayName ? `${person.displayName} (${person.username})` : person.username,
      })
    }
    return Array.from(byID.values())
  }, [currentHumanMembers, people])

  function reset() {
    setCreateMode('blank')
    setSelectedProject(defaultProjectId)
    setAgent('')
    setTitle('')
    setDescription('')
    setPrompt('')
    setTaskType('chore')
    setPriority(2)
    setAssignee('')
    setLabelsStr('')
    setDueDate('')
    setParentId('')
    setEstimateDuration('')
    setWorkflowDefinitionId('')
    setActorBindings({})
    setTaskTemplateId('')
    setTemplateInputs({})
    setFieldErrors({})
    setErr(null)
    setTaskAssets([])
    setMention(null)
  }

  function openDialog() {
    reset()
    setOpen(true)
    setTimeout(() => {
      const first = allProjectsAgents
        ? (allProjectsAgents.find((p) => p.projectId === defaultProjectId)?.agents.find((a) => a.model !== 'human')?.name ?? '')
        : (defaultAgents.find((a) => a.model !== 'human')?.name ?? '')
      setAgent(first)
    }, 0)
  }

  function onProjectChange(proj: string) {
    setSelectedProject(proj)
    const projAgents = allProjectsAgents?.find((p) => p.projectId === proj)?.agents ?? []
    setAgent(projAgents.find((a) => a.model !== 'human')?.name ?? '')
    setActorBindings({})
    setTaskTemplateId('')
    setTemplateInputs({})
    setFieldErrors({})
    // Uploaded files live in the project they were uploaded to; switching
    // projects leaves them behind (still in that project's library).
    setTaskAssets([])
    setMention(null)
  }

  async function onAssetsSelected(files: FileList | null) {
    if (!files || !files.length || !selectedProject) return
    const room = MAX_TASK_ASSETS - taskAssets.length
    if (room <= 0) {
      setErr(t('tasks.assets.maxReached'))
      return
    }
    const chosen = Array.from(files).slice(0, room)
    setAssetsBusy(true)
    setErr(null)
    try {
      const form = new FormData()
      for (const f of chosen) form.append('file', f)
      const uploaded = await apiPostForm<UploadedAsset[]>(
        `/api/v1/projects/${encodeURIComponent(selectedProject)}/assets`,
        form,
      )
      setTaskAssets((prev) => [
        ...prev,
        ...uploaded.map((a) => ({ fileId: a.id, displayName: a.displayName, role: 'reference' as AssetRole, required: false, source: 'upload' as const })),
      ])
      if (chosen.length < files.length) setErr(t('tasks.assets.maxReached'))
    } catch (e) {
      // The file never reached the library, so there is nothing to bind —
      // block the submit with the error instead of continuing without it.
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setAssetsBusy(false)
      if (assetInputRef.current) assetInputRef.current.value = ''
    }
  }

  function stageBinding(fileId: string) {
    if (!mention) return
    const candidate = libraryAssets.find((a) => a.id === fileId)
    const displayName = candidate?.displayName ?? ''
    setTaskAssets((prev) => {
      // Already staged (e.g. uploaded this session): keep the chip exactly as
      // it is — no duplicate, role untouched (user feedback: @-picking a
      // session upload must not read as a second upload).
      if (prev.some((b) => b.fileId === fileId)) return prev
      return [
        ...prev,
        { fileId, displayName, role: 'reference' as AssetRole, required: false, source: 'library' as const },
      ].slice(0, MAX_TASK_ASSETS)
    })
    closeMentionAndInsert(displayName)
  }

  function removeBinding(fileId: string) {
    // Remove the chip AND the marker this picker inserted (3.4 sync rule):
    // a stale "@name" in the text with no binding behind it reads as a live
    // reference. Hand-written text is never touched — only markers we
    // recorded.
    const binding = taskAssets.find((b) => b.fileId === fileId)
    setTaskAssets((prev) => prev.filter((b) => b.fileId !== fileId))
    if (!binding) return
    setPrompt((prev) => {
      const marker = `@${binding.displayName}`
      const idx = prev.indexOf(marker)
      if (idx === -1) return prev
      const before = prev.slice(0, idx)
      const after = prev.slice(idx + marker.length)
      return (before + after).replace(/ {2,}/g, ' ').trimEnd()
    })
  }

  function openMention(textarea: HTMLTextAreaElement) {
    const caret = textarea.selectionStart ?? textarea.value.length
    setMention({ at: caret, query: '' })
    setMentionPos(computeMentionPos(textarea, caret))
    setLibraryRefreshKey((k) => k + 1)
  }

  function closeMentionAndInsert(displayName: string) {
    if (!mention) return
    const { at, query } = mention
    let insertLen = 0
    setPrompt((prev) => {
      // Replace "@query" (the chars typed since the "@") with the full name.
      const before = prev.slice(0, at)
      const typedQuery = prev.slice(at, at + 1 + query.length)
      const keptQuery = typedQuery.startsWith('@') ? typedQuery.slice(1) : query
      const after = prev.slice(at + 1 + keptQuery.length)
      const needsSpaceBefore = before.length > 0 && !/\s$/.test(before)
      // The marker may already exist (file @-picked earlier); a second
      // occurrence would desync chip removal.
      const marker = `@${displayName}`
      const markerText = (before + after).includes(marker) ? '' : `${marker} `
      const insert = `${needsSpaceBefore && markerText ? ' ' : ''}${markerText}`
      insertLen = insert.length
      return before + insert + after
    })
    setMention(null)
    requestAnimationFrame(() => {
      const el = promptInputRef.current
      if (!el) return
      const pos = at + insertLen
      el.focus()
      el.setSelectionRange(pos, pos)
    })
  }

  function onPromptChange(e: React.ChangeEvent<HTMLTextAreaElement>) {
    clearFieldError('prompt')
    const value = e.target.value
    const caret = e.target.selectionStart ?? value.length
    setPrompt(value)
    if (composingRef.current) return
    setMention((prev) => {
      if (prev) {
        // Keep tracking while the "@anchor" is still in the text.
        if (value[prev.at] !== '@') return null
        const between = value.slice(prev.at + 1, caret)
        if (/\s/.test(between)) return null
        return { at: prev.at, query: between }
      }
      // Trigger: "@" right after start/whitespace with a preceding space
      // boundary, typed at the caret (not an old "@" elsewhere in the text).
      if (value[caret - 1] !== '@') return null
      if (caret >= 2 && !/\s/.test(value[caret - 2])) return null
      return { at: caret - 1, query: '' }
    })
    // The caret (and thus the popover anchor) moves while the query grows.
    if (mention) setMentionPos(computeMentionPos(e.target, caret))
    // Library bindings live in the text as "@name" markers: deleting the
    // marker from the text un-binds (ZCode-style). Upload chips persist —
    // the paperclip flow owns those, the mention is optional prose.
    setTaskAssets((prev) => {
      const stale = prev.filter((b) => b.source === 'library' && !value.includes(`@${b.displayName}`))
      return stale.length === 0 ? prev : prev.filter((b) => !stale.includes(b))
    })
  }

  function onCreateModeChange(mode: 'blank' | 'template') {
    setCreateMode(mode)
    if (mode === 'blank') {
      setTaskTemplateId('')
      setTemplateInputs({})
      return
    }
    if (!taskTemplateId && taskTemplates.length > 0) {
      onTemplateChange(taskTemplates[0].id)
    }
  }

  const parentChoices = useMemo(() => {
    return taskOptions.filter((o) => !o.project || o.project === selectedProject)
  }, [taskOptions, selectedProject])

  const workflowActorSlots = useMemo(() => {
    const steps = selectedWorkflow?.steps ?? []
    const slots: { key: string; role: string; preferredType: 'agent' | 'human'; titles: string[] }[] = []
    function addStep(step: WorkflowStepOpt, titlePrefix = '') {
      const role = step.actorRole?.trim()
      if (!role) return
      const preferredType = step.type === 'human_review' ? 'human' : 'agent'
      const title = titlePrefix ? `${titlePrefix} / ${step.title}` : step.title
      const key = step.id?.trim() || role
      slots.push({ key, role, preferredType, titles: [title] })
    }
    for (const step of steps) {
      if (step.type === 'parallel_stage') {
        for (const branch of step.branches ?? []) {
          const branchTitle = `${step.title} / ${branch.title || branch.id}`
          const childSteps = branch.workflow?.steps ?? []
          if (childSteps.length > 0) {
            for (const childStep of childSteps) addStep(childStep, branchTitle)
            continue
          }
          const role = (branch.actorRole || branch.id).trim()
          if (!role) continue
          slots.push({ key: branch.id.trim() || role, role, preferredType: 'agent', titles: [branchTitle] })
        }
        continue
      }
      addStep(step)
    }
    return slots
  }, [selectedWorkflow])

  function requiredMessage() {
    return t('forms.required', { defaultValue: t('forms.fillRequired') })
  }

  function clearFieldError(name: string) {
    setFieldErrors((current) => {
      if (!current[name]) return current
      const next = { ...current }
      delete next[name]
      return next
    })
  }

  function controlClass(name: string) {
    return cn(
      fieldCls,
      fieldErrors[name] && 'border-red-400 bg-red-50/40 focus:border-red-500 dark:border-red-500/70 dark:bg-red-950/20',
    )
  }

  function fieldError(name: string) {
    const message = fieldErrors[name]
    if (!message) return null
    return <p className="mt-1 text-xs text-red-600 dark:text-red-400">{message}</p>
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    setErr(null)
    const nextFieldErrors: Record<string, string> = {}
    const required = requiredMessage()
    if (!agent.trim()) {
      nextFieldErrors.agent = required
      setErr(t('forms.fillRequired'))
      setFieldErrors(nextFieldErrors)
      return
    }
    if (createMode === 'template') {
      if (!selectedTemplate) {
        nextFieldErrors.template = required
        setErr(t('taskTemplates.selectRequired'))
        setFieldErrors(nextFieldErrors)
        return
      }
    } else if (!title.trim() || !prompt.trim()) {
      if (!title.trim()) nextFieldErrors.title = required
      if (!prompt.trim()) nextFieldErrors.prompt = required
      setErr(t('forms.fillRequired'))
      setFieldErrors(nextFieldErrors)
      return
    }
    if (workflowDefinitionId && missingWorkflowActors.length > 0) {
      for (const slot of missingWorkflowActors) {
        nextFieldErrors[`actor:${slot.key}`] = required
      }
      setErr(t('workflows.actorBindingsRequired'))
      setFieldErrors(nextFieldErrors)
      return
    }
    if (selectedTemplate) {
      for (const variable of selectedTemplate.variables ?? []) {
        if (variable.required && !templateInputs[variable.name]?.trim()) {
          nextFieldErrors[`variable:${variable.name}`] = required
          setErr(t('taskTemplates.variableRequired', { name: variable.name }))
          setFieldErrors(nextFieldErrors)
          return
        }
      }
    }
    setFieldErrors({})
    if (taskAssets.length > MAX_TASK_ASSETS) {
      setErr(t('tasks.assets.maxReached'))
      return
    }
    setBusy(true)
    try {
      const labels = labelsStr.split(',').map(l => l.trim()).filter(Boolean)
      // Bindings ride the creation request itself: the workflow start,
      // attention wakeup, and autoStart all fire before the 201 response —
      // a post-create bind loop would race them (empty manifest).
      const assetBindings = taskAssets.map((a) => ({ fileId: a.fileId, role: a.role, required: a.required }))
      if (createMode === 'template' && selectedTemplate) {
        await apiPost<{ id: string }>(
          `/api/v1/projects/${encodeURIComponent(selectedProject)}/tasks/from-template`,
          {
              templateId: selectedTemplate.id,
              inputs: templateInputs,
              agent: agent.trim(),
              ...(assignee ? { assignee } : {}),
              ...(labels.length > 0 ? { labels } : {}),
              ...(dueDate ? { dueDate } : {}),
              ...(parentId ? { parentId } : {}),
              ...(estimateDuration.trim() ? { estimateDuration: estimateDuration.trim() } : {}),
              ...(baseBranch.trim() ? { baseBranch: baseBranch.trim() } : {}),
              ...(branchName.trim() ? { branchName: branchName.trim() } : {}),
              workflowActorBindings: actorBindings,
              ...(assetBindings.length > 0 ? { assets: assetBindings } : {}),
          },
        )
      } else {
        await apiPost<{ id: string }>(
          `/api/v1/projects/${encodeURIComponent(selectedProject)}/tasks`,
          {
            agent: agent.trim(),
            title: title.trim(),
            description: description.trim(),
            prompt: prompt.trim(),
            type: taskType,
            priority,
            ...(assignee ? { assignee } : {}),
            ...(labels.length > 0 ? { labels } : {}),
            ...(dueDate ? { dueDate } : {}),
            ...(parentId ? { parentId } : {}),
            ...(estimateDuration.trim() ? { estimateDuration: estimateDuration.trim() } : {}),
            ...(baseBranch.trim() ? { baseBranch: baseBranch.trim() } : {}),
            ...(branchName.trim() ? { branchName: branchName.trim() } : {}),
            ...(workflowDefinitionId ? { workflowDefinitionId } : {}),
            ...(workflowDefinitionId ? { workflowActorBindings: actorBindings } : {}),
            ...(assetBindings.length > 0 ? { assets: assetBindings } : {}),
          },
        )
      }
      setOpen(false)
      onCreated()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  function autoBindingFor(role: string, preferredType: 'agent' | 'human'): ActorBinding {
    if (preferredType === 'agent') {
      const exact = currentAgentActors.find((a) => a.name === role)
      const weak = currentAgentActors.find((a) => role.includes(a.name) || a.name.includes(role.replace(/-agent$/, '')))
      return { type: 'agent', id: exact?.name || weak?.name || currentAgentActors[0]?.name || '' }
    }
    return { type: 'human', id: currentHumanMembers[0]?.name || people[0]?.username || '' }
  }

  function workflowDefaultBindings(workflow?: WorkflowOpt): Record<string, ActorBinding> {
    const next: Record<string, ActorBinding> = {}
    function addRole(step: WorkflowStepOpt) {
      const role = step.actorRole?.trim()
      if (!role) return
      const key = step.id?.trim() || role
      if (next[key]) return
      next[key] = autoBindingFor(role, step.type === 'human_review' ? 'human' : 'agent')
    }
    for (const step of workflow?.steps ?? []) {
      if (step.type === 'parallel_stage') {
        for (const branch of step.branches ?? []) {
          const childSteps = branch.workflow?.steps ?? []
          if (childSteps.length > 0) {
            for (const childStep of childSteps) addRole(childStep)
            continue
          }
          const role = (branch.actorRole || branch.id).trim()
          const key = branch.id.trim() || role
          if (!role || next[key]) continue
          next[key] = autoBindingFor(role, 'agent')
        }
        continue
      }
      addRole(step)
    }
    return next
  }

  function onWorkflowChange(id: string) {
    setWorkflowDefinitionId(id)
    const workflow = workflows.find((wf) => wf.id === id)
    setActorBindings(workflowDefaultBindings(workflow))
  }

  function onTemplateChange(id: string) {
    setTaskTemplateId(id)
    const template = taskTemplates.find((item) => item.id === id)
    if (!template) {
      setTemplateInputs({})
      setWorkflowDefinitionId('')
      setActorBindings({})
      return
    }
    setTaskType(template.type || 'chore')
    setPriority(template.priority ?? 2)
    setLabelsStr('')
    setTitle(template.titleTemplate)
    setDescription(template.descriptionTemplate || '')
    setPrompt(template.promptTemplate)
    const inputs: Record<string, string> = {}
    for (const variable of template.variables ?? []) {
      inputs[variable.name] = variable.default || ''
    }
    setTemplateInputs(inputs)
    if (template.workflowDefinitionId) {
      setWorkflowDefinitionId(template.workflowDefinitionId)
      if (template.workflowActorBindings && Object.keys(template.workflowActorBindings).length > 0) {
        setActorBindings(template.workflowActorBindings)
      } else {
        const workflow = workflows.find((wf) => wf.id === template.workflowDefinitionId)
        setActorBindings(workflowDefaultBindings(workflow))
      }
    } else {
      setWorkflowDefinitionId('')
      setActorBindings({})
    }
  }

  function renderTemplatePreview(value: string | undefined) {
    if (!value) return ''
    return value.replace(TEMPLATE_VAR_RE, (_, name: string) => templateInputs[name]?.trim() || `{{${name}}}`)
  }

  function updateActorBinding(role: string, patch: Partial<ActorBinding>) {
    clearFieldError(`actor:${role}`)
    setActorBindings((current) => {
      const prev = current[role] ?? { type: 'agent', id: '' }
      const nextType = patch.type ?? prev.type
      let nextID = patch.id ?? prev.id
      if (patch.type && patch.type !== prev.type) {
        nextID = patch.type === 'agent' ? currentAgentActors[0]?.name || '' : currentHumanMembers[0]?.name || people[0]?.username || ''
      }
      return { ...current, [role]: { type: nextType, id: nextID } }
    })
  }

  const bindingForSlot = (slot: { key: string; role: string }) => actorBindings[slot.key] ?? actorBindings[slot.role]
  const missingWorkflowActors = workflowActorSlots.filter((slot) => !bindingForSlot(slot)?.id.trim())

  return (
    <>
      <button
        type="button"
        data-tour-task-create
        onClick={openDialog}
        className="rounded-lg border border-sky-600 bg-white px-3 py-2 text-sm font-medium text-sky-700 hover:bg-sky-50 dark:border-sky-500 dark:bg-zinc-900 dark:text-sky-400 dark:hover:bg-zinc-800"
      >
        {t('forms.createTask')}
      </button>
      {open ? (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 p-4"
          role="presentation"
          {...overlayDismissProps(() => !busy && setOpen(false))}
        >
          <div
            className="max-h-[min(90vh,760px)] w-full max-w-2xl overflow-y-auto rounded-xl border border-neutral-200 bg-white shadow-lg dark:border-zinc-700 dark:bg-zinc-900 animate-scale-in"
            onClick={(e) => e.stopPropagation()}
            role="dialog"
            aria-labelledby="create-task-title"
          >
            <div className="border-b border-neutral-200 px-4 py-3 dark:border-zinc-700">
              <h2 id="create-task-title" className="text-base font-semibold text-neutral-900 dark:text-zinc-100">
                {t('forms.createTask')}
              </h2>
            </div>
            <form onSubmit={onSubmit} className="space-y-3 px-4 py-3">
              {multiProject && (
                <label className="block text-sm">
                  <span className="text-neutral-600 dark:text-zinc-400">{t('workbench.filterProject')}</span>
                  <select value={selectedProject} onChange={(e) => onProjectChange(e.target.value)} className={fieldCls}>
                    {allProjectsAgents!.map((p) => <option key={p.projectId} value={p.projectId}>{p.projectId}</option>)}
                  </select>
                </label>
              )}

              {currentAgentActors.length === 0 && (
                <p className="text-sm text-amber-800 dark:text-amber-400">{t('forms.needAgentsForTask')}</p>
              )}

              <div className="grid grid-cols-2 gap-2 rounded-lg bg-neutral-100 p-1 dark:bg-zinc-800/70">
                {(['blank', 'template'] as const).map((mode) => (
                  <button
                    key={mode}
                    type="button"
                    onClick={() => onCreateModeChange(mode)}
                    className={cn(
                      'rounded-md px-3 py-1.5 text-sm font-medium transition-colors',
                      createMode === mode
                        ? 'bg-white text-neutral-900 shadow-sm dark:bg-zinc-950 dark:text-zinc-100'
                        : 'text-neutral-500 hover:text-neutral-800 dark:text-zinc-400 dark:hover:text-zinc-100',
                    )}
                  >
                    {mode === 'blank' ? t('taskTemplates.createBlank') : t('taskTemplates.createFromTemplate')}
                  </button>
                ))}
              </div>

              {createMode === 'template' ? (
                <div className="rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-zinc-700 dark:bg-zinc-950/40">
                  {templatesState.status === 'loading' ? (
                    <p className="text-sm text-neutral-500 dark:text-zinc-400">{t('api.loading')}</p>
                  ) : taskTemplates.length === 0 ? (
                    <p className="text-sm text-amber-700 dark:text-amber-400">{t('taskTemplates.emptyForCreate')}</p>
                  ) : (
                    <label className="block text-sm">
                      <span className="text-neutral-600 dark:text-zinc-400">{t('taskTemplates.selectTemplate')}</span>
                      <select value={taskTemplateId} onChange={(e) => { clearFieldError('template'); onTemplateChange(e.target.value) }} className={controlClass('template')}>
                        <option value="">{t('taskTemplates.none')}</option>
                        {taskTemplates.map((template) => <option key={template.id} value={template.id}>{template.name}</option>)}
                      </select>
                      {fieldError('template')}
                      {selectedTemplate?.description ? <p className="mt-0.5 text-xs text-neutral-400 dark:text-zinc-500">{selectedTemplate.description}</p> : null}
                    </label>
                  )}
                </div>
              ) : null}

              {createMode === 'template' && selectedTemplate && (selectedTemplate.variables?.length ?? 0) > 0 ? (
                <div className="rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-zinc-700 dark:bg-zinc-950/40">
                  <div className="text-sm font-medium text-neutral-700 dark:text-zinc-300">{t('taskTemplates.variables')}</div>
                  <div className="mt-2 grid gap-2">
                    {selectedTemplate.variables!.map((variable) => (
                      <label key={variable.name} className="block text-sm">
                        <span className="text-neutral-600 dark:text-zinc-400">
                          {variable.name}{variable.required ? ' *' : ''}
                        </span>
                        <input
                          value={templateInputs[variable.name] ?? ''}
                          onChange={(e) => {
                            clearFieldError(`variable:${variable.name}`)
                            setTemplateInputs((current) => ({ ...current, [variable.name]: e.target.value }))
                          }}
                          className={controlClass(`variable:${variable.name}`)}
                          placeholder={variable.description || variable.default || ''}
                        />
                        {fieldError(`variable:${variable.name}`)}
                      </label>
                    ))}
                  </div>
                </div>
              ) : null}

              <label className="block text-sm">
                <span className="text-neutral-600 dark:text-zinc-400">{t('forms.agent')}</span>
                <select value={agent} onChange={(e) => { clearFieldError('agent'); setAgent(e.target.value) }} className={controlClass('agent')} disabled={currentAgentActors.length === 0}>
                  {currentAgentActors.map((a) => <option key={a.name} value={a.name}>{a.name}</option>)}
                </select>
                {fieldError('agent')}
              </label>

              <label className="block text-sm">
                <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.colAssignee')}</span>
                <select value={assignee} onChange={(e) => setAssignee(e.target.value)} className={fieldCls}>
                  <option value="">{t('tasks.assignDefault')}</option>
                  {humanAssignees.map((person) => (
                    <option key={`user:${person.id}`} value={person.id}>{person.label}</option>
                  ))}
                  {allAgentAssignees.map((a) => (
                    <option key={`${a.projectId}/${a.name}`} value={`${a.projectId}/${a.name}`}>
                      {a.projectId}/{a.name}
                    </option>
                  ))}
                </select>
                <p className="mt-0.5 text-xs text-neutral-400 dark:text-zinc-500">{t('tasks.assignHint')}</p>
              </label>

              {createMode === 'template' && selectedTemplate ? (
                <div className="rounded-lg border border-neutral-200 bg-white p-3 dark:border-zinc-700 dark:bg-zinc-900">
                  <div className="text-sm font-medium text-neutral-700 dark:text-zinc-300">{t('taskTemplates.preview')}</div>
                  <div className="mt-2 space-y-2 text-sm">
                    <PreviewBlock label={t('forms.title')} value={renderTemplatePreview(selectedTemplate.titleTemplate)} />
                    {selectedTemplate.descriptionTemplate ? <PreviewBlock label={t('tasks.description')} value={renderTemplatePreview(selectedTemplate.descriptionTemplate)} /> : null}
                    <PreviewBlock label={t('forms.prompt')} value={renderTemplatePreview(selectedTemplate.promptTemplate)} multiline />
                  </div>
                </div>
              ) : (
                <>
                  <label className="block text-sm">
                    <span className="text-neutral-600 dark:text-zinc-400">{t('forms.title')}</span>
                    <input value={title} onChange={(e) => { clearFieldError('title'); setTitle(e.target.value) }} className={controlClass('title')} />
                    {fieldError('title')}
                  </label>

                  <label className="block text-sm">
                    <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.description')}</span>
                    <textarea value={description} onChange={(e) => setDescription(e.target.value)} rows={3} className={cn(fieldCls, 'resize-y')} placeholder={t('tasks.descriptionHint')} />
                  </label>

                  <label className="block text-sm">
                    <span className="text-neutral-600 dark:text-zinc-400">{t('forms.prompt')}</span>
                    <div className="relative">
                      <textarea
                        ref={promptInputRef}
                        value={prompt}
                        onChange={onPromptChange}
                        onKeyDown={(e) => {
                          if (!mention) return
                          if (e.key === 'ArrowDown' || e.key === 'ArrowUp' || e.key === 'Enter' || e.key === 'Escape') {
                            e.preventDefault()
                            mentionPopRef.current?.handleKey(e)
                          }
                        }}
                        onCompositionStart={() => { composingRef.current = true }}
                        onCompositionEnd={(e) => {
                          composingRef.current = false
                          // An IME commit can land text right after the "@";
                          // re-run the same trigger logic on the final value.
                          onPromptChange({ target: e.currentTarget } as React.ChangeEvent<HTMLTextAreaElement>)
                        }}
                        rows={8}
                        className={cn(controlClass('prompt'), 'resize-y')}
                        placeholder={t('tasks.assets.promptPlaceholder')}
                      />
                      {mention && (
                        <AssetMentionPopover
                          ref={mentionPopRef}
                          candidates={libraryAssets}
                          query={mention.query}
                          remaining={MAX_TASK_ASSETS - taskAssets.length}
                          style={{ left: mentionPos.left, top: mentionPos.top }}
                          onPick={stageBinding}
                          onClose={() => setMention(null)}
                        />
                      )}
                    </div>
                    <p className="mt-0.5 text-xs text-neutral-400 dark:text-zinc-500">{t('tasks.assets.mentionHint')}</p>
                    {fieldError('prompt')}
                  </label>
                </>
              )}

              <div className="block text-sm">
                <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.assets.label')}</span>
                {taskAssets.length > 0 && (
                  <div className="mt-1.5 space-y-2">
                    {([
                      ['upload', 'tasks.assets.uploadsGroup'],
                      ['library', 'tasks.assets.referencesGroup'],
                    ] as const).map(([source, labelKey]) => {
                      const group = taskAssets.filter((b) => b.source === source)
                      if (group.length === 0) return null
                      return (
                        <div key={source}>
                          <div className="text-xs text-neutral-400 dark:text-zinc-500">{t(labelKey)}</div>
                          <div className="mt-1 flex flex-wrap gap-1.5">
                            {group.map((a) => (
                              <span
                                key={a.fileId}
                                className="inline-flex items-center gap-1 rounded-md border border-neutral-200 bg-white px-2 py-1 text-xs text-neutral-700 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-300"
                              >
                                {a.source === 'library' ? (
                                  <FileText className="h-3 w-3 text-sky-500 dark:text-sky-400" />
                                ) : (
                                  <Paperclip className="h-3 w-3 text-neutral-400" />
                                )}
                                <span className="max-w-48 truncate">{a.displayName}</span>
                                <button
                                  type="button"
                                  title={t('tasks.assets.roleToggleHint')}
                                  onClick={() =>
                                    setTaskAssets((prev) =>
                                      prev.map((b) =>
                                        b.fileId === a.fileId
                                          ? { ...b, role: b.role === 'requirement_input' ? 'reference' : 'requirement_input' }
                                          : b,
                                      ),
                                    )
                                  }
                                  className={cn(
                                    'rounded px-1 text-[10px] font-medium',
                                    a.role === 'requirement_input'
                                      ? 'bg-sky-100 text-sky-700 dark:bg-sky-950 dark:text-sky-300'
                                      : 'bg-neutral-100 text-neutral-500 dark:bg-zinc-700 dark:text-zinc-400',
                                  )}
                                >
                                  {t(`tasks.assets.role.${a.role}`)}
                                </button>
                                <button
                                  type="button"
                                  title={t('tasks.assets.requiredToggleHint')}
                                  aria-pressed={a.required}
                                  onClick={() =>
                                    setTaskAssets((prev) =>
                                      prev.map((b) => (b.fileId === a.fileId ? { ...b, required: !b.required } : b)),
                                    )
                                  }
                                  className={cn(
                                    'rounded px-1 text-[10px] font-medium',
                                    a.required
                                      ? 'bg-amber-100 text-amber-700 dark:bg-amber-950 dark:text-amber-300'
                                      : 'bg-neutral-100 text-neutral-400 line-through dark:bg-zinc-700 dark:text-zinc-500',
                                  )}
                                >
                                  {t('tasks.assets.requiredBadge')}
                                </button>
                                <button
                                  type="button"
                                  aria-label={t('tasks.assets.remove')}
                                  onClick={() => removeBinding(a.fileId)}
                                  className="text-neutral-400 hover:text-neutral-700 dark:hover:text-zinc-200"
                                >
                                  <X className="h-3 w-3" />
                                </button>
                              </span>
                            ))}
                          </div>
                        </div>
                      )
                    })}
                  </div>
                )}
                <div className="mt-1.5 flex items-center gap-3">
                  <button
                    type="button"
                    onClick={() => assetInputRef.current?.click()}
                    disabled={assetsBusy}
                    className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-sm font-medium text-neutral-700 shadow-sm hover:bg-neutral-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200 dark:hover:bg-zinc-700"
                  >
                    <Paperclip className="h-4 w-4" />
                    {assetsBusy ? t('tasks.assets.uploading') : t('tasks.assets.upload')}
                  </button>
                  <span className="text-xs text-neutral-400 dark:text-zinc-500">{t('tasks.assets.hint')}</span>
                </div>
                <input
                  ref={assetInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => void onAssetsSelected(e.target.files)}
                />
              </div>

              {createMode === 'blank' ? (
                <div className="grid grid-cols-2 gap-3">
                  <label className="block text-sm">
                    <span className="text-neutral-600 dark:text-zinc-400">{t('forms.type')}</span>
                    <select value={taskType} onChange={(e) => setTaskType(e.target.value)} className={fieldCls}>
                      {TASK_TYPES.map((ty) => <option key={ty} value={ty}>{t(`forms.taskType.${ty}`, { defaultValue: ty })}</option>)}
                    </select>
                  </label>
                  <label className="block text-sm">
                    <span className="text-neutral-600 dark:text-zinc-400">{t('forms.priority')}</span>
                    <select value={priority} onChange={(e) => setPriority(Number(e.target.value))} className={fieldCls}>
                      {[0, 1, 2, 3].map((p) => <option key={p} value={p}>P{p} — {t(`forms.priorityLabel.${p}`)}</option>)}
                    </select>
                  </label>
                </div>
              ) : null}

              <div className="grid grid-cols-2 gap-3">
                <label className="block text-sm">
                  <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.dueDate')}</span>
                  <input type="date" value={dueDate} onChange={(e) => setDueDate(e.target.value)} className={fieldCls} />
                </label>
                <label className="block text-sm">
                  <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.estimateDuration')}</span>
                  <input value={estimateDuration} onChange={(e) => setEstimateDuration(e.target.value)} placeholder="30m" className={fieldCls} />
                </label>
              </div>

              <div className="grid grid-cols-2 gap-3">
                <label className="block text-sm">
                  <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.parentTask')}</span>
                  {parentChoices.length > 0 ? (
                    <select value={parentId} onChange={(e) => setParentId(e.target.value)} className={fieldCls}>
                      <option value="">{t('tasks.parentTaskNone')}</option>
                      {parentChoices.map((o) => (
                        <option key={o.id} value={o.id}>{o.title}</option>
                      ))}
                    </select>
                  ) : (
                    <input value={parentId} onChange={(e) => setParentId(e.target.value)} placeholder="t-..." className={cn(fieldCls, 'font-mono text-xs')} />
                  )}
                </label>
                <label className="block text-sm">
                  <span className="text-neutral-600 dark:text-zinc-400">{t('tasks.labels')}</span>
                  <input value={labelsStr} onChange={(e) => setLabelsStr(e.target.value)} placeholder={t('tasks.labelsHint')} className={fieldCls} />
                </label>
              </div>

              {/* Git Branch & Worktree Isolation Selector */}
              <div className="rounded-lg border border-sky-100 bg-sky-50/40 p-3 dark:border-sky-950/60 dark:bg-sky-950/20">
                <div className="flex items-center justify-between">
                  <span className="text-xs font-semibold text-sky-900 dark:text-sky-300">
                    {t('tasks.branchIsolationTitle', { defaultValue: '🌿 Git 分支隔离与独立 Worktree' })}
                  </span>
                  <span className="text-[11px] text-neutral-400 dark:text-zinc-500">
                    {t('tasks.branchIsolationDesc', { defaultValue: '自动隔离开发环境，多任务互不冲突' })}
                  </span>
                </div>
                <div className="mt-2 flex items-center gap-2">
                  <div className="flex-1">
                    <div className="flex items-center justify-between">
                      <label className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">{t('tasks.baseBranchLabel', { defaultValue: 'Base 分支 (from)' })}</label>
                      <span className="text-[10px] text-sky-600 dark:text-sky-400">{t('tasks.branchSearchable', { defaultValue: '可搜索/选择' })}</span>
                    </div>
                    <BranchCombobox
                      value={baseBranch}
                      onChange={setBaseBranch}
                      branches={availableBranches}
                      branchInfos={branchInfos}
                      onRefresh={refreshBranches}
                      refreshing={branchRefreshing}
                      fieldCls={fieldCls}
                    />
                  </div>
                  <div className="mt-4 shrink-0 text-sky-600 dark:text-sky-400 font-bold select-none">
                    ──►
                  </div>
                  <div className="flex-1">
                    <label className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">{t('tasks.featureBranchLabel', { defaultValue: '特性分支 (to)' })}</label>
                    <input
                      value={branchName}
                      onChange={(e) => setBranchName(e.target.value)}
                      placeholder={t('tasks.featureBranchPlaceholder', { defaultValue: 'feature/task-name (留空自动生成)' })}
                      className={cn(fieldCls, 'mt-0.5 font-mono text-xs')}
                    />
                  </div>
                </div>
              </div>

              {createMode === 'blank' ? (
                <label className="block text-sm">
                  <span className="text-neutral-600 dark:text-zinc-400">{t('workflows.taskWorkflow')}</span>
                  <select value={workflowDefinitionId} onChange={(e) => onWorkflowChange(e.target.value)} className={fieldCls}>
                    <option value="">{t('workflows.noWorkflow')}</option>
                    {workflows.filter((wf) => wf.id !== 'project-initialization-v1').map((wf) => <option key={wf.id} value={wf.id}>{wf.name}</option>)}
                  </select>
                  <p className="mt-0.5 text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.taskWorkflowHint')}</p>
                </label>
              ) : null}

              {selectedWorkflow ? (
                <DeliveryModeNotice
                  steps={selectedWorkflow.steps}
                  project={projectRemoteState.status === 'ok' ? projectRemoteState.data : undefined}
                  workflows={workflows}
                  onSelectWorkflow={onWorkflowChange}
                />
              ) : null}

              {workflowDefinitionId && workflowActorSlots.length > 0 ? (
                <div className="rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-zinc-700 dark:bg-zinc-950/40">
                  <div className="flex items-center justify-between gap-2">
                    <span className="text-sm font-medium text-neutral-700 dark:text-zinc-300">{t('workflows.actorBindings')}</span>
                    <span className={cn('text-xs', missingWorkflowActors.length === 0 ? 'text-emerald-600 dark:text-emerald-400' : 'text-amber-600 dark:text-amber-400')}>
                      {t('workflows.actorBindingsProgress', { done: workflowActorSlots.length - missingWorkflowActors.length, total: workflowActorSlots.length })}
                    </span>
                  </div>
                  <div className="mt-3 space-y-2">
                    {workflowActorSlots.map((slot) => {
                      const binding = bindingForSlot(slot) ?? { type: slot.preferredType, id: '' }
                      const options = binding.type === 'agent' ? currentAgentActors.map((a) => ({ id: a.name, label: a.name })) : humanAssignees
                      return (
                        <div
                          key={slot.key}
                          className={cn(
                            'rounded-md border bg-white p-2 dark:bg-zinc-900',
                            fieldErrors[`actor:${slot.key}`]
                              ? 'border-red-400 dark:border-red-500/70'
                              : 'border-neutral-200 dark:border-zinc-700',
                          )}
                        >
                          <div className="flex items-start justify-between gap-2">
                            <div className="min-w-0">
                              <p className="truncate text-sm font-medium text-neutral-800 dark:text-zinc-200">{slot.role}</p>
                              <p className="mt-0.5 line-clamp-1 text-xs text-neutral-400 dark:text-zinc-500">{slot.titles.join('、')}</p>
                            </div>
                          </div>
                          <div className="mt-2 grid grid-cols-[96px_minmax(0,1fr)] gap-2">
                            <select value={binding.type} onChange={(e) => updateActorBinding(slot.key, { type: e.target.value as 'agent' | 'human' })} className={fieldCls}>
                              <option value="agent">{t('workflows.actorTypeAgent')}</option>
                              <option value="human">{t('workflows.actorTypeHuman')}</option>
                            </select>
                            <select value={binding.id} onChange={(e) => updateActorBinding(slot.key, { id: e.target.value })} className={fieldCls}>
                              <option value="">{t('workflows.selectActor')}</option>
                              {options.map((option) => (
                                <option key={option.id} value={option.id}>{option.label}</option>
                              ))}
                            </select>
                          </div>
                          {fieldError(`actor:${slot.key}`)}
                        </div>
                      )
                    })}
                  </div>
                </div>
              ) : null}

              {err && <p className="text-sm text-red-600 dark:text-red-400">{err}</p>}
              <div className="flex justify-end gap-2 pt-1">
                <button type="button" onClick={() => setOpen(false)} disabled={busy} className="rounded-lg border border-neutral-300 px-3 py-1.5 text-sm dark:border-zinc-600">{t('forms.cancel')}</button>
                <button type="submit" disabled={busy || currentAgentActors.length === 0} className="rounded-lg bg-sky-600 px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50">{busy ? t('forms.saving') : t('forms.submit')}</button>
              </div>
            </form>
          </div>
        </div>
      ) : null}
    </>
  )
}

function PreviewBlock({ label, value, multiline }: { label: string; value: string; multiline?: boolean }) {
  return (
    <div>
      <div className="text-xs font-medium text-neutral-400 dark:text-zinc-500">{label}</div>
      <div className={cn(
        'mt-1 rounded-md bg-neutral-50 px-2.5 py-2 text-neutral-700 dark:bg-zinc-950/60 dark:text-zinc-300',
        multiline ? 'max-h-36 overflow-y-auto whitespace-pre-wrap' : 'truncate',
      )}>
        {value || '—'}
      </div>
    </div>
  )
}

type BranchInfo = { name: string; sha?: string; lastCommitDate?: string }
type BranchesResponse = { branches: string[]; branchInfos?: BranchInfo[]; refreshed?: boolean; refreshErr?: string }

function BranchCombobox({
  value,
  onChange,
  branches,
  branchInfos,
  onRefresh,
  refreshing,
  fieldCls,
}: {
  value: string
  onChange: (val: string) => void
  branches: string[]
  branchInfos: BranchInfo[]
  onRefresh: () => void
  refreshing: boolean
  fieldCls: string
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [])

  const infoByName = useMemo(() => {
    const map = new Map<string, BranchInfo>()
    for (const info of branchInfos) map.set(info.name, info)
    return map
  }, [branchInfos])

  const filtered = useMemo(() => {
    const q = value.toLowerCase().trim()
    if (!q) return branches
    return branches.filter((b) => b.toLowerCase().includes(q))
  }, [branches, value])

  return (
    <div ref={containerRef} className="relative">
      <div className="relative flex items-center">
        <GitBranch className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 size-3.5 text-neutral-400 dark:text-zinc-500" />
        <input
          value={value}
          onChange={(e) => {
            onChange(e.target.value)
            setOpen(true)
          }}
          onFocus={() => setOpen(true)}
          placeholder="main"
          className={cn(fieldCls, 'mt-0.5 pl-8 pr-7 font-mono text-xs')}
        />
        <button
          type="button"
          tabIndex={-1}
          onClick={() => setOpen(!open)}
          className="absolute right-2 top-1/2 -translate-y-1/2 p-0.5 text-neutral-400 hover:text-neutral-600 dark:text-zinc-500 dark:hover:text-zinc-300"
        >
          <ChevronDown className={cn('size-3.5 transition-transform duration-150', open && 'rotate-180')} />
        </button>
      </div>

      {open && (
        <div className="absolute left-0 top-full z-50 mt-1 max-h-52 w-full min-w-[200px] overflow-y-auto rounded-lg border border-neutral-200 bg-white py-1 shadow-xl dark:border-zinc-700 dark:bg-zinc-900 animate-scale-in">
          <div className="flex items-center justify-between px-2.5 py-1">
            <span className="text-[10px] font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('tasks.availableBranches', { defaultValue: '可用分支 (Git Branches)' })}
            </span>
            <button
              type="button"
              onClick={() => onRefresh()}
              disabled={refreshing}
              title={t('tasks.syncRemoteBranches', { defaultValue: '从远程同步分支列表' })}
              className="inline-flex items-center gap-1 rounded px-1 py-0.5 text-[10px] font-medium text-sky-600 transition hover:bg-sky-50 disabled:opacity-50 dark:text-sky-400 dark:hover:bg-sky-950/60"
            >
              <RefreshCw className={cn('size-3', refreshing && 'animate-spin')} />
              {refreshing ? t('tasks.syncing', { defaultValue: '同步中…' }) : t('tasks.sync', { defaultValue: '同步' })}
            </button>
          </div>
          {filtered.length === 0 ? (
            <div className="px-3 py-2 text-xs text-neutral-400 dark:text-zinc-500">
              {t('tasks.noBranchMatch', { value, defaultValue: '未找到匹配分支，将使用 "{{value}}"' })}
            </div>
          ) : (
            filtered.map((b) => {
              const isSelected = value === b
              const info = infoByName.get(b)
              return (
                <button
                  key={b}
                  type="button"
                  onClick={() => {
                    onChange(b)
                    setOpen(false)
                  }}
                  className={cn(
                    'flex w-full items-center justify-between px-3 py-1.5 text-left text-xs font-mono transition-colors',
                    isSelected
                      ? 'bg-sky-50 font-semibold text-sky-700 dark:bg-sky-950/60 dark:text-sky-300'
                      : 'text-neutral-700 hover:bg-neutral-100 dark:text-zinc-300 dark:hover:bg-zinc-800',
                  )}
                >
                  <span className="flex items-center gap-1.5 truncate">
                    <GitBranch className="size-3 shrink-0 text-neutral-400 dark:text-zinc-500" />
                    <span className="truncate">{b}</span>
                    {info?.sha && <span className="shrink-0 text-[10px] text-neutral-400 dark:text-zinc-500">{info.sha}</span>}
                    {info?.lastCommitDate && (
                      <span className="shrink-0 text-[10px] font-sans text-neutral-400 dark:text-zinc-500">· {info.lastCommitDate}</span>
                    )}
                  </span>
                  {isSelected && <Check className="size-3.5 shrink-0 text-sky-600 dark:text-sky-400" />}
                </button>
              )
            })
          )}
        </div>
      )}
    </div>
  )
}
