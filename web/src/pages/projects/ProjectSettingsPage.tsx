import { useCallback, useEffect, useState } from 'react'
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
  Lock,
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
  onReload: () => void
}) {
  const { t } = useTranslation()
  const [description, setDescription] = useState(initialDescription ?? '')
  const [repo, setRepo] = useState(initialRepo ?? '')
  const [locked, setLocked] = useState(Boolean(initialRepo && initialRepo.trim() !== ''))
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [initModalOpen, setInitModalOpen] = useState(false)
  const [toastMessage, setToastMessage] = useState<string | null>(null)

  const save = useCallback(async () => {
    setSaving(true)
    setSaved(false)
    try {
      await apiPut(`/api/v1/projects/${encodeURIComponent(projectId)}`, { description, repo })
      setDirty(false)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (e) {
      alert(String(e))
    } finally {
      setSaving(false)
    }
  }, [projectId, description, repo])

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
                    className="inline-flex items-center gap-1 rounded-md border border-neutral-200 bg-white px-2.5 py-1 text-xs font-medium text-neutral-700 hover:bg-neutral-50 hover:text-sky-600 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700 transition-colors shadow-2xs"
                  >
                    <RotateCw className="size-3" />
                    <span>重新初始化</span>
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
                  className="shrink-0 rounded-md bg-sky-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-sky-700 transition-colors shadow-xs"
                >
                  {t('projectSettings.initWorkspace')}
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

        {/* Remote code host info if bound */}
        {remoteUrl && (
          <div className="flex items-center justify-between gap-4 px-5 py-3 bg-sky-50/40 dark:bg-sky-950/20">
            <dt className="w-32 shrink-0 text-xs font-medium text-sky-800 dark:text-sky-300 flex items-center gap-1.5">
              <FolderGit2 className="size-3.5" />
              <span>远程代码仓库</span>
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
                <span>在 {remoteProvider === 'gitlab' ? 'GitLab' : '远程'} 查看</span>
                <ArrowRight className="size-3" />
              </a>
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
        isOpen={initModalOpen}
        onClose={() => setInitModalOpen(false)}
        onSuccess={handleInitSuccess}
      />
    </section>
  )
}

const TEMPLATES = [
  { id: 'react_go_fullstack', label: 'projectSettings.templateReactGo', icon: Layers },
  { id: 'tauri_desktop', label: 'projectSettings.templateTauri', icon: Laptop },
  { id: 'react_vite', label: 'projectSettings.templateReactVite', icon: Globe },
  { id: 'go_api', label: 'projectSettings.templateGoApi', icon: Zap },
  { id: 'python_fastapi', label: 'projectSettings.templateFastApi', icon: Sparkles },
  { id: 'blank', label: 'projectSettings.templateBlank', icon: FileText },
]

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

  // Sync localPath, fetch agents & check GitLab connection status whenever modal opens
  useEffect(() => {
    if (isOpen) {
      const legacyDefaultRepo = `/opt/multigent/data/projects/${projectId}/workspace`
      setLocalPath(currentRepo.trim() && currentRepo.trim() !== legacyDefaultRepo ? currentRepo.trim() : fallbackRepo)
      setRepoSlug(projectId)
      setError(null)
      setLoadingAgents(true)

      // A new project has no membership yet. Fall back to workspace agents;
      // the submit step creates the project membership before the task.
      apiFetch<Array<{ name?: string; displayName?: string; model?: string }>>(`/api/v1/projects/${encodeURIComponent(projectId)}/agents`)
        .then((projAgentsRes) => {
          const projList = Array.isArray(projAgentsRes) ? projAgentsRes : []
          const availableWorkers = projList.filter((w) => w.name && w.model !== 'human')
          if (availableWorkers.length > 0) return availableWorkers
          return apiFetch<{ agents?: Array<{ name?: string; displayName?: string; model?: string }> }>('/api/v1/agents')
            .then((workspaceAgentsRes) => (Array.isArray(workspaceAgentsRes?.agents) ? workspaceAgentsRes.agents : [])
              .filter((w) => w.name && w.model !== 'human'))
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

      // React + Go is currently the first deterministic project template. The
      // server writes its fixed files before the Agent task starts; the Agent
      // then installs dependencies, verifies the result and handles Git.
      const deterministicTemplate = activeTab === 'create_new' && useTemplate && selectedTemplate === 'react_go_fullstack'
      let templateReport: { templateId: string; templateVersion: string; templateDigest: string } | null = null
      if (deterministicTemplate) {
        templateReport = await apiPost<{ templateId: string; templateVersion: string; templateDigest: string }>(
          `/api/v1/projects/${encodeURIComponent(projectId)}/initialize-template`,
          { repo: targetRepo, templateId: selectedTemplate, agent: selectedAgent },
        )
      }

      // 3. Prepare task payload & dispatch
      let taskTitle = ''
      let taskPrompt = ''

      if (activeTab === 'bind_existing') {
        if (existingType === 'remote') {
          const url = remoteUrl.trim() || 'git@gitlab.internal:group/repo.git'
          taskTitle = `【工程初始化】克隆远程仓库并检查就绪`
          taskPrompt = `请使用配置好的 Git SSH 凭据或 GitLab 访问令牌，在项目工作区目录 (${targetRepo}) 中克隆远程仓库 "${url}"。\n\n克隆完成后：\n1. 检查工程目录结构与分支信息；\n2. 安装项目依赖（如 npm install / go mod download 等）；\n3. 执行一次语法或单测检查；\n4. 输出初始化就绪报告，说明工程已就绪可开始后续任务。`
        } else {
          taskTitle = `【工程初始化】绑定并校验本地工作区`
          taskPrompt = `项目工作区已绑定到本地路径 "${targetRepo}"。\n\n请进入该目录：\n1. 检查现有代码结构与 Git 状态；\n2. 确认开发环境与依赖就绪情况；\n3. 输出环境健康检查报告。`
        }
      } else {
        // Create new
        const tpl = TEMPLATES.find((x) => x.id === selectedTemplate)
        const tplName = (useTemplate && tpl) ? t(tpl.label) : '基础空白'

        const cleanCloneUrl = remoteMetadata.cloneUrl
        const templateReadySteps = templateReport
          ? `模板已由系统确定性生成（${templateReport.templateId} v${templateReport.templateVersion}，摘要 ${templateReport.templateDigest.slice(0, 12)}）。不要重写基础骨架，先执行：
1. make doctor；
2. cd web && npm install（没有 package-lock.json 时）并执行 npm run build；
3. cd server && go test ./...；
4. 检查 .multigent/runtime.json、前端 /api 代理和 server/api/health 是否一致。
`
          : ''
        if (cleanCloneUrl) {
          taskTitle = `【工程初始化】构建 ${tplName} 脚手架并首推远程 GitLab`
          taskPrompt = `请在当前任务工作区完成 "${tplName}" 工程初始化并首次推送到远程 GitLab 仓库。不要访问或假设宿主机路径；执行命令时以当前目录为准，平台已在任务工作区注入确定性模板。\n\n${templateReadySteps}如果模板尚未由系统生成，再补齐缺失的工程文件；不要覆盖已有用户文件。然后：\n1. 使用系统已注入的 GitLab credential helper 配置远程仓库并推送（禁止把 Token 写入 URL）：\n   git remote add origin "${cleanCloneUrl}" || git remote set-url origin "${cleanCloneUrl}"\n   git branch -M main\n   git add .\n   git commit -m "chore: initial ${tplName} scaffold"\n   git push -u origin main\n2. 输出初始化完成报告，列出目录架构、验证结果与启动命令。`
        } else {
          taskTitle = `【工程初始化】构建 ${tplName} 模板脚手架`
          taskPrompt = `请在当前任务工作区完成 "${tplName}" 工程初始化。不要访问或假设宿主机路径；执行命令时以当前目录为准，平台已在任务工作区注入确定性模板。\n\n${templateReadySteps}如果模板尚未由系统生成，再补齐缺失的工程文件；不要覆盖已有用户文件。然后：\n1. 初始化 Git 仓库并创建首个提交；\n2. 执行基础测试与构建，确保工程可一键启动；\n3. 输出初始化完成报告，列出目录架构、验证结果与启动命令。`
        }
      }

      // 4. Create and directly start initialization task
      await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/tasks`, {
        agent: selectedAgent,
        title: taskTitle,
        description: `自动化工程初始化 (${activeTab === 'bind_existing' ? '已有仓库' : '从零新建'})`,
        prompt: taskPrompt,
        type: 'chore',
        priority: 3,
        autoStart: true,
      })

      showToast(t('projectSettings.initSuccess', { defaultValue: '工程初始化任务已创建并启动！' }), 'success')
      onSuccess(targetRepo)
      navigate(`/projects/${encodeURIComponent(projectId)}/tasks`)
    } catch (err) {
      setError(err instanceof ApiError && err.serverMessage
        ? err.serverMessage
        : err instanceof Error ? err.message : String(err))
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
                      同步在 GitLab 上自动创建远程仓库并绑定 (推荐)
                    </span>
                  </label>

                  {syncToGitLab && (
                    <div className="space-y-3 pt-1">
                      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                        <label className="block">
                          <span className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                            命名空间 (Namespace / Group)
                          </span>
                          <select
                            value={selectedNamespaceId || ''}
                            onChange={(e) => setSelectedNamespaceId(Number(e.target.value))}
                            className="mt-1 w-full rounded-md border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-900 outline-none focus:border-sky-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                          >
                            {gitlabNamespaces.map((ns) => (
                              <option key={ns.id} value={ns.id}>
                                {ns.fullPath} ({ns.kind === 'user' ? '个人' : '团队'})
                              </option>
                            ))}
                          </select>
                        </label>

                        <label className="block">
                          <span className="text-[11px] font-medium text-neutral-600 dark:text-zinc-400">
                            仓库路径 (Repository Slug)
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
                          可见性:
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
                          <span>私有 (Private)</span>
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
                          <span>内部 (Internal)</span>
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
        </div>

        {/* Footer */}
        <div className="flex items-center justify-end gap-2.5 border-t border-neutral-100 bg-neutral-50/50 px-6 py-3.5 dark:border-zinc-800 dark:bg-zinc-950/30">
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
        </div>
      </div>
    </div>
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
          <Markdown remarkPlugins={[remarkGfm]}>{value || '*（空）*'}</Markdown>
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
