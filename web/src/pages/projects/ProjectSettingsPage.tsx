import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {
  AlertTriangle,
  ArrowRight,
  CheckCircle2,
  FileText,
  FolderGit2,
  GitBranch,
  Globe,
  HardDrive,
  Laptop,
  Layers,
  Loader2,
  Lock,
  MessageSquare,
  Plus,
  RotateCw,
  Save,
  Sparkles,
  Trash2,
  Unlock,
  Users,
  X,
  Zap,
} from 'lucide-react'
import { cn } from '../../lib/cn'
import { useApiJson } from '../../lib/use-api'
import { useFormatDateTime } from '../../lib/format-datetime'
import { ApiError, apiDelete, apiFetch, apiPost, apiPut } from '../../lib/api'
import { ConfirmDialog } from '../../components/ui/ConfirmDialog'
import { showToast } from '../../components/ui/Toast'

type ProjectDetail = {
  name: string
  description: string
  repo: string
  defaultRepo?: string
  remoteProvider?: string
  remoteConnection?: string
  remoteProjectId?: string
  remoteUrl?: string
  cloneUrl?: string
  defaultBranch?: string
  templateId?: string
  templateVersion?: string
  templateDigest?: string
  deployPort?: number
  runtimeProfile?: string
}
type PromptData = { content: string }

export default function ProjectSettingsPage() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { projectId } = useParams<{ projectId: string }>()
  const [reloadKey, setReloadKey] = useState(0)

  const detailPath = projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}` : null
  const detailState = useApiJson<ProjectDetail>(detailPath, reloadKey)

  const promptPath = projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}/prompt` : null
  const promptState = useApiJson<PromptData>(promptPath)

  const detail = detailState.status === 'ok' ? detailState.data : null

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="shrink-0 px-6 pt-5 pb-3">
        <h1 className="text-xl font-semibold text-neutral-900 dark:text-zinc-100">{t('projectNav.settings')}</h1>
        <p className="mt-0.5 text-sm text-neutral-500 dark:text-zinc-500">{t('projectSettings.subtitle')}</p>
      </div>

      <div className="flex-1 overflow-y-auto px-6 pb-6">
        <div className="space-y-6">
          {/* Basic info */}
          {detail && projectId && (
            <BasicInfoEditor
              projectId={projectId}
              name={detail.name}
              initialDescription={detail.description}
              initialRepo={detail.repo}
              defaultRepo={detail.defaultRepo}
              remoteProvider={detail.remoteProvider}
              remoteConnection={detail.remoteConnection}
              remoteProjectId={detail.remoteProjectId}
              remoteUrl={detail.remoteUrl}
              cloneUrl={detail.cloneUrl}
              defaultBranch={detail.defaultBranch}
              deployPort={detail.deployPort}
              initialRuntimeProfile={detail.runtimeProfile}
              onReload={() => setReloadKey((k) => k + 1)}
            />
          )}

          {/* Project prompt */}
          {promptState.status === 'ok' && projectId && (
            <PromptEditor
              label={t('prompt.projectPrompt')}
              apiPath={`/api/v1/projects/${encodeURIComponent(projectId)}/prompt`}
              initialContent={promptState.data.content}
            />
          )}

          {/* Installed connection tools (agent tool bindings) */}
          {projectId && <InstalledConnections projectId={projectId} />}

          {/* Project ChatOps Channel */}
          {projectId && <ProjectChatOpsChannel projectId={projectId} />}

          {projectId && (
            <DangerZone
              projectId={projectId}
              onDeleted={() => navigate('/projects')}
            />
          )}
        </div>
      </div>
    </div>
  )
}

type ProjectToolBinding = {
  id: string
  agentId?: string
  agentWorkerId?: string
  connectionId: string
  connectionName?: string
  provider: string
  adapterType?: string
  status: string
  updatedAt?: string
}

type ProjectAgentSummary = { name: string }

type ConnectionGrant = {
  id: string
  targetType: string
  targetId: string
}

// Code-host providers whose repo/push/CI role runs through the workspace-level
// connection regardless of project bindings (kept in sync with ConnectionsPage).
const codeHostProviders = new Set(['gitlab', 'github', 'gitee'])

