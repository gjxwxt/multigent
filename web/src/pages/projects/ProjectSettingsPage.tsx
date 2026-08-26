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
import { apiDelete, apiFetch, apiPost, apiPut } from '../../lib/api'
import { ConfirmDialog } from '../../components/ui/ConfirmDialog'

type ProjectDetail = { name: string; description: string; repo: string }
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
  onReload,
}: {
  projectId: string
  name: string
  initialDescription: string
  initialRepo: string
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
      </dl>

      <InitializeProjectModal
        projectId={projectId}
        currentDescription={description}
        currentRepo={repo}
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
  isOpen,
  onClose,
  onSuccess,
}: {
  projectId: string
  currentDescription: string
  currentRepo: string
  isOpen: boolean
  onClose: () => void
  onSuccess: (newRepo: string) => void
}) {
  const { t } = useTranslation()
  const [activeTab, setActiveTab] = useState<'bind_existing' | 'create_new'>('bind_existing')
  const [existingType, setExistingType] = useState<'local' | 'remote'>('remote')
  const [localPath, setLocalPath] = useState(currentRepo.trim() || `/opt/multigent/data/projects/${projectId}/workspace`)
  const [remoteUrl, setRemoteUrl] = useState('')
  const [useTemplate, setUseTemplate] = useState(true)
  const [selectedTemplate, setSelectedTemplate] = useState('react_go_fullstack')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const [agents, setAgents] = useState<Array<{ name: string; displayName?: string; model?: string }>>([])
  const [selectedAgent, setSelectedAgent] = useState<string>('')
  const [loadingAgents, setLoadingAgents] = useState(false)

  // Sync localPath & fetch project member agents whenever modal opens
  useEffect(() => {
    if (isOpen) {
      if (currentRepo.trim()) {
        setLocalPath(currentRepo.trim())
      }
      setError(null)
      setLoadingAgents(true)

      apiFetch<Array<{ name?: string; displayName?: string; model?: string }>>(`/api/v1/projects/${encodeURIComponent(projectId)}/agents`)
        .then((projAgentsRes) => {
          const projList = Array.isArray(projAgentsRes) ? projAgentsRes : []
          const availableWorkers = projList.filter((w) => w.name && w.model !== 'human')
          setAgents(availableWorkers as Array<{ name: string; displayName?: string; model?: string }>)
          if (availableWorkers.length > 0) {
            setSelectedAgent(availableWorkers[0].name)
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
    }
  }, [isOpen, currentRepo, projectId])

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

      const defaultProjectWorkspace = currentRepo.trim() || `/opt/multigent/data/projects/${projectId}/workspace`
      const targetRepo =
        activeTab === 'bind_existing' && existingType === 'local' && localPath.trim()
          ? localPath.trim()
          : defaultProjectWorkspace

      // 1. Update project repo in backend
      await apiPut(`/api/v1/projects/${encodeURIComponent(projectId)}`, {
        description: currentDescription,
        repo: targetRepo,
      })

      // 2. Ensure agent is bound to project memberships
      try {
        await apiPost(
          `/api/v1/projects/${encodeURIComponent(projectId)}/memberships`,
          { workerName: selectedAgent }
        )
      } catch {
        // non-blocking if already member
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
        if (useTemplate) {
          const tpl = TEMPLATES.find((x) => x.id === selectedTemplate)
          const tplName = tpl ? t(tpl.label) : selectedTemplate
          taskTitle = `【工程初始化】构建 ${tplName} 模板脚手架`
          taskPrompt = `请在当前项目工作区 (${targetRepo}) 初始化 "${tplName}" 工程骨架：\n\n1. 初始化 Git 仓库并创建符合最佳实践的完整工程目录；\n2. 生成基础依赖配置（package.json、go.mod、requirements.txt 等）与入口文件；\n3. 添加标准的 .gitignore 和详细的 README.md 开发说明；\n4. 进行一次基础构建与语法校验，确保工程可一键启动；\n5. 输出初始化完成报告，列出目录架构与启动命令。`
        } else {
          taskTitle = `【工程初始化】初始化空白代码工程`
          taskPrompt = `请在当前项目工作区 (${targetRepo}) 初始化基础 Git 仓库，创建标准的 README.md 和 .gitignore 文件，并输出初始化完成说明。`
        }
      }

      // 4. Create initialization task
      const createdTask = await apiPost<{ id?: string }>(`/api/v1/projects/${encodeURIComponent(projectId)}/tasks`, {
        agent: selectedAgent,
        title: taskTitle,
        description: `自动化工程初始化 (${activeTab === 'bind_existing' ? '已有仓库' : '从零新建'})`,
        prompt: taskPrompt,
        type: 'chore',
        priority: 3,
      })

      // 5. Directly start this exact initialization task
      if (createdTask?.id) {
        try {
          await apiPost(
            `/api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(createdTask.id)}/start`,
            {},
            { suppressToast: true }
          )
        } catch {
          // non-blocking
        }
      }

      onSuccess(targetRepo)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
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

