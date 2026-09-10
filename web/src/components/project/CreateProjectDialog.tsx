import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertCircle,
  Bot,
  Check,
  ChevronDown,
  ChevronUp,
  FolderKanban,
  Globe,
  Info,
  Loader2,
  Lock,
  MessageSquare,
  Users,
  X,
} from 'lucide-react'
import { apiFetch, apiPost } from '../../lib/api'
import { useAuth } from '../../lib/auth'
import { overlayDismissProps } from '../ui/overlay'
import { showToast } from '../ui/Toast'

type AgentWorker = {
  id: string
  name: string
  description?: string
  status?: string
}

type WorkspaceUser = {
  username: string
  displayName?: string
  role?: string
  email?: string
  projects?: Array<{ project: string; role: string }>
  imBound?: boolean
  imProviders?: string[]
}

type IMInstance = {
  id: string
  provider: string
  displayName?: string
  display_name?: string
  attestation?: string
}

type UserIMConnection = {
  id: string
  name: string
  provider: string
  providerLabel: string
  baseUrl?: string
  instanceId?: string
  instanceName?: string
  bound: boolean
  sharedBinding?: boolean
  externalUsername?: string
  externalUserId?: string
}

type ProjectMemberRole = 'viewer' | 'operator' | 'manager'

export function CreateProjectDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void
  onCreated: (name: string) => void
}) {
  const { t } = useTranslation()

  // Basic info state
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [saving, setSaving] = useState(false)
  const [saveStep, setSaveStep] = useState<string>('')
  const [error, setError] = useState<string | null>(null)

  // Agent Team state
  const [teamExpanded, setTeamExpanded] = useState(false)
  const [workers, setWorkers] = useState<AgentWorker[]>([])
  const [selectedWorkerIds, setSelectedWorkerIds] = useState<string[]>([])

  // Project Members state
  const { user: authUser } = useAuth()
  const currentUsername = authUser?.username || 'admin'
  const [membersExpanded, setMembersExpanded] = useState(false)
  const [users, setUsers] = useState<WorkspaceUser[]>([])
  const [selectedUsernames, setSelectedUsernames] = useState<string[]>([currentUsername])
  const [memberRoles, setMemberRoles] = useState<Record<string, ProjectMemberRole>>({
    [currentUsername]: 'manager',
  })

  // ChatOps Channel state
  const [chatopsEnabled, setChatopsEnabled] = useState(true)
  const [chatopsExpanded, setChatopsExpanded] = useState(false)
  const [channelMode, setChannelMode] = useState<'create' | 'link'>('create')
  const [channelVisibility, setChannelVisibility] = useState<'private' | 'public'>('private')
  const [customChannelName, setCustomChannelName] = useState('')
  const [imInstances, setImInstances] = useState<IMInstance[]>([])
  const [selectedInstanceId, setSelectedInstanceId] = useState<string>('')
  const [imConnections, setImConnections] = useState<UserIMConnection[]>([])

  // Fetch initial data: agents, users, IM instances, connections
  useEffect(() => {
    async function loadData() {
      // 1. Load Agent Workers
      try {
        const workersRes = await apiFetch<{ agents?: AgentWorker[] }>('/api/v1/agents', {
          suppressToast: true,
          silentStatuses: [403],
        })
        const list = workersRes?.agents ?? []
        setWorkers(list)

        // Preselect Mira and Lina by default, or all available if none match
        const defaultPreselected = list
          .filter((w) => {
            const lower = w.name.toLowerCase()
            return lower.includes('mira') || lower.includes('lina')
          })
          .map((w) => w.id)

        if (defaultPreselected.length > 0) {
          setSelectedWorkerIds(defaultPreselected)
        } else if (list.length > 0) {
          setSelectedWorkerIds(list.slice(0, 2).map((w) => w.id))
        }
      } catch {
        // Fallback default workers
      }

      // 2. Load Workspace Users
      try {
        const usersRes = await apiFetch<WorkspaceUser[]>('/api/v1/users', {
          suppressToast: true,
          silentStatuses: [403],
        })
        const list = usersRes ?? []
        setUsers(list)
        if (!list.some((u) => u.username === currentUsername)) {
          setSelectedUsernames([currentUsername])
        }
      } catch {
        // Fallback to current user if non-admin or failed
        setUsers([{ username: currentUsername, displayName: authUser?.displayName || currentUsername }])
        setSelectedUsernames([currentUsername])
      }

      // 3. Load IM Instances & Connections
      try {
        const instancesRes = await apiFetch<{ instances?: IMInstance[] }>('/api/v1/im/instances', {
          suppressToast: true,
          silentStatuses: [403],
        })
        const instList = instancesRes?.instances ?? []
        setImInstances(instList)
        if (instList.length > 0) {
          setSelectedInstanceId(instList[0].id)
        }

        const identsRes = await apiFetch<{ connections?: UserIMConnection[] }>('/api/v1/user/im-identities', {
          suppressToast: true,
          silentStatuses: [403],
        })
        const conns = identsRes?.connections ?? []
        setImConnections(conns)

        // If no connections or instances available, disable chatops by default
        if (instList.length === 0 && conns.length === 0) {
          setChatopsEnabled(false)
        }
      } catch {
        setChatopsEnabled(false)
      }
    }

    void loadData()
  }, [currentUsername, authUser?.displayName])

  // Effective Channel Name
  const effectiveChannelName = useMemo(() => {
    if (customChannelName.trim()) {
      let c = customChannelName.trim()
      if (!c.startsWith('#')) c = `#${c}`
      return c
    }
    const baseKey = name.trim() || 'project'
    return `#proj-${baseKey.toLowerCase()}`
  }, [customChannelName, name])

  // Preselected Agent Team Summary Text
  const agentTeamSummary = useMemo(() => {
    if (selectedWorkerIds.length === 0) {
      return t('projects.teamNoneSelected', { defaultValue: '未选定智能体 (可后续在项目中添加)' })
    }
    const names = workers
      .filter((w) => selectedWorkerIds.includes(w.id))
      .map((w) => w.name)
    return names.join('、')
  }, [selectedWorkerIds, workers, t])

  // Project Members Summary Text
  const membersSummary = useMemo(() => {
    const count = selectedUsernames.length
    if (count <= 1) {
      return t('projects.membersCreatorOnly', {
        defaultValue: `当前创建者 (${currentUsername}) 将默认加入`,
        name: currentUsername,
      })
    }
    return t('projects.membersWithOthers', {
      defaultValue: `包含创建者 (${currentUsername}) 等 ${count} 位成员`,
      name: currentUsername,
      count,
    })
  }, [selectedUsernames, currentUsername, t])

  // Active IM Instance display
  const activeInstance = useMemo(() => {
    if (selectedInstanceId) {
      const found = imInstances.find((i) => i.id === selectedInstanceId)
      if (found) return found.displayName || found.display_name || found.provider || 'Mattermost'
    }
    if (imConnections.length > 0) {
      return imConnections[0].instanceName || imConnections[0].name || 'Mattermost'
    }
    return 'Mattermost'
  }, [selectedInstanceId, imInstances, imConnections])

  // Active IM Instance provider
  const activeProvider = useMemo(() => {
    if (selectedInstanceId) {
      const found = imInstances.find((i) => i.id === selectedInstanceId)
      if (found?.provider) return found.provider.toLowerCase()
    }
    if (imConnections.length > 0 && imConnections[0].provider) {
      return imConnections[0].provider.toLowerCase()
    }
    return 'mattermost'
  }, [selectedInstanceId, imInstances, imConnections])

  // Member binding preview in channel
  const memberChannelPreview = useMemo(() => {
    const boundExternalUsernames = new Set<string>()
    for (const c of imConnections) {
      if (c.bound || c.sharedBinding) {
        if (c.externalUsername) boundExternalUsernames.add(c.externalUsername)
      }
    }

    return selectedUsernames.map((u) => {
      const userInfo = users.find((usr) => usr.username === u)
      let isBound = false
      if (userInfo?.imProviders && userInfo.imProviders.length > 0) {
        isBound = userInfo.imProviders.some((p) => p.toLowerCase() === activeProvider)
      } else if (userInfo?.imBound !== undefined) {
        isBound = userInfo.imBound
      } else if (u === currentUsername) {
        isBound =
          boundExternalUsernames.size > 0 ||
          imConnections.some(
            (c) => (c.bound || c.sharedBinding) && (!activeProvider || c.provider.toLowerCase() === activeProvider)
          )
      }
      return {
        username: u,
        isBound,
      }
    })
  }, [selectedUsernames, users, currentUsername, imConnections, activeProvider])

  // Toggle worker selection
  function toggleWorker(workerId: string) {
    setSelectedWorkerIds((prev) =>
      prev.includes(workerId) ? prev.filter((id) => id !== workerId) : [...prev, workerId]
    )
  }

  // Toggle user selection
  function toggleUser(username: string) {
    if (username === currentUsername) return // creator always included
    setSelectedUsernames((prev) => {
      if (prev.includes(username)) {
        return prev.filter((u) => u !== username)
      } else {
        if (!memberRoles[username]) {
          setMemberRoles((roles) => ({ ...roles, [username]: 'operator' }))
        }
        return [...prev, username]
      }
    })
  }

  function changeMemberRole(username: string, role: ProjectMemberRole) {
    setMemberRoles((prev) => ({ ...prev, [username]: role }))
  }

  // Create Project Workflow
  async function handleCreate() {
    const projectName = name.trim()
    if (!projectName) {
      setError(t('forms.fillRequired', { defaultValue: '请填写项目标识 / 代码' }))
      return
    }

    const nameRegex = /^[a-zA-Z0-9-_.]+$/
    if (!nameRegex.test(projectName) || projectName.startsWith('.') || projectName.includes('..')) {
      setError('项目标识仅支持英文字母、数字、短横线(-)、下划线(_)与点(.)，不可包含中文或空格，且不能以点开头')
      return
    }
    if (projectName.length > 80) {
      setError('项目标识长度不能超过 80 个字符')
      return
    }

    setError(null)
    setSaving(true)
    setSaveStep(t('projects.creatingProject', { defaultValue: '正在创建项目及配置资源...' }))

    try {
      const cleanChan = effectiveChannelName ? effectiveChannelName.replace(/^#/, '') : ''
      const channelPayload = (chatopsEnabled && cleanChan) ? {
        provider: 'mattermost',
        mode: channelMode,
        instanceId: selectedInstanceId || undefined,
        channelName: cleanChan,
        displayName: effectiveChannelName,
        visibility: channelVisibility,
        workerIds: selectedWorkerIds,
        memberUsernames: selectedUsernames,
      } : undefined

      const membersPayload = selectedUsernames.map((u) => ({
        username: u,
        role: u === currentUsername ? 'manager' : (memberRoles[u] || 'operator'),
      }))

      const payload = {
        name: projectName,
        description: description.trim(),
        workerIds: selectedWorkerIds,
        members: membersPayload,
        channel: channelPayload,
      }

      const res = await apiPost<{
        ok: boolean
        project: string
        channel?: {
          status?: string
          warning?: string
          failedMembers?: string[]
          failedAgents?: string[]
          failedBots?: string[]
        }
      }>('/api/v1/projects', payload)

      if (res?.channel?.status === 'partial' && res.channel.warning) {
        showToast(res.channel.warning, 'info')
      }

      onCreated(projectName)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
      setSaveStep('')
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/45 p-4 pt-[6vh] sm:pt-[8vh]"
      {...overlayDismissProps(onClose)}
    >
      <div
        className="relative flex max-h-[88vh] w-full max-w-xl flex-col overflow-hidden rounded-2xl border border-neutral-200/90 bg-white shadow-2xl transition-all dark:border-zinc-700/80 dark:bg-zinc-900"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex shrink-0 items-center justify-between border-b border-neutral-200/70 bg-neutral-50/50 px-6 py-4 dark:border-zinc-800 dark:bg-zinc-800/30">
          <div className="flex items-center gap-3">
            <div className="flex size-9 items-center justify-center rounded-xl bg-sky-50 text-sky-600 ring-1 ring-sky-100 dark:bg-sky-950/40 dark:text-sky-400 dark:ring-sky-900/50">
              <FolderKanban className="size-5" />
            </div>
            <div>
              <h2 className="text-base font-semibold text-neutral-900 dark:text-zinc-100">
                {t('projects.createTitle', { defaultValue: '创建新项目' })}
              </h2>
              <p className="text-xs text-neutral-500 dark:text-zinc-400">
                {t('projects.createSubtitle', { defaultValue: '定义业务项目，配置 Agent Team 与专属协作频道' })}
              </p>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-lg p-1.5 text-neutral-400 transition hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
          >
            <X className="size-4" />
          </button>
        </div>

        {/* Form Body */}
        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-5">
          {error && (
            <div className="flex items-center gap-2 rounded-lg bg-red-50 p-3 text-xs text-red-600 dark:bg-red-950/30 dark:text-red-300">
              <AlertCircle className="size-4 shrink-0" />
              <span>{error}</span>
            </div>
          )}

          {/* Section 1: Project Basics */}
          <div className="space-y-3">
            <div>
              <div className="flex items-center justify-between">
                <label className="block text-xs font-medium text-neutral-700 dark:text-zinc-300">
                  {t('projects.key', { defaultValue: '项目唯一标识 / 代码 (Key)' })} <span className="text-red-500">*</span>
                </label>
                <span className="text-[11px] text-neutral-400 dark:text-zinc-500">
                  仅支持英文、数字、-、_、. (最多 80 字符)
                </span>
              </div>
              <input
                type="text"
                autoFocus
                value={name}
                onChange={(e) => {
                  setName(e.target.value)
                  if (error) setError(null)
                }}
                placeholder="例如：pilot-change-request 或 orderhub"
                className="mt-1 w-full rounded-lg border border-neutral-200 bg-white px-3.5 py-2 font-mono text-sm text-neutral-900 outline-none transition placeholder:text-neutral-400 placeholder:font-sans focus:border-sky-500 focus:ring-2 focus:ring-sky-500/15 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
              />
              {name.trim() && !/^[a-zA-Z0-9-_.]+$/.test(name.trim()) && (
                <p className="mt-1 text-[11px] text-amber-600 dark:text-amber-400">
                  项目标识包含非法字符。请仅使用英文字母、数字、短横线(-)、下划线(_)或点(.)，不可包含中文或空格。
                </p>
              )}
            </div>

            <div>
              <label className="block text-xs font-medium text-neutral-700 dark:text-zinc-300">
                {t('projects.description', { defaultValue: '项目名称 / 描述 (Description)' })}
              </label>
              <input
                type="text"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="例如：变更申请台账 MVP，支持端到端审批与工单协同"
                className="mt-1 w-full rounded-lg border border-neutral-200 bg-white px-3.5 py-2 text-sm text-neutral-900 outline-none transition placeholder:text-neutral-400 focus:border-sky-500 focus:ring-2 focus:ring-sky-500/15 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
              />
              <p className="mt-1 text-[11px] text-neutral-400 dark:text-zinc-500">
                可填写中文业务名称或目标说明，将在项目列表及卡片中展示
              </p>
            </div>
          </div>

          <div className="h-px bg-neutral-100 dark:bg-zinc-800" />

          {/* Section 2: Agent Team */}
          <div className="rounded-xl border border-neutral-200/80 bg-neutral-50/40 transition dark:border-zinc-800 dark:bg-zinc-800/20">
            <div className="flex items-center justify-between p-3.5">
              <div className="flex items-center gap-2.5 min-w-0">
                <div className="flex size-7 shrink-0 items-center justify-center rounded-md bg-indigo-50 text-indigo-600 dark:bg-indigo-950/40 dark:text-indigo-400">
                  <Bot className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-semibold text-neutral-900 dark:text-zinc-100">
                      Agent Team
                    </span>
                    <span className="rounded bg-indigo-50 px-1.5 py-0.5 text-[10px] font-medium text-indigo-700 dark:bg-indigo-950/50 dark:text-indigo-300">
                      {selectedWorkerIds.length} 位预设
                    </span>
                  </div>
                  <p className="truncate text-[11px] text-neutral-500 dark:text-zinc-400">
                    {agentTeamSummary}
                  </p>
                </div>
              </div>
              <button
                type="button"
                onClick={() => setTeamExpanded((prev) => !prev)}
                className="inline-flex shrink-0 whitespace-nowrap items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium text-neutral-600 hover:bg-neutral-200/50 dark:text-zinc-400 dark:hover:bg-zinc-700/50"
              >
                {teamExpanded ? (
                  <>
                    {t('common.collapse', { defaultValue: '收起' })} <ChevronUp className="size-3.5" />
                  </>
                ) : (
                  <>
                    {t('projects.adjustTeam', { defaultValue: '调整团队' })}{' '}
                    <ChevronDown className="size-3.5" />
                  </>
                )}
              </button>
            </div>

            {teamExpanded && (
              <div className="border-t border-neutral-200/60 p-3.5 pt-2.5 dark:border-zinc-800">
                <p className="mb-2 text-[11px] text-neutral-500 dark:text-zinc-400">
                  {t('projects.agentTeamHint', {
                    defaultValue: '选择派驻到该项目的智能体。系统将自动分配开发与初审等标准角色：',
                  })}
                </p>
                <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                  {workers.map((w) => {
                    const isSelected = selectedWorkerIds.includes(w.id)
                    return (
                      <div
                        key={w.id}
                        onClick={() => toggleWorker(w.id)}
                        className={`flex cursor-pointer items-center justify-between rounded-lg border p-2.5 transition ${
                          isSelected
                            ? 'border-indigo-300 bg-white shadow-xs dark:border-indigo-800/80 dark:bg-zinc-900'
                            : 'border-neutral-200 bg-neutral-50/50 hover:bg-white dark:border-zinc-800 dark:bg-zinc-800/40 dark:hover:bg-zinc-800'
                        }`}
                      >
                        <div className="flex items-center gap-2 min-w-0">
                          <input
                            type="checkbox"
                            checked={isSelected}
                            onChange={() => toggleWorker(w.id)}
                            className="size-3.5 rounded border-neutral-300 text-indigo-600 focus:ring-indigo-500/20"
                          />
                          <div className="min-w-0">
                            <span className="truncate text-xs font-semibold text-neutral-900 dark:text-zinc-100">
                              {w.name}
                            </span>
                            {w.description && (
                              <p className="truncate text-[10px] text-neutral-400 dark:text-zinc-500">
                                {w.description}
                              </p>
                            )}
                          </div>
                        </div>
                        <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] font-mono text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                          {w.name.toLowerCase().includes('mira')
                            ? 'Developer'
                            : w.name.toLowerCase().includes('lina')
                              ? 'Reviewer'
                              : 'Agent'}
                        </span>
                      </div>
                    )
                  })}
                </div>
              </div>
            )}
          </div>

          {/* Section 3: Project Members */}
          <div className="rounded-xl border border-neutral-200/80 bg-neutral-50/40 transition dark:border-zinc-800 dark:bg-zinc-800/20">
            <div className="flex items-center justify-between p-3.5">
              <div className="flex items-center gap-2.5 min-w-0">
                <div className="flex size-7 shrink-0 items-center justify-center rounded-md bg-emerald-50 text-emerald-600 dark:bg-emerald-950/40 dark:text-emerald-400">
                  <Users className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-semibold text-neutral-900 dark:text-zinc-100">
                      {t('projects.membersTitle', { defaultValue: '项目成员' })}
                    </span>
                    <span className="rounded bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 dark:bg-emerald-950/50 dark:text-emerald-300">
                      {selectedUsernames.length} 人
                    </span>
                  </div>
                  <p className="truncate text-[11px] text-neutral-500 dark:text-zinc-400">
                    {membersSummary}
                  </p>
                </div>
              </div>
              <button
                type="button"
                onClick={() => setMembersExpanded((prev) => !prev)}
                className="inline-flex shrink-0 whitespace-nowrap items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium text-neutral-600 hover:bg-neutral-200/50 dark:text-zinc-400 dark:hover:bg-zinc-700/50"
              >
                {membersExpanded ? (
                  <>
                    {t('common.collapse', { defaultValue: '收起' })} <ChevronUp className="size-3.5" />
                  </>
                ) : (
                  <>
                    {t('projects.addMembers', { defaultValue: '添加成员' })}{' '}
                    <ChevronDown className="size-3.5" />
                  </>
                )}
              </button>
            </div>

            {membersExpanded && (
              <div className="border-t border-neutral-200/60 p-3.5 pt-2.5 dark:border-zinc-800">
                <p className="mb-2 text-[11px] text-neutral-500 dark:text-zinc-400">
                  {t('projects.membersHint', {
                    defaultValue: '勾选授权访问本工作流项目的团队同事（创建者默认拥有管理权限）：',
                  })}
                </p>
                <div className="space-y-1.5 max-h-36 overflow-y-auto pr-1">
                  {users.map((u) => {
                    const isCreator = u.username === currentUsername
                    const isSelected = selectedUsernames.includes(u.username)
                    return (
                      <div
                        key={u.username}
                        onClick={() => !isCreator && toggleUser(u.username)}
                        className={`flex items-center justify-between rounded-lg border p-2 text-xs transition ${
                          isSelected
                            ? 'border-emerald-300 bg-white dark:border-emerald-900/60 dark:bg-zinc-900'
                            : 'border-neutral-200 bg-neutral-50/50 hover:bg-white dark:border-zinc-800 dark:bg-zinc-800/40 dark:hover:bg-zinc-800'
                        } ${isCreator ? 'cursor-default opacity-85' : 'cursor-pointer'}`}
                      >
                        <div className="flex items-center gap-2 min-w-0">
                          <input
                            type="checkbox"
                            checked={isSelected}
                            disabled={isCreator}
                            onChange={() => toggleUser(u.username)}
                            className="size-3.5 rounded border-neutral-300 text-emerald-600 focus:ring-emerald-500/20 shrink-0"
                          />
                          <span className="font-medium text-neutral-900 dark:text-zinc-100 truncate">
                            {u.displayName || u.username}
                          </span>
                          <span className="text-[11px] text-neutral-400 dark:text-zinc-500 shrink-0">
                            @{u.username}
                          </span>
                        </div>
                        {isCreator ? (
                          <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400 shrink-0">
                            {t('projects.roleCreator', { defaultValue: '创建者 · 管理员' })}
                          </span>
                        ) : isSelected ? (
                          <div className="flex items-center gap-1.5 shrink-0" onClick={(e) => e.stopPropagation()}>
                            <select
                              value={memberRoles[u.username] || 'operator'}
                              onChange={(e) => changeMemberRole(u.username, e.target.value as ProjectMemberRole)}
                              className="rounded border border-neutral-300 bg-white px-2 py-0.5 text-[11px] font-medium text-neutral-700 shadow-sm outline-none transition focus:border-emerald-500 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200"
                            >
                              <option value="operator">{t('users.prole_operator', { defaultValue: '执行者' })}</option>
                              <option value="viewer">{t('users.prole_viewer', { defaultValue: '查看者' })}</option>
                              <option value="manager">{t('users.prole_manager', { defaultValue: '项目管理者' })}</option>
                            </select>
                          </div>
                        ) : (
                          <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] text-neutral-400 dark:bg-zinc-800/60 dark:text-zinc-500 shrink-0">
                            {t('users.prole_operator', { defaultValue: '执行者' })}
                          </span>
                        )}
                      </div>
                    )
                  })}
                </div>
              </div>
            )}
          </div>

          {/* Section 4: ChatOps Channel */}
          <div className="rounded-xl border border-neutral-200/80 bg-neutral-50/40 transition dark:border-zinc-800 dark:bg-zinc-800/20">
            <div className="flex items-center justify-between p-3.5">
              <div className="flex items-center gap-2.5 min-w-0">
                <input
                  type="checkbox"
                  checked={chatopsEnabled}
                  onChange={(e) => setChatopsEnabled(e.target.checked)}
                  className="size-4 rounded border-neutral-300 text-sky-600 focus:ring-sky-500/20"
                />
                <div className="flex size-7 shrink-0 items-center justify-center rounded-md bg-sky-50 text-sky-600 dark:bg-sky-950/40 dark:text-sky-400">
                  <MessageSquare className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-semibold text-neutral-900 dark:text-zinc-100">
                      {t('projects.chatopsTitle', { defaultValue: '协同频道' })}
                    </span>
                    {chatopsEnabled ? (
                      <span className="inline-flex items-center gap-1 rounded bg-sky-50 px-1.5 py-0.5 text-[10px] font-medium text-sky-700 dark:bg-sky-950/50 dark:text-sky-300">
                        <Lock className="size-2.5" />
                        {channelVisibility === 'private' ? '私有频道' : '公开频道'}
                      </span>
                    ) : (
                      <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                        {t('common.disabled', { defaultValue: '未启用' })}
                      </span>
                    )}
                  </div>
                  <p className="truncate text-[11px] text-neutral-500 dark:text-zinc-400">
                    {chatopsEnabled
                      ? t('projects.chatopsSummary', {
                          defaultValue: `将在【${activeInstance}】自动创建私有频道 ${effectiveChannelName}，同步 Agent 与成员`,
                          instance: activeInstance,
                          channel: effectiveChannelName,
                        })
                      : t('projects.chatopsDisabledHint', { defaultValue: '不自动创建 Mattermost 项目频道' })}
                  </p>
                </div>
              </div>

              {chatopsEnabled && (
                <button
                  type="button"
                  data-tour-chatops-details
                  onClick={() => setChatopsExpanded((prev) => !prev)}
                  className="inline-flex shrink-0 whitespace-nowrap items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium text-neutral-600 hover:bg-neutral-200/50 dark:text-zinc-400 dark:hover:bg-zinc-700/50"
                >
                  {chatopsExpanded ? (
                    <>
                      {t('common.collapse', { defaultValue: '收起' })} <ChevronUp className="size-3.5" />
                    </>
                  ) : (
                    <>
                      {t('projects.chatopsDetails', { defaultValue: '配置细节' })}{' '}
                      <ChevronDown className="size-3.5" />
                    </>
                  )}
                </button>
              )}
            </div>

            {chatopsEnabled && chatopsExpanded && (
              <div className="space-y-3 border-t border-neutral-200/60 p-3.5 pt-2.5 dark:border-zinc-800">
                {/* Instance Selector if multiple */}
                {imInstances.length > 1 && (
                  <div>
                    <label className="block text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                      {t('projects.imInstance', { defaultValue: '目标 IM 实例' })}
                    </label>
                    <select
                      value={selectedInstanceId}
                      onChange={(e) => setSelectedInstanceId(e.target.value)}
                      className="mt-1 w-full rounded-md border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-900 outline-none focus:border-sky-500 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                    >
                      {imInstances.map((inst) => (
                        <option key={inst.id} value={inst.id}>
                          {inst.displayName || inst.display_name || inst.provider} ({inst.provider})
                        </option>
                      ))}
                    </select>
                  </div>
                )}

                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  {/* Channel Mode */}
                  <div>
                    <span className="block text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                      {t('projects.channelMode', { defaultValue: '频道模式' })}
                    </span>
                    <div className="mt-1 flex items-center gap-3 text-xs">
                      <label className="inline-flex items-center gap-1.5 cursor-pointer">
                        <input
                          type="radio"
                          name="channelMode"
                          value="create"
                          checked={channelMode === 'create'}
                          onChange={() => setChannelMode('create')}
                          className="size-3.5 text-sky-600"
                        />
                        <span>{t('projects.modeCreate', { defaultValue: '自动创建新频道' })}</span>
                      </label>
                      <label className="inline-flex items-center gap-1.5 cursor-pointer">
                        <input
                          type="radio"
                          name="channelMode"
                          value="link"
                          checked={channelMode === 'link'}
                          onChange={() => setChannelMode('link')}
                          className="size-3.5 text-sky-600"
                        />
                        <span>{t('projects.modeLink', { defaultValue: '关联已有' })}</span>
                      </label>
                    </div>
                  </div>

                  {/* Channel Visibility */}
                  <div>
                    <span className="block text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                      {t('projects.visibility', { defaultValue: '频道可见性' })}
                    </span>
                    <div className="mt-1 flex items-center gap-3 text-xs">
                      <label className="inline-flex items-center gap-1.5 cursor-pointer">
                        <input
                          type="radio"
                          name="channelVisibility"
                          value="private"
                          checked={channelVisibility === 'private'}
                          onChange={() => setChannelVisibility('private')}
                          className="size-3.5 text-sky-600"
                        />
                        <span className="inline-flex items-center gap-1 font-medium text-emerald-700 dark:text-emerald-400">
                          <Lock className="size-3" />
                          {t('projects.visPrivate', { defaultValue: '私有频道 (推荐)' })}
                        </span>
                      </label>
                      <label className="inline-flex items-center gap-1.5 cursor-pointer">
                        <input
                          type="radio"
                          name="channelVisibility"
                          value="public"
                          checked={channelVisibility === 'public'}
                          onChange={() => setChannelVisibility('public')}
                          className="size-3.5 text-sky-600"
                        />
                        <span className="inline-flex items-center gap-1 text-neutral-600 dark:text-zinc-400">
                          <Globe className="size-3" />
                          {t('projects.visPublic', { defaultValue: '公开频道' })}
                        </span>
                      </label>
                    </div>
                  </div>
                </div>

                {/* Channel Name Input */}
                <div>
                  <label className="block text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                    {channelMode === 'create'
                      ? t('projects.channelName', { defaultValue: '频道名称' })
                      : t('projects.existingChannelName', { defaultValue: '已有频道名称或 ID' })}
                  </label>
                  <input
                    type="text"
                    value={customChannelName}
                    onChange={(e) => setCustomChannelName(e.target.value)}
                    placeholder={effectiveChannelName}
                    className="mt-1 w-full rounded-md border border-neutral-200 bg-white px-2.5 py-1.5 font-mono text-xs text-neutral-900 outline-none focus:border-sky-500 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                  />
                </div>

                {/* Access preview */}
                <div className="rounded-lg bg-sky-50/60 p-2.5 text-[11px] text-sky-800 dark:bg-sky-950/30 dark:text-sky-300">
                  <div className="font-medium mb-1">
                    {t('projects.previewTitle', { defaultValue: '成员准入预览' })}:
                  </div>
                  <ul className="space-y-0.5">
                    {memberChannelPreview.map((m) => (
                      <li key={m.username} className="flex items-center gap-1.5">
                        {m.isBound ? (
                          <>
                            <Check className="size-3 text-emerald-600 dark:text-emerald-400" />
                            <span>@{m.username} (已绑定 {activeInstance}，将自动加入频道)</span>
                          </>
                        ) : (
                          <>
                            <Info className="size-3 text-amber-500" />
                            <span className="text-neutral-600 dark:text-zinc-400">
                              @{m.username} (尚未绑定该 IM，将保留为待邀请状态)
                            </span>
                          </>
                        )}
                      </li>
                    ))}
                  </ul>
                </div>

                <p className="text-[10px] text-neutral-400 dark:text-zinc-500">
                  💡 {t('projects.chatopsHint', {
                    defaultValue: '仅在启动带流水线工作流的任务时会在该频道创建 Task Thread，普通任务不产生打扰。',
                  })}
                </p>
              </div>
            )}
          </div>
        </div>

        {/* Footer Actions */}
        <div className="flex shrink-0 items-center justify-between border-t border-neutral-200/70 bg-neutral-50/50 px-6 py-3.5 dark:border-zinc-800 dark:bg-zinc-800/30">
          <div className="text-xs text-neutral-500 dark:text-zinc-400">
            {saving && saveStep && (
              <span className="inline-flex items-center gap-1.5 text-sky-600 dark:text-sky-400">
                <Loader2 className="size-3.5 animate-spin" />
                {saveStep}
              </span>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={saving}
              onClick={onClose}
              className="rounded-lg border border-neutral-200 bg-white px-3.5 py-1.5 text-xs font-medium text-neutral-700 shadow-xs transition hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
            >
              {t('common.cancel', { defaultValue: '取消' })}
            </button>
            <button
              type="button"
              data-tour-project-submit
              disabled={saving || !name.trim()}
              onClick={() => void handleCreate()}
              className="inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-4 py-1.5 text-xs font-medium text-white shadow-xs transition hover:bg-sky-700 focus:outline-none focus:ring-2 focus:ring-sky-500/20 disabled:opacity-50"
            >
              {saving ? (
                <>
                  <Loader2 className="size-3.5 animate-spin" />
                  {t('common.creating', { defaultValue: '正在创建...' })}
                </>
              ) : (
                t('projects.createAction', { defaultValue: '立即创建' })
              )}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