// Aggregates the project's agent tool bindings by connection: one card per
// installed connection with the covered agents as removable chips. Platform
// features (Git push, the design gate) use workspace-level connections and are
// unaffected — the copy says so explicitly.
function InstalledConnections({ projectId }: { projectId: string }) {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()
  const [reloadKey, setReloadKey] = useState(0)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [removing, setRemoving] = useState<{ connectionId: string; label: string } | null>(null)
  const [addingFor, setAddingFor] = useState<string | null>(null)

  const path = `/api/v1/projects/${encodeURIComponent(projectId)}/tool-bindings`
  const state = useApiJson<{ bindings: ProjectToolBinding[] }>(path, reloadKey, { silentStatuses: [404], keepPreviousDataOnReload: true })
  const bindings = state.status === 'ok' ? (state.data.bindings ?? []) : []

  const agentsPath = `/api/v1/projects/${encodeURIComponent(projectId)}/agents`
  const agentsState = useApiJson<ProjectAgentSummary[]>(agentsPath, reloadKey, { silentStatuses: [404], keepPreviousDataOnReload: true })
  const agents = agentsState.status === 'ok' ? (agentsState.data ?? []) : []
  const isAgentsReady = agentsState.status === 'ok'

  // One entry per distinct connection, preserving first-seen order. Stale
  // bindings whose agent no longer sits in the project are flagged so admins
  // can clean them up instead of wondering why a ghost agent shows up.
  // Never flag stale during loading/revalidation before member list resolves.
  const cards = useMemo(() => {
    const byConnection = new Map<string, ProjectToolBinding[]>()
    for (const binding of bindings) {
      const list = byConnection.get(binding.connectionId)
      if (list) list.push(binding)
      else byConnection.set(binding.connectionId, [binding])
    }
    const memberNames = new Set(agents.map((a) => a.name).filter(Boolean))
    return Array.from(byConnection.entries()).map(([connectionId, rows]) => {
      const first = rows[0]
      const covered = new Set(rows.map((r) => (r.agentId || r.agentWorkerId || '').trim()).filter(Boolean))
      const stale = isAgentsReady
        ? rows.filter((r) => {
            const name = (r.agentId || '').trim()
            return name !== '' && !memberNames.has(name)
          })
        : []
      return { connectionId, rows, first, covered, stale }
    })
  }, [bindings, agents, isAgentsReady])

  const refresh = useCallback(() => setReloadKey((k) => k + 1), [])

  function agentsCoveredLabels(connectionId: string): string {
    const names = bindings
      .filter((b) => b.connectionId === connectionId)
      .map((b) => (b.agentId || b.agentWorkerId || '').trim())
      .filter(Boolean)
    return names.length > 0 ? names.join('、') : '—'
  }

  async function removeBinding(binding: ProjectToolBinding) {
    setBusyId(binding.id)
    try {
      await apiDelete(`/api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(binding.agentId || '')}/tool-bindings/${encodeURIComponent(binding.id)}`)
      refresh()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setBusyId(null)
    }
  }

  // Uninstall = delete every binding under this connection plus the
  // project-level grant (otherwise the runtime would still admit the
  // connection via grant matching with no binding left to narrow it).
  // Server errors stop the loop: a silent half-uninstall is the worst
  // outcome (bindings gone, grant still admits the connection at runtime).
  async function uninstallConnection(connectionId: string, _label?: string) {
    setBusyId(connectionId)
    try {
      const rows = bindings.filter((b) => b.connectionId === connectionId)
      let deletedBindings = 0
      let deletedGrants = 0
      const errors: string[] = []
      for (const row of rows) {
        try {
          await apiDelete(`/api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(row.agentId || '')}/tool-bindings/${encodeURIComponent(row.id)}`)
          deletedBindings += 1
        } catch (e) {
          errors.push(e instanceof Error ? e.message : String(e))
        }
      }
      let grantsState: { grants: ConnectionGrant[] } | null = null
      try {
        grantsState = await apiFetch<{ grants: ConnectionGrant[] }>(`/api/v1/connections/${encodeURIComponent(connectionId)}/grants`)
      } catch (e) {
        errors.push(e instanceof Error ? e.message : String(e))
      }
      if (grantsState) {
        for (const grant of grantsState.grants ?? []) {
          if (grant.targetType === 'project' && grant.targetId === projectId) {
            try {
              await apiDelete(`/api/v1/connections/${encodeURIComponent(connectionId)}/grants/${encodeURIComponent(grant.id)}`)
              deletedGrants += 1
            } catch (e) {
              errors.push(e instanceof Error ? e.message : String(e))
            }
          }
        }
      }
      if (errors.length > 0) {
        showToast(t('projectSettings.installedPartialError', {
          defaultValue: '卸载未完全成功：已移除 {{bindings}} 条绑定、{{grants}} 条项目授权；{{count}} 项失败。错误：{{errors}}',
          bindings: deletedBindings,
          grants: deletedGrants,
          count: errors.length,
          errors: errors.slice(0, 2).join('；'),
        }), 'error')
      } else {
        showToast(t('projectSettings.installedRemovedToast', { defaultValue: '已卸载' }) + ' ✓', 'success')
      }
      setRemoving(null)
      refresh()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
      refresh()
    } finally {
      setBusyId(null)
    }
  }

  async function addAgentToConnection(connectionId: string, agentName: string) {
    setBusyId(connectionId)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(agentName)}/tool-bindings`, {
        connectionId,
        status: 'enabled',
      })
      setAddingFor(null)
      refresh()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <section className="rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40">
      <div className="border-b border-neutral-100 px-5 py-3 dark:border-zinc-800">
        <span className="text-sm font-semibold text-neutral-800 dark:text-zinc-100">{t('projectSettings.installedTitle', { defaultValue: '已安装的连接工具' })}</span>
        <p className="mt-1 text-xs leading-relaxed text-neutral-500 dark:text-zinc-500">{t('projectSettings.installedDesc', { defaultValue: '这些连接已授权给本项目内的 Agent 运行时使用。' })}</p>
      </div>
      {state.status === 'loading' && bindings.length === 0 ? (
        <div className="flex items-center gap-2 px-5 py-4 text-sm text-neutral-500 dark:text-zinc-400">
          <Loader2 className="size-4 animate-spin" />
          {t('common.loading', { defaultValue: '加载中…' })}
        </div>
      ) : cards.length === 0 ? (
        <p className="px-5 py-4 text-sm text-neutral-500 dark:text-zinc-500">{t('projectSettings.installedEmpty', { defaultValue: '还没有授权任何连接工具给本项目的 Agent。' })}</p>
      ) : (
        <ul className="divide-y divide-neutral-100 dark:divide-zinc-800">
          {cards.map((card) => {
            const candidates = agents.filter((a) => !card.covered.has(a.name))
            const label = `${card.first.provider}${card.first.connectionName ? ` / ${card.first.connectionName}` : ''}`
            return (
              <li key={card.connectionId} className="px-5 py-3">
                <div className="flex items-start justify-between gap-4">
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium text-neutral-800 dark:text-zinc-200">
                      {card.first.provider}
                      {card.first.connectionName ? <span className="text-neutral-400 dark:text-zinc-500"> / {card.first.connectionName}</span> : null}
                    </p>
                    <p className="mt-0.5 text-xs text-neutral-500 dark:text-zinc-500">
                      {card.rows.length} agent{card.rows.length > 1 ? 's' : ''}
                      {card.first.adapterType ? ` · ${card.first.adapterType}` : ''}
                      {card.first.updatedAt ? ` · ${fmt(card.first.updatedAt)}` : ''}
                    </p>
                    {codeHostProviders.has(card.first.provider) && (
                      <p className="mt-1 text-[11px] text-sky-700/80 dark:text-sky-400/80">
                        {t('projectSettings.installedCodeHostNote', { defaultValue: '代码托管类连接：远端仓库、Git 推送与 CI 流水线走工作区级连接，不受卸载影响。' })}
                      </p>
                    )}
                  </div>
                  <button
                    type="button"
                    disabled={busyId === card.connectionId}
                    onClick={() => setRemoving({ connectionId: card.connectionId, label })}
                    className="shrink-0 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-xs font-medium text-neutral-600 transition-colors hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
                  >
                    {busyId === card.connectionId ? <Loader2 className="size-3.5 animate-spin" /> : t('projectSettings.installedUninstall', { defaultValue: '卸载' })}
                  </button>
                </div>
                <div className="mt-2 flex flex-wrap items-center gap-1.5">
                  {card.rows.map((row) => {
                    const agentName = row.agentId || row.agentWorkerId || ''
                    const isStale = card.stale.some((s) => s.id === row.id)
                    return (
                      <span
                        key={row.id}
                        className={cn(
                          'inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs',
                          isStale
                            ? 'border-amber-300 bg-amber-50 text-amber-700 dark:border-amber-700/60 dark:bg-amber-900/20 dark:text-amber-300'
                            : 'border-neutral-200 bg-neutral-50 text-neutral-600 dark:border-zinc-700 dark:bg-zinc-800/60 dark:text-zinc-300'
                        )}
                        title={isStale ? t('projectSettings.installedStaleTip', { defaultValue: '该 Agent 已不在项目成员中，建议移除' }) : undefined}
                      >
                        {agentName}
                        {isStale ? <AlertTriangle className="size-3" /> : null}
                        <button
                          type="button"
                          aria-label={t('projectSettings.installedRemove', { defaultValue: '移除授权' })}
                          disabled={busyId === card.connectionId}
                          onClick={() => void removeBinding(row)}
                          className="ml-0.5 rounded-full p-0.5 text-neutral-400 transition-colors hover:bg-neutral-200 hover:text-neutral-700 disabled:opacity-40 dark:hover:bg-zinc-700 dark:hover:text-zinc-200"
                        >
                          <X className="size-3" />
                        </button>
                      </span>
                    )
                  })}
                  <span className="relative inline-flex">
                    <button
                      type="button"
                      disabled={busyId === card.connectionId || candidates.length === 0}
                      onClick={() => setAddingFor(addingFor === card.connectionId ? null : card.connectionId)}
                      className="inline-flex items-center gap-1 rounded-full border border-dashed border-neutral-300 px-2 py-0.5 text-xs text-neutral-500 transition-colors hover:border-neutral-400 hover:text-neutral-700 disabled:opacity-40 dark:border-zinc-600 dark:text-zinc-400 dark:hover:border-zinc-500 dark:hover:text-zinc-200"
                    >
                      <Plus className="size-3" />
                      {t('projectSettings.installedAddAgent', { defaultValue: '添加 Agent' })}
                    </button>
                    {addingFor === card.connectionId && candidates.length > 0 ? (
                      <span className="absolute left-0 top-full z-10 mt-1 w-44 rounded-lg border border-neutral-200 bg-white py-1 shadow-lg dark:border-zinc-700 dark:bg-zinc-900">
                        {candidates.map((agent) => (
                          <button
                            key={agent.name}
                            type="button"
                            onClick={() => void addAgentToConnection(card.connectionId, agent.name)}
                            className="block w-full px-3 py-1.5 text-left text-xs text-neutral-700 transition-colors hover:bg-neutral-50 dark:text-zinc-300 dark:hover:bg-zinc-800"
                          >
                            {agent.name}
                          </button>
                        ))}
                      </span>
                    ) : null}
                  </span>
                </div>
              </li>
            )
          })}
        </ul>
      )}
      <ConfirmDialog
        open={removing !== null}
        title={t('projectSettings.installedUninstall', { defaultValue: '卸载' })}
        description={removing
          ? t('projectSettings.installedUninstallConfirmDetail', {
              defaultValue: `卸载「${removing.label}」将删除 {{bindings}} 条 Agent 绑定（{{agents}}）和 1 条项目级授权；之后本项目 Agent 将无法再调用该连接的工具。`,
              name: removing.label,
              bindings: bindings.filter((b) => b.connectionId === removing.connectionId).length,
              agents: agentsCoveredLabels(removing.connectionId),
            })
          : ''}
        confirmLabel={t('projectSettings.installedUninstall', { defaultValue: '卸载' })}
        cancelLabel={t('common.cancel')}
        busy={busyId !== null}
        onCancel={() => setRemoving(null)}
        onConfirm={() => { if (removing) void uninstallConnection(removing.connectionId, removing.label) }}
      />
    </section>
  )
}

type ProjectChannelLink = {
  id: string
  workspaceId: string
  projectId: string
  provider: string
  imInstanceId: string
  teamId: string
  channelId: string
  channelName: string
  displayName: string
  visibility: string
  status: string
  createdAt: string
  updatedAt: string
}

type AgentChannelBinding = {
  id: string
  workspaceId: string
  agentWorkerId?: string
  projectId: string
  agentId: string
  provider: string
  connectionId: string
  externalBotId?: string
  externalChatId: string
  status: string
  lastActivityAt?: string
}

type ProjectChannelListItem = {
  link: ProjectChannelLink
  bindings: AgentChannelBinding[]
  hasError: boolean
}

function ProjectChatOpsChannel({ projectId }: { projectId: string }) {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()
  const [reloadKey, setReloadKey] = useState(0)
  const [retrying, setRetrying] = useState(false)

  const path = `/api/v1/projects/${encodeURIComponent(projectId)}/channels`
  const state = useApiJson<{ ok: boolean; channels: ProjectChannelListItem[] }>(path, reloadKey, {
    silentStatuses: [404],
    keepPreviousDataOnReload: true,
  })
  const channels = state.status === 'ok' ? (state.data.channels ?? []) : []

  async function handleRetrySync(item: ProjectChannelListItem) {
    setRetrying(true)
    try {
      const res = await apiPost<{
        ok: boolean
        status?: string
        warning?: string
        failedMembers?: string[]
        failedAgents?: string[]
        boundAgents?: string[]
      }>(`/api/v1/projects/${encodeURIComponent(projectId)}/channels/provision`, {
        provider: item.link.provider || 'mattermost',
        mode: 'link',
        instanceId: item.link.imInstanceId || undefined,
        teamId: item.link.teamId || undefined,
        channelName: item.link.channelName,
        displayName: item.link.displayName,
        visibility: item.link.visibility,
      })

      if (res?.status === 'partial' && res.warning) {
        showToast(res.warning, 'info')
      } else {
        showToast(t('projectSettings.chatopsSyncSuccess', { defaultValue: '协同频道与 Agent 机器人同步成功' }), 'success')
      }
      setReloadKey((k) => k + 1)
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setRetrying(false)
    }
  }

  return (
    <section className="rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40">
      <div className="flex items-center justify-between border-b border-neutral-100 px-5 py-3 dark:border-zinc-800">
        <div>
          <div className="flex items-center gap-2">
            <span className="text-sm font-semibold text-neutral-800 dark:text-zinc-100">
              {t('projectSettings.chatopsTitle', { defaultValue: '项目协同频道 (ChatOps)' })}
            </span>
          </div>
          <p className="mt-1 text-xs leading-relaxed text-neutral-500 dark:text-zinc-500">
            {t('projectSettings.chatopsDesc', {
              defaultValue: '管理与本项目绑定的即时通讯 (Mattermost) 协同频道及团队 Agent 机器人状态。',
            })}
          </p>
        </div>
        <button
          type="button"
          onClick={() => setReloadKey((k) => k + 1)}
          disabled={state.status === 'loading'}
          className="inline-flex items-center gap-1 rounded border border-neutral-200 bg-white px-2 py-1 text-xs text-neutral-600 transition-colors hover:bg-neutral-50 disabled:opacity-40 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700"
          title={t('common.refresh', { defaultValue: '刷新' })}
        >
          <RotateCw className={cn('size-3', state.status === 'loading' && 'animate-spin')} />
          {t('common.refresh', { defaultValue: '刷新' })}
        </button>
      </div>

      {state.status === 'loading' && channels.length === 0 ? (
        <div className="flex items-center gap-2 px-5 py-4 text-sm text-neutral-500 dark:text-zinc-400">
          <Loader2 className="size-4 animate-spin" />
          {t('common.loading', { defaultValue: '加载中…' })}
        </div>
      ) : channels.length === 0 ? (
        <div className="px-5 py-6 text-center text-sm text-neutral-500 dark:text-zinc-400">
          <MessageSquare className="mx-auto mb-2 size-8 text-neutral-300 dark:text-zinc-600" />
          <p className="font-medium text-neutral-700 dark:text-zinc-300">
            {t('projectSettings.chatopsEmptyTitle', { defaultValue: '未关联或创建协同频道' })}
          </p>
          <p className="mt-1 text-xs text-neutral-400 dark:text-zinc-500">
            {t('projectSettings.chatopsEmptyDesc', {
              defaultValue: '新建项目时开启协同频道，系统将自动在 Mattermost 中创建专属项目频道并完成机器人与成员配置。',
            })}
          </p>
        </div>
      ) : (
        <div className="divide-y divide-neutral-100 dark:divide-zinc-800">
          {channels.map((chan) => {
            const hasError = chan.hasError
            const errorCount = chan.bindings.filter((b) => b.status === 'error').length

            return (
              <div key={chan.link.id} className="p-5 space-y-4">
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start gap-3">
                    <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-indigo-50 text-indigo-600 dark:bg-indigo-950/50 dark:text-indigo-400">
                      <MessageSquare className="size-5" />
                    </div>
                    <div>
                      <div className="flex items-center gap-2">
                        <span className="font-medium text-neutral-900 dark:text-zinc-100">
                          {chan.link.displayName || `#${chan.link.channelName}`}
                        </span>
                        <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[11px] font-medium text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400">
                          {chan.link.provider.toUpperCase()}
                        </span>
                        <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[11px] text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                          {chan.link.visibility === 'public'
                            ? t('projects.channelVisibilityPublic', { defaultValue: '公开频道' })
                            : t('projects.channelVisibilityPrivate', { defaultValue: '私有频道' })}
                        </span>
                        {hasError ? (
                          <span className="inline-flex items-center gap-1 rounded bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-950/60 dark:text-amber-300">
                            <AlertTriangle className="size-3" />
                            {t('projectSettings.chatopsPartialError', {
                              defaultValue: '存在异常绑定 ({{count}})',
                              count: errorCount,
                            })}
                          </span>
                        ) : (
                          <span className="inline-flex items-center gap-1 rounded bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-300">
                            <CheckCircle2 className="size-3" />
                            {t('projectSettings.chatopsStatusNormal', { defaultValue: '正常' })}
                          </span>
                        )}
                      </div>
                      <div className="mt-1 flex items-center gap-3 text-xs text-neutral-500 dark:text-zinc-400">
                        <span>频道名: <code className="rounded bg-neutral-100 px-1 py-0.5 text-neutral-700 dark:bg-zinc-800 dark:text-zinc-300">{chan.link.channelName}</code></span>
                        {chan.link.updatedAt && <span>更新于: {fmt(chan.link.updatedAt)}</span>}
                      </div>
                    </div>
                  </div>

                  <button
                    type="button"
                    disabled={retrying}
                    onClick={() => void handleRetrySync(chan)}
                    className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-xs font-medium text-neutral-700 shadow-sm transition-colors hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200 dark:hover:bg-zinc-700"
                  >
                    <RotateCw className={cn('size-3.5', retrying && 'animate-spin')} />
                    {hasError
                      ? t('projectSettings.chatopsRetrySync', { defaultValue: '重试恢复绑定' })
                      : t('projectSettings.chatopsResync', { defaultValue: '重新同步频道' })}
                  </button>
                </div>

                {hasError && (
                  <div className="flex items-start gap-2.5 rounded-lg border border-amber-200 bg-amber-50/70 p-3 text-xs text-amber-800 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-300">
                    <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
                    <div>
                      <p className="font-semibold">
                        {t('projectSettings.chatopsWarningTitle', { defaultValue: '协同机器人部分同步失败' })}
                      </p>
                      <p className="mt-0.5 leading-relaxed text-amber-700 dark:text-amber-300/90">
                        {t('projectSettings.chatopsWarningDesc', {
                          defaultValue:
                            '标记为异常的 Agent 机器人未能加入该频道（可能由于权限不足或频道限制）。请确认 Mattermost 团队与机器人设置后，点击上方【重试恢复绑定】。',
                        })}
                      </p>
                    </div>
                  </div>
                )}

                <div>
                  <h4 className="text-xs font-medium text-neutral-500 dark:text-zinc-400 mb-2">
                    {t('projectSettings.chatopsAgentsTitle', { defaultValue: '已绑定的 Agent 机器人' })}
                  </h4>
                  {chan.bindings.length === 0 ? (
                    <p className="text-xs text-neutral-400 dark:text-zinc-500">
                      {t('projectSettings.chatopsNoAgents', { defaultValue: '尚未配置绑定的 Agent 机器人。' })}
                    </p>
                  ) : (
                    <div className="flex flex-wrap gap-2">
                      {chan.bindings.map((b) => {
                        const isErr = b.status === 'error'
                        return (
                          <div
                            key={b.id}
                            className={cn(
                              'inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs',
                              isErr
                                ? 'border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-800 dark:bg-amber-950/50 dark:text-amber-200'
                                : 'border-neutral-200 bg-neutral-50 text-neutral-700 dark:border-zinc-700 dark:bg-zinc-800/80 dark:text-zinc-200'
                            )}
                          >
                            {isErr ? (
                              <AlertTriangle className="size-3.5 text-amber-600 dark:text-amber-400" />
                            ) : (
                              <CheckCircle2 className="size-3.5 text-emerald-600 dark:text-emerald-400" />
                            )}
                            <span className="font-medium">{b.agentId}</span>
                            <span className="text-[10px] opacity-70">({b.status})</span>
                          </div>
                        )
                      })}
                    </div>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}
    </section>
  )
}

function DangerZone({ projectId, onDeleted }: { projectId: string; onDeleted: () => void }) {
  const { t } = useTranslation()
  const [busy, setBusy] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)

  async function deleteProject() {
    setBusy(true)
    try {
      await apiDelete(`/api/v1/projects/${encodeURIComponent(projectId)}`)
      setConfirmOpen(false)
      onDeleted()
    } catch (e) {
      alert(String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="rounded-lg border border-red-200/80 bg-white dark:border-red-900/60 dark:bg-zinc-900/40">
      <div className="border-b border-red-100 px-5 py-3 dark:border-red-900/40">
        <span className="text-sm font-semibold text-red-700 dark:text-red-300">{t('projectSettings.dangerZone')}</span>
      </div>
      <div className="flex items-center justify-between gap-4 px-5 py-4">
        <div>
          <p className="text-sm font-medium text-neutral-800 dark:text-zinc-200">{t('projectSettings.deleteProject')}</p>
          <p className="mt-1 text-xs leading-relaxed text-neutral-500 dark:text-zinc-500">{t('projectSettings.deleteProjectDesc')}</p>
        </div>
        <button
          type="button"
          disabled={busy}
          onClick={() => setConfirmOpen(true)}
          className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-red-200 bg-white px-3 py-2 text-sm font-medium text-red-600 transition-colors hover:bg-red-50 disabled:opacity-50 dark:border-red-900/60 dark:bg-zinc-900 dark:text-red-400 dark:hover:bg-red-900/20"
        >
          <Trash2 className="size-4" strokeWidth={1.8} />
          {t('projectSettings.deleteProject')}
        </button>
      </div>
      <ConfirmDialog
        open={confirmOpen}
        title={t('projectSettings.deleteProject')}
        description={t('projectSettings.confirmDeleteProject', { name: projectId })}
        confirmLabel={t('common.delete')}
        cancelLabel={t('common.cancel')}
        busy={busy}
        onCancel={() => setConfirmOpen(false)}
        onConfirm={() => void deleteProject()}
      />
    </section>
  )
}

function BasicInfoEditor({
  projectId,
  name,
  initialDescription,
  initialRepo,
  defaultRepo,
  remoteProvider,
  remoteUrl,
  cloneUrl,
  remoteConnection,
  remoteProjectId,
  defaultBranch,
  deployPort,
  initialRuntimeProfile,
  onReload,
}: {
  projectId: string
  name: string
  initialDescription: string
  initialRepo: string
  defaultRepo?: string
  remoteProvider?: string
  remoteConnection?: string
  remoteProjectId?: string
  remoteUrl?: string
  cloneUrl?: string
  defaultBranch?: string
  deployPort?: number
  initialRuntimeProfile?: string
  onReload: () => void
}) {
  const { t } = useTranslation()
  const [description, setDescription] = useState(initialDescription ?? '')
  const [repo, setRepo] = useState(initialRepo ?? '')
  // Empty server value is presented as "auto" (server default); a declared
  // profile ("jvm21") shows as the explicit choice.
  const [runtimeProfile, setRuntimeProfile] = useState(initialRuntimeProfile ?? '')
  const [locked, setLocked] = useState(Boolean(initialRepo && initialRepo.trim() !== ''))
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [initModalOpen, setInitModalOpen] = useState(false)
  const [toastMessage, setToastMessage] = useState<string | null>(null)
  const [initializationStatus, setInitializationStatus] = useState<ProjectInitializationStatus>({ status: 'loading' })

  // Initialization is durable server state. Hydrate it when this page opens
  // and keep the action label current while an existing run is executing.
  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined

    const poll = async () => {
      try {
        const data = await apiFetch<ProjectInitializationStatus>(
          `/api/v1/projects/${encodeURIComponent(projectId)}/initialization`,
          { silentStatuses: [404] },
        )
        if (cancelled) return
        setInitializationStatus(data)
        if (isInitializationRunning(data.status)) {
          timer = setTimeout(poll, 2500)
        }
      } catch {
        if (!cancelled) setInitializationStatus({ status: 'idle' })
      }
    }

    void poll()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [projectId])

  const save = useCallback(async () => {
    setSaving(true)
    setSaved(false)
    try {
      // Three-state semantics: "auto" (empty) and "base" are DIFFERENT
      // declarations — auto inherits the agent preference or server default,
      // explicit base pins the project to base and wins over agent
      // preferences. The field is always sent explicitly so switching to auto
      // is a deliberate clear, while the selected value persists verbatim.
      const payload: Record<string, unknown> = { description, repo }
      if (runtimeProfile === '') {
        payload.runtimeProfile = ''
      } else {
        payload.runtimeProfile = runtimeProfile
      }
      await apiPut(`/api/v1/projects/${encodeURIComponent(projectId)}`, payload)
      setDirty(false)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (e) {
      alert(String(e))
    } finally {
      setSaving(false)
    }
  }, [projectId, description, repo, runtimeProfile])

  const change = useCallback((setter: (v: string) => void) => (v: string) => {
    setter(v)
    setDirty(true)
    setSaved(false)
  }, [])

  function handleUnlock() {
    if (window.confirm(t('projectSettings.unlockHint'))) {
      setLocked(false)
    }
  }

  function handleInitSuccess(newRepo: string) {
    setRepo(newRepo)
    setLocked(true)
    setDirty(false)
    setInitModalOpen(false)
    setToastMessage(t('projectSettings.initSuccess'))
    onReload()
    setTimeout(() => setToastMessage(null), 6000)
  }

  const initStatus = initializationStatus.status
  const initRunning = isInitializationRunning(initStatus)
  const initButtonLabel = initStatus === 'loading'
    ? t('projectSettings.initCheckingStatus')
    : initRunning
      ? t('projectSettings.initInProgressButton')
      : initStatus === 'failed'
        ? t('projectSettings.initFailedButton')
        : t('projectSettings.initWorkspace')

  return (
    <section className="rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40">
      <div className="flex items-center justify-between border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold text-neutral-800 dark:text-zinc-200">{t('projectSettings.basicInfo')}</span>
          {dirty && <span className="text-[10px] text-amber-500">●</span>}
          {saved && <span className="text-[10px] text-emerald-500">{t('prompt.saved')}</span>}
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={save}
            disabled={saving || !dirty}
            className="flex items-center gap-1 rounded-md bg-sky-600 px-2.5 py-1 text-[11px] font-medium text-white transition-colors hover:bg-sky-700 disabled:opacity-50"
          >
            <Save className="size-3" strokeWidth={2} />
            {saving ? t('prompt.saving') : t('prompt.save')}
          </button>
        </div>
      </div>

      {toastMessage && (
        <div className="mx-5 mt-4 flex items-center justify-between gap-3 rounded-lg border border-emerald-200 bg-emerald-50 px-3.5 py-2.5 text-xs text-emerald-800 dark:border-emerald-900/60 dark:bg-emerald-950/30 dark:text-emerald-300">
          <div className="flex items-center gap-2">
            <CheckCircle2 className="size-4 text-emerald-600 dark:text-emerald-400 shrink-0" />
            <span>{toastMessage}</span>
          </div>
          <Link
            to={`/projects/${encodeURIComponent(projectId)}/tasks`}
            className="flex items-center gap-1 font-semibold text-emerald-700 hover:underline dark:text-emerald-300 shrink-0"
          >
            {t('projectSettings.viewTasks')} <ArrowRight className="size-3" />
          </Link>
        </div>
      )}

      <dl className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
        {/* Name — read-only */}
        <div className="flex items-baseline gap-4 px-5 py-2.5">
          <dt className="w-32 shrink-0 text-xs font-medium text-neutral-500 dark:text-zinc-500">{t('projectSettings.name')}</dt>
          <dd className="font-mono text-sm text-neutral-800 dark:text-zinc-200">{name}</dd>
        </div>
        {/* Description — editable */}
        <div className="flex items-start gap-4 px-5 py-2.5">
          <dt className="w-32 shrink-0 pt-1.5 text-xs font-medium text-neutral-500 dark:text-zinc-500">{t('projectSettings.description')}</dt>
          <dd className="flex-1">
            <input
              type="text"
              value={description}
              onChange={(e) => change(setDescription)(e.target.value)}
              placeholder="—"
              className="w-full rounded-md border border-neutral-200 bg-transparent px-2.5 py-1 text-sm text-neutral-800 outline-none placeholder:text-neutral-400 focus:border-sky-400 focus:ring-1 focus:ring-sky-400/30 dark:border-zinc-700 dark:text-zinc-200 dark:placeholder:text-zinc-600 dark:focus:border-sky-500"
            />
          </dd>
        </div>
        {/* Repo — with lock/unlock status */}
        <div className="flex items-start gap-4 px-5 py-2.5">
          <dt className="w-32 shrink-0 pt-1.5 text-xs font-medium text-neutral-500 dark:text-zinc-500">{t('projectSettings.repo')}</dt>
          <dd className="flex-1 space-y-1.5">
            <div className="flex items-center gap-2">
              <div className="relative flex-1">
                <input
                  type="text"
                  value={repo}
                  readOnly={locked}
                  onChange={(e) => change(setRepo)(e.target.value)}
                  placeholder={t('projectSettings.localPathPlaceholder', { name: projectId })}
                  className={cn(
                    'w-full rounded-md border px-2.5 py-1.5 font-mono text-xs outline-none transition-colors',
                    locked
                      ? 'border-neutral-200 bg-neutral-50 text-neutral-600 cursor-not-allowed dark:border-zinc-700 dark:bg-zinc-800/60 dark:text-zinc-400'
                      : 'border-neutral-200 bg-transparent text-neutral-800 placeholder:text-neutral-400 focus:border-sky-400 focus:ring-1 focus:ring-sky-400/30 dark:border-zinc-700 dark:text-zinc-200 dark:placeholder:text-zinc-600 dark:focus:border-sky-500'
                  )}
                />
              </div>
              {locked ? (
                <div className="flex items-center gap-2 shrink-0">
                  <span className="inline-flex items-center gap-1 rounded bg-neutral-100 dark:bg-zinc-800 px-2.5 py-1 text-xs font-medium text-neutral-600 dark:text-zinc-300">
                    <Lock className="size-3 text-neutral-500" />
                    {t('projectSettings.repoLocked')}
                  </span>
                  <button
                    type="button"
                    onClick={() => setInitModalOpen(true)}
                    className={cn(
                      'inline-flex items-center gap-1 rounded-md border px-2.5 py-1 text-xs font-medium transition-colors shadow-2xs',
                      initRunning
                        ? 'border-sky-200 bg-sky-50 text-sky-700 hover:bg-sky-100 dark:border-sky-800 dark:bg-sky-950/40 dark:text-sky-300 dark:hover:bg-sky-950/70'
                        : initStatus === 'failed'
                          ? 'border-amber-200 bg-amber-50 text-amber-700 hover:bg-amber-100 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-300 dark:hover:bg-amber-950/70'
                          : 'border-neutral-200 bg-white text-neutral-700 hover:bg-neutral-50 hover:text-sky-600 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700'
                    )}
                  >
                    {initRunning ? <Loader2 className="size-3 animate-spin" /> : <RotateCw className="size-3" />}
                    <span>{initRunning || initStatus === 'failed' ? initButtonLabel : t('projectSettings.reinitialize')}</span>
                  </button>
                  <button
                    type="button"
                    onClick={handleUnlock}
                    title={t('projectSettings.unlock')}
                    className="inline-flex items-center gap-1 rounded px-1.5 py-1 text-xs text-neutral-400 hover:text-neutral-600 dark:hover:text-zinc-300"
                  >
                    <Unlock className="size-3.5" />
                    <span>{t('projectSettings.unlock')}</span>
                  </button>
                </div>
              ) : (
                <button
                  type="button"
                  onClick={() => setInitModalOpen(true)}
                  className={cn(
                    'shrink-0 rounded-md px-3 py-1.5 text-xs font-medium text-white transition-colors shadow-xs',
                    initRunning ? 'bg-sky-600 hover:bg-sky-700' : 'bg-sky-600 hover:bg-sky-700',
                  )}
                >
                  <span className="inline-flex items-center gap-1.5">
                    {initRunning && <Loader2 className="size-3 animate-spin" />}
                    {initRunning ? initButtonLabel : t('projectSettings.initWorkspace')}
                  </span>
                </button>
              )}
            </div>
            {!locked && (
              <p className="text-[11px] text-neutral-400 dark:text-zinc-500">
                {t('projectSettings.localPathPlaceholder', { name: projectId })}
              </p>
            )}
          </dd>
        </div>

        {/* Runtime profile — declares the managed runtime capability for
            every agent sandbox and preview in this project. Explicitly sent
            on save so a change to "auto" is a deliberate clear. */}
        <div className="flex items-start gap-4 px-5 py-2.5">
          <dt className="w-32 shrink-0 pt-1.5 text-xs font-medium text-neutral-500 dark:text-zinc-500">
            {t('projectSettings.runtimeProfile')}
          </dt>
          <dd className="flex-1 space-y-1.5">
            <select
              value={runtimeProfile}
              onChange={(e) => change(setRuntimeProfile)(e.target.value)}
              className="w-full max-w-xs rounded-md border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-900 outline-none focus:border-sky-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
            >
              <option value="">{t('projectSettings.runtimeProfileAuto')}</option>
              <option value="base">{t('projectSettings.runtimeProfileBase')}</option>
              <option value="jvm21">{t('projectSettings.runtimeProfileJvm21')}</option>
            </select>
            <p className="text-[11px] text-neutral-400 dark:text-zinc-500">
              {t('projectSettings.runtimeProfileHint')}
            </p>
          </dd>
        </div>

        {/* Remote code host info if bound */}
        {remoteUrl && (
          <div className="flex items-center justify-between gap-4 px-5 py-3 bg-sky-50/40 dark:bg-sky-950/20">
            <dt className="w-32 shrink-0 text-xs font-medium text-sky-800 dark:text-sky-300 flex items-center gap-1.5">
              <FolderGit2 className="size-3.5" />
              <span>{t('projectSettings.remoteRepo', { defaultValue: '远程代码仓库' })}</span>
            </dt>
            <dd className="flex-1 flex items-center justify-between gap-3">
              <div className="min-w-0">
                <span className="font-mono text-xs text-neutral-700 dark:text-zinc-300 truncate block">
                  {remoteUrl}
                </span>
                {cloneUrl && (
                  <span className="font-mono text-[10px] text-neutral-400 dark:text-zinc-500 truncate block mt-0.5">
                    git clone {cloneUrl}
                  </span>
                )}
              </div>
              <a
                href={remoteUrl}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1.5 rounded bg-white px-2.5 py-1 text-[11px] font-medium text-sky-600 border border-sky-200 hover:bg-sky-50 shadow-xs dark:bg-zinc-800 dark:border-zinc-700 dark:text-sky-400 shrink-0"
              >
                <span>{remoteProvider === 'gitlab' ? t('projectSettings.viewOnGitlab', { defaultValue: '在 GitLab 查看' }) : t('projectSettings.viewOnRemote', { defaultValue: '在远程查看' })}</span>
                <ArrowRight className="size-3" />
              </a>
            </dd>
          </div>
        )}

        {/* Reserved deploy port (platform port pool, mirrored to GitLab APP_PORT) */}
        {Boolean(deployPort) && (
          <div className="flex items-center justify-between gap-4 px-5 py-3 bg-emerald-50/40 dark:bg-emerald-950/20">
            <dt className="w-32 shrink-0 text-xs font-medium text-emerald-800 dark:text-emerald-300 flex items-center gap-1.5">
              <Globe className="size-3.5" />
              <span>{t('projectSettings.deployPort', { defaultValue: '部署端口' })}</span>
            </dt>
            <dd className="flex-1 min-w-0">
              <span className="font-mono text-xs text-neutral-700 dark:text-zinc-300">{deployPort}</span>
              <span className="ml-2 text-[10px] text-neutral-400 dark:text-zinc-500">
                {t('projectSettings.deployPortHint', { defaultValue: '平台端口池自动预留，已同步为 GitLab CI 变量 APP_PORT' })}
              </span>
            </dd>
          </div>
        )}
      </dl>

      <InitializeProjectModal
        projectId={projectId}
        currentDescription={description}
        currentRepo={repo}
        defaultRepo={defaultRepo}
        existingRemoteProvider={remoteProvider}
        existingRemoteConnection={remoteConnection}
        existingRemoteProjectId={remoteProjectId}
        existingRemoteUrl={remoteUrl}
        existingCloneUrl={cloneUrl}
        existingDefaultBranch={defaultBranch}
        existingInitializationTaskId={
          initializationStatus.task?.id && initializationStatus.status !== 'completed'
            ? initializationStatus.task.id
            : undefined
        }
        existingInitializationStatus={initializationStatus.status}
        isOpen={initModalOpen}
        onClose={() => setInitModalOpen(false)}
        onSuccess={handleInitSuccess}
      />
    </section>
  )
}

const TEMPLATES = [
  { id: 'react_go_fullstack', label: 'projectSettings.templateReactGo', icon: Layers },
  { id: 'react_spring_boot', label: 'projectSettings.templateReactSpringBoot', icon: Layers },
  { id: 'tauri_desktop', label: 'projectSettings.templateTauri', icon: Laptop },
  { id: 'react_vite', label: 'projectSettings.templateReactVite', icon: Globe },
  { id: 'go_api', label: 'projectSettings.templateGoApi', icon: Zap },
  { id: 'python_fastapi', label: 'projectSettings.templateFastApi', icon: Sparkles },
  { id: 'blank', label: 'projectSettings.templateBlank', icon: FileText },
]

type InitializationWorkflow = {
  definition: { steps: Array<{ id: string; title: string }> }
  run: { status: string; activeStepId?: string }
  steps: Array<{ stepId: string; status: string; summary?: string }>
}

type ProjectInitializationStatus = {
  status: string
  task?: { id: string; status?: string; updatedAt?: string }
}

function isInitializationRunning(status?: string) {
  return ['queued', 'active', 'pending', 'in_progress'].includes(status || '')
}

function InitializeProjectModal({
  projectId,
  currentDescription,
  currentRepo,
  defaultRepo,
  existingRemoteProvider,
  existingRemoteConnection,
  existingRemoteProjectId,
  existingRemoteUrl,
  existingCloneUrl,
  existingDefaultBranch,
  existingInitializationTaskId,
  existingInitializationStatus,
  isOpen,
  onClose,
  onSuccess,
}: {
  projectId: string
  currentDescription: string
  currentRepo: string
  defaultRepo?: string
  existingRemoteProvider?: string
  existingRemoteConnection?: string
  existingRemoteProjectId?: string
  existingRemoteUrl?: string
  existingCloneUrl?: string
  existingDefaultBranch?: string
  existingInitializationTaskId?: string
  existingInitializationStatus?: string
  isOpen: boolean
  onClose: () => void
  onSuccess: (newRepo: string) => void
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const [activeTab, setActiveTab] = useState<'bind_existing' | 'create_new'>('bind_existing')
  const [existingType, setExistingType] = useState<'local' | 'remote'>('remote')
  const fallbackRepo = defaultRepo?.trim() || `/opt/multigent/data/projects/${projectId}/workspace`
  const [localPath, setLocalPath] = useState(currentRepo.trim() || fallbackRepo)
  const [remoteUrl, setRemoteUrl] = useState('')
  const [useTemplate, setUseTemplate] = useState(true)
  const [selectedTemplate, setSelectedTemplate] = useState('react_go_fullstack')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // GitLab remote creation state
  const [gitlabStatus, setGitlabStatus] = useState<{ connected: boolean; connectionId?: string }>({ connected: false })
  const [gitlabNamespaces, setGitlabNamespaces] = useState<Array<{ id: number; name: string; fullPath: string; kind: string }>>([])
  const [syncToGitLab, setSyncToGitLab] = useState(true)
  const [selectedNamespaceId, setSelectedNamespaceId] = useState<number | null>(null)
  const [repoSlug, setRepoSlug] = useState(projectId)
  const [visibility, setVisibility] = useState<'private' | 'internal' | 'public'>('private')

  const [agents, setAgents] = useState<Array<{ name: string; displayName?: string; model?: string }>>([])
  const [selectedAgent, setSelectedAgent] = useState<string>('')
  const [loadingAgents, setLoadingAgents] = useState(false)
  const [initializationTaskId, setInitializationTaskId] = useState<string | null>(null)
  const [initializationRepo, setInitializationRepo] = useState('')
  const [initializationWorkflow, setInitializationWorkflow] = useState<InitializationWorkflow | null>(null)
  const [initializationLoadError, setInitializationLoadError] = useState<string | null>(null)

  // Sync localPath, fetch agents & check GitLab connection status whenever modal opens
  useEffect(() => {
    if (isOpen) {
      const legacyDefaultRepo = `/opt/multigent/data/projects/${projectId}/workspace`
      setLocalPath(currentRepo.trim() && currentRepo.trim() !== legacyDefaultRepo ? currentRepo.trim() : fallbackRepo)
      setRepoSlug(projectId)
      setError(null)
      setLoadingAgents(true)
      setInitializationTaskId(
        existingInitializationTaskId && existingInitializationStatus !== 'completed'
          ? existingInitializationTaskId
          : null,
      )
      setInitializationRepo('')
      setInitializationWorkflow(null)
      setInitializationLoadError(null)

      // Initialization is project-scoped: only explicitly assigned project
      // members may execute it. Never leak workspace-global agents here.
      apiFetch<Array<{ name?: string; displayName?: string; model?: string }>>(`/api/v1/projects/${encodeURIComponent(projectId)}/agents`)
        .then((projAgentsRes) => {
          const projList = Array.isArray(projAgentsRes) ? projAgentsRes : []
          return projList.filter((w) => w.name && w.model !== 'human')
        })
        .then((availableWorkers) => {
          setAgents(availableWorkers as Array<{ name: string; displayName?: string; model?: string }>)
          if (availableWorkers.length > 0) {
            setSelectedAgent(availableWorkers[0].name || '')
          } else {
            setSelectedAgent('')
          }
        })
        .catch(() => {
          setAgents([])
          setSelectedAgent('')
        })
        .finally(() => {
          setLoadingAgents(false)
        })

      // Check GitLab connection status & fetch namespaces
      apiFetch<{ connected: boolean; connectionId?: string }>('/api/v1/integrations/gitlab/status')
        .then((st) => {
          setGitlabStatus(st)
          if (st.connected) {
            return apiFetch<{ ok: boolean; namespaces: Array<{ id: number; name: string; fullPath: string; kind: string }> }>('/api/v1/integrations/gitlab/namespaces')
              .then((nsRes) => {
                const list = nsRes.namespaces || []
                setGitlabNamespaces(list)
                if (list.length > 0) {
                  setSelectedNamespaceId(list[0].id)
                }
              })
          }
        })
        .catch(() => {
          setGitlabStatus({ connected: false })
        })
    }
  }, [isOpen, currentRepo, projectId, fallbackRepo])

  // The initialization task owns the durable state. Polling is deliberately
  // used here instead of a second socket protocol; the task follow page can
  // take over with the same workflow data if this modal is closed.
  useEffect(() => {
    if (!isOpen || !initializationTaskId) return
    let cancelled = false
    let attempts = 0
    let timer: ReturnType<typeof setTimeout> | undefined

    const poll = async () => {
      try {
        const data = await apiFetch<InitializationWorkflow>(
          `/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(initializationTaskId)}/workflow`,
          { silentStatuses: [404] },
        )
        if (cancelled) return
        attempts = 0
        setInitializationWorkflow(data)
        setInitializationLoadError(null)
        if (!['completed', 'failed', 'cancelled'].includes(data.run.status)) {
          timer = setTimeout(poll, 1500)
        }
      } catch (err) {
        if (cancelled) return
        attempts += 1
        if (attempts >= 5) {
          setInitializationLoadError(err instanceof Error ? err.message : String(err))
        }
        timer = setTimeout(poll, 1500)
      }
    }

    void poll()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [initializationTaskId, isOpen, projectId])

  if (!isOpen) return null

  async function handleConfirm() {
    setBusy(true)
    setError(null)
    try {
      if (!selectedAgent) {
        setError(t('projectSettings.noAgentWarningTitle'))
        setBusy(false)
        return
      }

      // Older clients persisted this path before the server exposed the real
      // workspace root. Treat it as empty so retries migrate to defaultRepo.
      const legacyDefaultRepo = `/opt/multigent/data/projects/${projectId}/workspace`
      const persistedRepo = currentRepo.trim() === legacyDefaultRepo ? '' : currentRepo.trim()
      const defaultProjectWorkspace = persistedRepo || fallbackRepo
      const targetRepo =
        activeTab === 'bind_existing' && existingType === 'local' && localPath.trim()
          ? localPath.trim()
          : defaultProjectWorkspace

      let remoteMetadata: any = {}
      if (activeTab === 'create_new' && syncToGitLab && gitlabStatus.connected) {
        const requestedSlug = repoSlug.trim() || projectId
        const canReuseRemote = existingRemoteProvider === 'gitlab'
          && existingRemoteConnection === gitlabStatus.connectionId
          && Boolean(existingRemoteProjectId && existingRemoteUrl && existingCloneUrl)
          && requestedSlug === projectId
        if (canReuseRemote) {
          // A previous attempt may have created the remote before failing in a
          // later step. Reuse it instead of turning a retry into duplicate 409.
          remoteMetadata = {
            remoteProvider: 'gitlab',
            remoteConnection: existingRemoteConnection,
            remoteProjectId: existingRemoteProjectId,
            remoteUrl: existingRemoteUrl,
            cloneUrl: existingCloneUrl,
            defaultBranch: existingDefaultBranch || 'main',
          }
        } else {
          const createRes = await apiPost<{ ok: boolean; connectionId: string; repository: { id: string; name: string; webUrl: string; httpCloneUrl: string; sshCloneUrl: string; defaultBranch: string } }>('/api/v1/integrations/gitlab/projects', {
            connectionId: gitlabStatus.connectionId,
            name: requestedSlug,
            path: requestedSlug,
            namespaceId: selectedNamespaceId || 0,
            visibility: visibility,
            description: currentDescription || `Repository for ${projectId}`,
          })
          if (createRes && createRes.repository) {
            remoteMetadata = {
              remoteProvider: 'gitlab',
              remoteConnection: createRes.connectionId,
              remoteProjectId: createRes.repository.id,
              remoteUrl: createRes.repository.webUrl,
              cloneUrl: createRes.repository.httpCloneUrl,
              defaultBranch: createRes.repository.defaultBranch || 'main',
            }
          }
        }
      }

      // 1. Update project repo and remote metadata in backend
      await apiPut(`/api/v1/projects/${encodeURIComponent(projectId)}`, {
        description: currentDescription,
        repo: targetRepo,
        ...remoteMetadata,
      })

      // 2. Ensure the selected workspace agent is bound to this project.
      // The API upserts memberships, so this is safe when re-initializing.
      await apiPost(
        `/api/v1/projects/${encodeURIComponent(projectId)}/memberships`,
        { workerName: selectedAgent }
      )

      // The initialization task needs the GitLab credential helper at runtime.
      // This keeps the remote URL clean and scoped to the selected project.
      if (remoteMetadata.remoteProvider === 'gitlab') {
        await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/tool-bindings/install`, {
          connectionId: remoteMetadata.remoteConnection,
          adapterType: 'cli',
        })
      }

      // React + Go and React + Spring Boot are deterministic project templates. The
      // server writes their fixed files before the Agent task starts; the Agent
      // then installs dependencies, verifies the result and handles Git.
      const deterministicTemplate =
        activeTab === 'create_new' &&
        useTemplate &&
        (selectedTemplate === 'react_go_fullstack' || selectedTemplate === 'react_spring_boot')
      if (deterministicTemplate) {
        await apiPost<{ templateId: string; templateVersion: string; templateDigest: string }>(
          `/api/v1/projects/${encodeURIComponent(projectId)}/initialize-template`,
          { repo: targetRepo, templateId: selectedTemplate, agent: selectedAgent },
        )
      }

      // 3. Prepare task payload & dispatch
      let taskTitle = ''
      let taskPrompt = ''
      const cleanCloneUrl = remoteMetadata.cloneUrl

      if (activeTab === 'bind_existing') {
        if (existingType === 'remote') {
          taskTitle = t('projectSettings.initTaskTitleClone', { defaultValue: '【工程初始化】克隆远程仓库并检查就绪' })
        } else {
          taskTitle = t('projectSettings.initTaskTitleBind', { defaultValue: '【工程初始化】绑定并校验本地工作区' })
        }
      } else {
        // Create new
        const tpl = TEMPLATES.find((x) => x.id === selectedTemplate)
        const tplName = (useTemplate && tpl) ? t(tpl.label) : t('projectSettings.initTemplateBlankName', { defaultValue: '基础空白' })

        if (cleanCloneUrl) {
          taskTitle = t('projectSettings.initTaskTitleScaffold', { name: tplName, defaultValue: '【工程初始化】构建 {{name}} 脚手架并首推远程 GitLab' })
        } else {
          taskTitle = t('projectSettings.initTaskTitleTemplate', { name: tplName, defaultValue: '【工程初始化】构建 {{name}} 模板脚手架' })
        }
      }

      // 4. Create and directly start the persisted initialization workflow.
      const resolvedRemote = (activeTab === 'bind_existing' && existingType === 'remote' ? remoteUrl.trim() : cleanCloneUrl) || 'none'
      const initializationRequest = [
        `mode=${activeTab === 'bind_existing' ? `bind_${existingType}` : 'create_new'}`,
        `repo=/workspace (host: ${targetRepo})`,
        `template=${activeTab === 'create_new' && useTemplate ? selectedTemplate : 'none'}`,
        `remote=${resolvedRemote}`,
      ].join('; ')
      // Keep the root prompt as context only. The workflow step descriptions
      // are the executable contract; repeating the whole procedure here
      // would encourage the agent to skip the persisted stage boundaries.
      taskPrompt = `initialization_request: ${initializationRequest}\n\n当前项目工作区已挂载至沙箱 /workspace。请按「项目初始化」工作流逐阶段执行当前初始化任务。只完成当前阶段并使用 workflow step done 汇报结构化结果；不要跳过失败阶段或把未验证的状态报告为完成。`

      const createdTask = await apiPost<{ id: string }>(`/api/v1/projects/${encodeURIComponent(projectId)}/tasks`, {
        agent: selectedAgent,
        title: taskTitle,
        description: activeTab === 'bind_existing'
          ? t('projectSettings.initTaskDescBind', { defaultValue: '自动化工程初始化 (已有仓库)' })
          : t('projectSettings.initTaskDescCreate', { defaultValue: '自动化工程初始化 (从零新建)' }),
        prompt: taskPrompt,
        type: 'chore',
        priority: 3,
        labels: ['project-initialization'],
        workflowDefinitionId: 'project-initialization-v1',
        workflowActorBindings: {
          'project-initializer': { type: 'agent', id: selectedAgent },
        },
        vars: {
          initialization_mode: activeTab === 'bind_existing' ? `bind_${existingType}` : 'create_new',
          initialization_repo: targetRepo,
          initialization_template: activeTab === 'create_new' && useTemplate ? selectedTemplate : 'none',
        },
        autoStart: true,
      })

      if (!createdTask?.id) throw new Error(t('projectSettings.initErrorNoTaskId', { defaultValue: '初始化任务创建成功但未返回任务 ID' }))
      setInitializationRepo(targetRepo)
      setInitializationTaskId(createdTask.id)
      showToast(t('projectSettings.initSuccess', { defaultValue: '工程初始化任务已创建并启动！' }), 'success')
    } catch (err) {
      setError(err instanceof ApiError && err.serverMessage
        ? err.serverMessage
        : err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  async function retryInitialization() {
    if (!initializationTaskId) return
    setBusy(true)
    setInitializationLoadError(null)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(initializationTaskId)}/start`, {}, { suppressToast: true })
      setInitializationWorkflow((current) => current ? {
        ...current,
        run: { ...current.run, status: 'active' },
        steps: current.steps.map((step) => step.stepId === current.run.activeStepId ? { ...step, status: 'pending', summary: '' } : step),
      } : current)
    } catch (err) {
      setInitializationLoadError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-neutral-900/60 p-4 backdrop-blur-sm">
      <div className="flex w-full max-w-xl flex-col rounded-xl border border-neutral-200 bg-white shadow-2xl dark:border-zinc-800 dark:bg-zinc-900 overflow-hidden animate-fade-in">
        {/* Header */}
        <div className="flex items-center justify-between border-b border-neutral-100 px-6 py-4 dark:border-zinc-800">
          <div className="flex items-center gap-2.5">
            <div className="flex size-8 items-center justify-center rounded-lg bg-sky-100 text-sky-600 dark:bg-sky-950/60 dark:text-sky-400">
              <FolderGit2 className="size-4" />
            </div>
            <div>
              <h2 className="text-base font-semibold text-neutral-900 dark:text-zinc-100">
                {t('projectSettings.initModalTitle')}
              </h2>
              <p className="text-xs text-neutral-500 dark:text-zinc-400">
                {t('projectSettings.name')}: <span className="font-mono font-medium text-neutral-700 dark:text-zinc-300">{projectId}</span>
              </p>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            disabled={Boolean(initializationTaskId && !['completed', 'failed', 'cancelled'].includes(initializationWorkflow?.run.status || ''))}
            className="rounded-lg p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
          >
            <X className="size-4" />
          </button>
        </div>

        {/* Tab Header */}
        <div className="grid grid-cols-2 border-b border-neutral-200 bg-neutral-50/70 p-1 dark:border-zinc-800 dark:bg-zinc-950/40">
          <button
            type="button"
            onClick={() => setActiveTab('bind_existing')}
            className={cn(
              'flex items-center justify-center gap-2 rounded-lg py-2.5 text-xs font-medium transition-all',
              activeTab === 'bind_existing'
                ? 'bg-white text-sky-600 shadow-sm dark:bg-zinc-900 dark:text-sky-400 font-semibold'
                : 'text-neutral-500 hover:text-neutral-800 dark:text-zinc-400 dark:hover:text-zinc-200'
            )}
          >
            <FolderGit2 className="size-4" />
            <span>{t('projectSettings.tabBindExisting')}</span>
          </button>
          <button
            type="button"
            onClick={() => setActiveTab('create_new')}
            className={cn(
              'flex items-center justify-center gap-2 rounded-lg py-2.5 text-xs font-medium transition-all',
              activeTab === 'create_new'
                ? 'bg-white text-sky-600 shadow-sm dark:bg-zinc-900 dark:text-sky-400 font-semibold'
                : 'text-neutral-500 hover:text-neutral-800 dark:text-zinc-400 dark:hover:text-zinc-200'
            )}
          >
            <Sparkles className="size-4" />
            <span>{t('projectSettings.tabCreateNew')}</span>
          </button>
        </div>

        {/* Modal Body */}
        <div className="space-y-4 px-6 py-5">
          {initializationTaskId ? (
            <InitializationProgress
              workflow={initializationWorkflow}
              loadError={initializationLoadError}
              taskId={initializationTaskId}
              onViewTask={() => navigate(`/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(initializationTaskId)}/follow`)}
            />
          ) : <>
          {error && (
            <div className="rounded-lg border border-red-200 bg-red-50 p-3 text-xs text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
              {error}
            </div>
          )}

          {activeTab === 'bind_existing' && (
            <div className="space-y-4">
              {/* Equal Segmented Control */}
              <div className="grid grid-cols-2 gap-1 rounded-lg bg-neutral-100 p-1 dark:bg-zinc-800/80 border border-neutral-200/60 dark:border-zinc-700/60">
                <button
                  type="button"
                  onClick={() => setExistingType('local')}
                  className={cn(
                    'flex items-center justify-center gap-2 rounded-md py-2 text-xs font-medium transition-all cursor-pointer',
                    existingType === 'local'
                      ? 'bg-white text-sky-600 shadow-sm dark:bg-zinc-900 dark:text-sky-400 font-semibold'
                      : 'text-neutral-500 hover:text-neutral-800 dark:text-zinc-400 dark:hover:text-zinc-200'
                  )}
                >
                  <HardDrive className="size-3.5" />
                  <span>{t('projectSettings.switchLocal')}</span>
                </button>
                <button
                  type="button"
                  onClick={() => setExistingType('remote')}
                  className={cn(
                    'flex items-center justify-center gap-2 rounded-md py-2 text-xs font-medium transition-all cursor-pointer',
                    existingType === 'remote'
                      ? 'bg-white text-sky-600 shadow-sm dark:bg-zinc-900 dark:text-sky-400 font-semibold'
                      : 'text-neutral-500 hover:text-neutral-800 dark:text-zinc-400 dark:hover:text-zinc-200'
                  )}
                >
                  <GitBranch className="size-3.5" />
                  <span>{t('projectSettings.switchRemote')}</span>
                </button>
              </div>

              {existingType === 'local' ? (
                <div className="space-y-1.5">
                  <label className="block text-xs font-medium text-neutral-700 dark:text-zinc-300">
                    {t('projectSettings.localPathLabel')}
                  </label>
                  <input
                    type="text"
                    value={localPath}
                    onChange={(e) => setLocalPath(e.target.value)}
                    placeholder={t('projectSettings.localPathPlaceholder', { name: projectId })}
                    className="w-full rounded-lg border border-neutral-200 bg-white px-3 py-2 font-mono text-xs text-neutral-900 outline-none transition-colors focus:border-sky-400 focus:ring-1 focus:ring-sky-400/30 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                  />
                  <p className="text-[11px] text-neutral-400 dark:text-zinc-500">
                    {t('projectSettings.localPathPlaceholder', { name: projectId })}
                  </p>
                </div>
              ) : (
                <div className="space-y-3">
                  <div className="space-y-1.5">
                    <label className="block text-xs font-medium text-neutral-700 dark:text-zinc-300">
                      {t('projectSettings.remoteUrlLabel')}
                    </label>
                    <input
                      type="text"
                      value={remoteUrl}
                      onChange={(e) => setRemoteUrl(e.target.value)}
                      placeholder={t('projectSettings.remoteUrlPlaceholder')}
                      className="w-full rounded-lg border border-neutral-200 bg-white px-3 py-2 font-mono text-xs text-neutral-900 outline-none transition-colors focus:border-sky-400 focus:ring-1 focus:ring-sky-400/30 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                    />
                  </div>

                  {/* Warning banner */}
                  <div className="flex gap-2.5 rounded-lg border border-amber-200/80 bg-amber-50/80 p-3 text-xs leading-relaxed text-amber-800 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-300">
                    <AlertTriangle className="size-4 text-amber-600 dark:text-amber-400 shrink-0 mt-0.5" />
                    <div>
                      <p className="font-semibold text-amber-900 dark:text-amber-200">
                        {t('projectSettings.remoteWarningTitle')}
                      </p>
                      <p className="mt-0.5">{t('projectSettings.remoteWarningDesc')}</p>
                    </div>
                  </div>
                </div>
              )}
            </div>
          )}

          {activeTab === 'create_new' && (
            <div className="space-y-4">
              <label className="flex items-center gap-2 cursor-pointer select-none">
                <input
                  type="checkbox"
                  checked={useTemplate}
                  onChange={(e) => setUseTemplate(e.target.checked)}
                  className="size-4 rounded border-neutral-300 text-sky-600 focus:ring-sky-500 dark:border-zinc-700 dark:bg-zinc-950"
                />
                <span className="text-xs font-semibold text-neutral-800 dark:text-zinc-200">
                  {t('projectSettings.useTemplate')}
                </span>
              </label>

              {useTemplate && (
                <div className="space-y-2">
                  <label className="block text-xs font-medium text-neutral-700 dark:text-zinc-300">
                    {t('projectSettings.templateTypeLabel')}
                  </label>
                  <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                    {TEMPLATES.map((tpl) => {
                      const Icon = tpl.icon
                      const isSelected = selectedTemplate === tpl.id
                      return (
                        <button
                          key={tpl.id}
                          type="button"
                          onClick={() => setSelectedTemplate(tpl.id)}
                          className={cn(
                            'flex items-start gap-2.5 rounded-lg border p-3 text-left transition-all cursor-pointer',
                            isSelected
                              ? 'border-sky-500 bg-sky-50/60 shadow-sm dark:border-sky-500 dark:bg-sky-950/30 ring-1 ring-sky-500'
                              : 'border-neutral-200 bg-white hover:border-neutral-300 dark:border-zinc-700 dark:bg-zinc-950 dark:hover:border-zinc-600'
                          )}
                        >
                          <div className={cn(
                            'flex size-7 shrink-0 items-center justify-center rounded-md',
                            isSelected ? 'bg-sky-600 text-white' : 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-300'
                          )}>
                            <Icon className="size-3.5" />
                          </div>
                          <div className="min-w-0">
                            <p className={cn('text-xs font-medium truncate', isSelected ? 'text-sky-900 dark:text-sky-100 font-semibold' : 'text-neutral-800 dark:text-zinc-200')}>
                              {t(tpl.label)}
                            </p>
                          </div>
                        </button>
                      )
                    })}
                  </div>
                </div>
              )}

              {/* GitLab Remote Repository Configuration */}
              {gitlabStatus.connected && (
                <div className="rounded-lg border border-sky-200 bg-sky-50/50 p-4 dark:border-sky-900/60 dark:bg-sky-950/20 space-y-3">
                  <label className="flex items-center gap-2 cursor-pointer select-none">
                    <input
                      type="checkbox"
                      checked={syncToGitLab}
                      onChange={(e) => setSyncToGitLab(e.target.checked)}
                      className="size-4 rounded border-neutral-300 text-sky-600 focus:ring-sky-500 dark:border-zinc-700 dark:bg-zinc-950"
                    />
                    <span className="text-xs font-semibold text-sky-900 dark:text-sky-200">
                      {t('projectSettings.initCreateRadio', { defaultValue: '同步在 GitLab 上自动创建远程仓库并绑定 (推荐)' })}
                    </span>
                  </label>

                  {syncToGitLab && (
                    <div className="space-y-3 pt-1">
                      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                        <label className="block">
                          <span className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                            {t('projectSettings.initNamespace', { defaultValue: '命名空间 (Namespace / Group)' })}
                          </span>
                          <select
                            value={selectedNamespaceId || ''}
                            onChange={(e) => setSelectedNamespaceId(Number(e.target.value))}
                            className="mt-1 w-full rounded-md border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-900 outline-none focus:border-sky-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                          >
                            {gitlabNamespaces.map((ns) => (
                              <option key={ns.id} value={ns.id}>
                                {ns.fullPath} ({ns.kind === 'user' ? t('projectSettings.nsKindUser', { defaultValue: '个人' }) : t('projectSettings.nsKindGroup', { defaultValue: '团队' })})
                              </option>
                            ))}
                          </select>
                        </label>

                        <label className="block">
                          <span className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                            {t('projectSettings.initRepoSlug', { defaultValue: '仓库路径 (Repository Slug)' })}
                          </span>
                          <input
                            type="text"
                            value={repoSlug}
                            onChange={(e) => setRepoSlug(e.target.value)}
                            placeholder={projectId}
                            className="mt-1 w-full rounded-md border border-neutral-200 bg-white px-2.5 py-1.5 font-mono text-xs text-neutral-900 outline-none focus:border-sky-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                          />
                        </label>
                      </div>

                      <div className="flex items-center gap-4">
                        <span className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                          {t('projectSettings.initVisibility', { defaultValue: '可见性:' })}
                        </span>
                        <label className="flex items-center gap-1.5 cursor-pointer text-xs text-neutral-700 dark:text-zinc-300">
                          <input
                            type="radio"
                            name="visibility"
                            value="private"
                            checked={visibility === 'private'}
                            onChange={() => setVisibility('private')}
                            className="size-3.5 text-sky-600 focus:ring-sky-500"
                          />
                          <span>{t('projectSettings.visPrivate', { defaultValue: '私有 (Private)' })}</span>
                        </label>
                        <label className="flex items-center gap-1.5 cursor-pointer text-xs text-neutral-700 dark:text-zinc-300">
                          <input
                            type="radio"
                            name="visibility"
                            value="internal"
                            checked={visibility === 'internal'}
                            onChange={() => setVisibility('internal')}
                            className="size-3.5 text-sky-600 focus:ring-sky-500"
                          />
                          <span>{t('projectSettings.visInternal', { defaultValue: '内部 (Internal)' })}</span>
                        </label>
                      </div>
                    </div>
                  )}
                </div>
              )}
            </div>
          )}

          {/* Assigned Agent Selector & Validation */}
          {!loadingAgents && agents.length === 0 ? (
            <div className="flex items-start gap-2.5 rounded-lg border border-amber-300 bg-amber-50 p-3.5 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
              <AlertTriangle className="size-4 text-amber-600 dark:text-amber-400 shrink-0 mt-0.5" />
              <div className="flex-1 min-w-0">
                <p className="font-semibold">{t('projectSettings.noAgentWarningTitle')}</p>
                <p className="mt-1 text-[11px] text-amber-700 dark:text-amber-300 leading-relaxed">
                  {t('projectSettings.noAgentWarningDesc')}
                </p>
                <div className="mt-3">
                  <Link
                    to={`/projects/${encodeURIComponent(projectId)}/members`}
                    onClick={onClose}
                    className="inline-flex items-center gap-1.5 rounded-md bg-amber-600 px-3 py-1.5 text-xs font-medium text-white shadow-xs hover:bg-amber-700 transition-colors"
                  >
                    <Users className="size-3.5" />
                    <span>{t('projectSettings.goToMembers')}</span>
                  </Link>
                </div>
              </div>
            </div>
          ) : (
            <div className="space-y-1.5 pt-2 border-t border-neutral-100 dark:border-zinc-800">
              <label className="block text-xs font-medium text-neutral-700 dark:text-zinc-300">
                {t('projectSettings.assignedAgentLabel')}
              </label>
              <select
                value={selectedAgent}
                onChange={(e) => setSelectedAgent(e.target.value)}
                className="w-full rounded-lg border border-neutral-200 bg-white px-3 py-2 text-xs text-neutral-900 outline-none transition-colors focus:border-sky-400 focus:ring-1 focus:ring-sky-400/30 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
              >
                {agents.map((a) => (
                  <option key={a.name} value={a.name}>
                    {a.displayName ? `${a.displayName} (${a.name})` : a.name} - {a.model || 'claudecode'}
                  </option>
                ))}
              </select>
            </div>
          )}
          </>}
        </div>

        {/* Footer */}
        <div className="flex items-center justify-end gap-2.5 border-t border-neutral-100 bg-neutral-50/50 px-6 py-3.5 dark:border-zinc-800 dark:bg-zinc-950/30">
          {initializationTaskId ? (
            <InitializationProgressActions
              workflow={initializationWorkflow}
              busy={busy}
              onClose={() => onSuccess(initializationRepo || localPath.trim() || fallbackRepo)}
              onRetry={() => void retryInitialization()}
              onViewTask={() => navigate(`/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(initializationTaskId)}/follow`)}
            />
          ) : <>
          <button
            type="button"
            onClick={onClose}
            disabled={busy}
            className="rounded-lg border border-neutral-200 bg-white px-3.5 py-1.5 text-xs font-medium text-neutral-700 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800 cursor-pointer"
          >
            {t('common.cancel')}
          </button>
          <button
            type="button"
            onClick={() => void handleConfirm()}
            disabled={busy || agents.length === 0 || (activeTab === 'bind_existing' && existingType === 'remote' && !remoteUrl.trim())}
            className="rounded-lg bg-sky-600 px-4 py-1.5 text-xs font-medium text-white shadow-sm hover:bg-sky-700 disabled:opacity-50 transition-colors cursor-pointer"
          >
            <span>
              {agents.length === 0 && !loadingAgents
                ? t('projectSettings.pleaseConfigureAgentFirst')
                : busy
                ? t('projectSettings.initializing')
                : t('projectSettings.confirmInit')}
            </span>
          </button>
          </>}
        </div>
      </div>
    </div>
  )
}

function InitializationProgress({
  workflow,
  loadError,
  taskId,
  onViewTask,
}: {
  workflow: InitializationWorkflow | null
  loadError: string | null
  taskId: string
  onViewTask: () => void
}) {
  const { t } = useTranslation()
  const fallbackSteps = [
    { id: 'prepare', title: t('projectSettings.initStepPrepare') },
    { id: 'dependencies', title: t('projectSettings.initStepDependencies') },
    { id: 'verify', title: t('projectSettings.initStepVerify') },
    { id: 'health', title: t('projectSettings.initStepHealth') },
    { id: 'sync', title: t('projectSettings.initStepSync') },
  ]
  const steps = (workflow?.definition.steps ?? fallbackSteps).map((step) => ({
    ...step,
    title: t(`projectSettings.initStep${step.id.charAt(0).toUpperCase()}${step.id.slice(1)}`, { defaultValue: step.title }),
  }))
  const statusFor = (stepID: string) => {
    const status = workflow?.steps.find((step) => step.stepId === stepID)?.status
    if (status === 'completed' || status === 'success') return 'completed'
    if (status === 'failed' || status === 'done_failed') return 'failed'
    if (workflow?.run.activeStepId === stepID) return 'running'
    return status || 'pending'
  }
  const runStatus = workflow?.run.status || 'queued'
  const failed = runStatus === 'failed' || steps.some((step) => statusFor(step.id) === 'failed')
  const completed = runStatus === 'completed'

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
            {completed ? t('projectSettings.initCompleted') : failed ? t('projectSettings.initFailed') : t('projectSettings.initRunning')}
          </h3>
          <p className="mt-1 text-[11px] text-neutral-500 dark:text-zinc-400">{t('projectSettings.initTaskId', { id: taskId })}</p>
        </div>
        <span className={cn(
          'rounded-full px-2.5 py-1 text-[10px] font-semibold',
          completed ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/50 dark:text-emerald-300' :
            failed ? 'bg-red-100 text-red-700 dark:bg-red-950/50 dark:text-red-300' :
              'bg-sky-100 text-sky-700 dark:bg-sky-950/50 dark:text-sky-300',
        )}>
          {completed ? t('projectSettings.initStatusCompleted') : failed ? t('projectSettings.initStatusFailed') : t('projectSettings.initStatusRunning')}
        </span>
      </div>

      <div className="rounded-lg border border-neutral-200 bg-neutral-50/70 p-3 dark:border-zinc-700 dark:bg-zinc-950/40">
        <ol className="space-y-2.5">
          {steps.map((step) => {
            const status = statusFor(step.id)
            return (
              <li key={step.id} className="flex items-center gap-2.5 text-xs">
                {status === 'completed' ? <CheckCircle2 className="size-4 shrink-0 text-emerald-600 dark:text-emerald-400" />
                  : status === 'running' || status === 'in_progress' ? <Loader2 className="size-4 shrink-0 animate-spin text-sky-600 dark:text-sky-400" />
                    : status === 'failed' ? <AlertTriangle className="size-4 shrink-0 text-red-600 dark:text-red-400" />
                      : <span className="size-4 shrink-0 rounded-full border border-neutral-300 dark:border-zinc-600" />}
                <span className={cn(
                  status === 'completed' && 'text-emerald-700 dark:text-emerald-300',
                  (status === 'running' || status === 'in_progress') && 'font-semibold text-sky-700 dark:text-sky-300',
                  status === 'failed' && 'font-semibold text-red-700 dark:text-red-300',
                  !['completed', 'running', 'in_progress', 'failed'].includes(status) && 'text-neutral-500 dark:text-zinc-500',
                )}>{step.title}</span>
                {status === 'failed' && <span className="ml-auto text-[10px] text-red-600 dark:text-red-400">{t('projectSettings.initNeedsRetry')}</span>}
              </li>
            )
          })}
        </ol>
      </div>

      {loadError && !workflow && <p className="text-[11px] text-amber-700 dark:text-amber-300">{t('projectSettings.initProgressUnavailable', { error: loadError })}</p>}
      {failed && workflow && <div className="rounded-lg border border-red-200 bg-red-50 p-3 text-[11px] leading-relaxed text-red-800 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">{t('projectSettings.initFailureHint')}</div>}
      {!workflow && !loadError && <p className="text-center text-xs text-neutral-500 dark:text-zinc-400">{t('projectSettings.initProgressLoading')}</p>}
      {(completed || failed) && <button type="button" onClick={onViewTask} className="text-xs font-medium text-sky-600 hover:underline dark:text-sky-400">{t('projectSettings.initViewTask')}</button>}
    </div>
  )
}

function InitializationProgressActions({
  workflow,
  busy,
  onClose,
  onRetry,
  onViewTask,
}: {
  workflow: InitializationWorkflow | null
  busy: boolean
  onClose: () => void
  onRetry: () => void
  onViewTask: () => void
}) {
  const { t } = useTranslation()
  const terminal = workflow && ['completed', 'failed', 'cancelled'].includes(workflow.run.status)
  return (
    <>
      <button type="button" onClick={onViewTask} className="rounded-lg border border-neutral-200 bg-white px-3.5 py-1.5 text-xs font-medium text-neutral-700 hover:bg-neutral-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800">{t('projectSettings.initViewTask')}</button>
      {terminal && workflow.run.status !== 'completed' && <button type="button" onClick={onRetry} disabled={busy} className="rounded-lg bg-amber-600 px-3.5 py-1.5 text-xs font-medium text-white hover:bg-amber-700 disabled:opacity-50">{busy ? t('projectSettings.initRetrying') : t('projectSettings.initRetry')}</button>}
      {terminal && workflow.run.status === 'completed' && <button type="button" onClick={onClose} className="rounded-lg bg-sky-600 px-4 py-1.5 text-xs font-medium text-white hover:bg-sky-700">{t('common.close')}</button>}
    </>
  )
}

function PromptEditor({ label, apiPath, initialContent }: { label: string; apiPath: string; initialContent: string }) {
  const { t } = useTranslation()
  const [value, setValue] = useState(initialContent)
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [preview, setPreview] = useState(false)
  const [saved, setSaved] = useState(false)

  const save = useCallback(async () => {
    setSaving(true)
    setSaved(false)
    try {
      await apiPut(apiPath, { content: value })
      setDirty(false)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (e) {
      alert(String(e))
    } finally {
      setSaving(false)
    }
  }, [apiPath, value])

  const change = useCallback((v: string) => {
    setValue(v)
    setDirty(true)
    setSaved(false)
  }, [])

  return (
    <section className="rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40">
      <div className="flex items-center justify-between border-b border-neutral-100 px-5 py-3 dark:border-zinc-700/40">
        <div className="flex items-center gap-2">
          <FileText className="size-4 text-neutral-400 dark:text-zinc-500" strokeWidth={1.8} />
          <span className="text-sm font-semibold text-neutral-800 dark:text-zinc-200">{label}</span>
          {dirty && <span className="text-[10px] text-amber-500">●</span>}
          {saved && <span className="text-[10px] text-emerald-500">{t('prompt.saved')}</span>}
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => setPreview((p) => !p)}
            className={cn(
              'rounded-md px-2 py-1 text-[11px] font-medium transition-colors',
              preview
                ? 'bg-sky-100 text-sky-700 dark:bg-sky-900/30 dark:text-sky-400'
                : 'text-neutral-400 hover:text-neutral-600 dark:text-zinc-500 dark:hover:text-zinc-400'
            )}
          >
            {preview ? t('prompt.edit') : t('prompt.preview')}
          </button>
          <button
            type="button"
            onClick={save}
            disabled={saving}
            className="flex items-center gap-1 rounded-md bg-sky-600 px-2.5 py-1 text-[11px] font-medium text-white transition-colors hover:bg-sky-700 disabled:opacity-50"
          >
            <Save className="size-3" strokeWidth={2} />
            {saving ? t('prompt.saving') : t('prompt.save')}
          </button>
        </div>
      </div>
      {preview ? (
        <div className="prose-none max-h-[50vh] overflow-auto p-5 text-sm leading-relaxed text-neutral-800 dark:text-zinc-200">
          <Markdown remarkPlugins={[remarkGfm]}>{value || t('projectSettings.promptEmpty', { defaultValue: '*（空）*' })}</Markdown>
        </div>
      ) : (
        <textarea
          value={value}
          onChange={(e) => change(e.target.value)}
          className="block w-full resize-y bg-transparent p-5 font-mono text-[13px] leading-relaxed text-neutral-800 outline-none placeholder:text-neutral-300 dark:text-zinc-200 dark:placeholder:text-zinc-700"
          rows={Math.max(8, Math.min(24, value.split('\n').length + 1))}
          placeholder="Markdown prompt..."
        />
      )}
    </section>
  )
}
