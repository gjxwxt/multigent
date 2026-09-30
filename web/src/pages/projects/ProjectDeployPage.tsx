// 部署中心页面（deploy center, phase 1）。
// 数据源：GET /api/v1/projects/{name}/deploy 聚合端点 + 部署单 CRUD。
// UI 按 docs/deploy-center-prototype.html 裁剪：无敏感变量明文、无容器状态卡、
// 审批走项目级开关、空态只展示真实存在的预分配端口与 runner 绑定。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import {
  CheckCircle2,
  Clock,
  ExternalLink,
  GitBranch,
  GitCommitHorizontal,
  History,
  Loader2,
  Rocket,
  Send,
  Server,
  ShieldCheck,
  Tag,
  Terminal,
  XCircle,
} from 'lucide-react'
import { cn } from '../../lib/cn'
import { useApiJson } from '../../lib/use-api'
import { apiPost } from '../../lib/api'
import { showToast } from '../../components/ui/Toast'
import { confirmDialog } from '../../components/ui/ConfirmDialog'

const cardCls = 'rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40'
const cardHeadCls = 'border-b border-neutral-100 px-5 py-3 dark:border-zinc-800'
const cardTitleCls = 'text-sm font-semibold text-neutral-800 dark:text-zinc-100'
const neutralButton =
  'inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-sm font-medium text-neutral-700 shadow-sm hover:bg-neutral-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200 dark:hover:bg-zinc-700'
const primaryButton =
  'inline-flex items-center gap-2 rounded-lg bg-sky-600 px-4 py-2 text-sm font-medium text-white hover:bg-sky-700 active:bg-sky-800 disabled:cursor-not-allowed disabled:opacity-60'

type CommitSpanEntry = { sha: string; shortSha: string; title: string; author: string; committedAt: string }

type DeployRequest = {
  id: string
  project: string
  branch: string
  sha: string
  env: string
  status: string
  pipelineId?: number
  commitSpan: CommitSpanEntry[]
  createdBy: string
  createdAt: string
  finishedAt?: string
  health?: { status?: string; latencyMs?: number; reason?: string }
}

type PipelineSummary = {
  id: number
  ref: string
  sha: string
  status: string
  jobs: { name: string; stage: string; status: string; runnerDescription?: string }[]
}

type DeployAggregate = {
  deployPort: number
  deployHostConfigured: boolean
  lastDeployed: DeployRequest | null
  inflight: DeployRequest | null
  health: { status: string; latencyMs?: number; reason?: string } | null
  pipelines: PipelineSummary[]
}

type BranchInfo = { name: string; isDefault: boolean; commitId: string; commitTitle: string }

const statusText: Record<string, string> = {
  pending_approval: '待审批',
  approved: '已批准',
  deploying: '部署中',
  success: '成功',
  failed: '失败',
  cancelled: '已取消',
  rejected: '已驳回',
}

function statusCls(status: string): string {
  switch (status) {
    case 'success':
      return 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-300'
    case 'failed':
    case 'rejected':
      return 'bg-red-100 text-red-800 dark:bg-red-950/60 dark:text-red-300'
    case 'deploying':
    case 'approved':
      return 'bg-sky-100 text-sky-800 dark:bg-sky-950/60 dark:text-sky-300'
    case 'pending_approval':
      return 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-300'
    default:
      return 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400'
  }
}

function pipelineStatusCls(status: string): string {
  switch (status) {
    case 'success':
      return 'text-emerald-700 dark:text-emerald-400'
    case 'failed':
      return 'text-red-600 dark:text-red-400'
    case 'running':
      return 'text-sky-700 dark:text-sky-400'
    default:
      return 'text-neutral-500 dark:text-zinc-400'
  }
}

function shortSha(sha: string): string {
  return sha ? sha.slice(0, 8) : ''
}

function formatTime(iso?: string): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(undefined, { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  } catch {
    return iso
  }
}

export default function ProjectDeployPage() {
  const { projectId = '' } = useParams()
  const { t } = useTranslation()
  const [reloadKey, setReloadKey] = useState(0)
  const [branch, setBranch] = useState('')
  const [selectedSha, setSelectedSha] = useState('')
  const [creating, setCreating] = useState(false)
  const [actionBusy, setActionBusy] = useState<string | null>(null)

  const aggState = useApiJson<DeployAggregate>(
    projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}/deploy` : null,
    reloadKey,
  )
  const branchesState = useApiJson<BranchInfo[]>(
    projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/branches` : null,
    reloadKey,
  )
  const agg = aggState.status === 'ok' ? aggState.data : null
  const branches = branchesState.status === 'ok' ? branchesState.data : []
  const inflight = agg?.inflight ?? null
  const lastDeployed = agg?.lastDeployed ?? null
  const reload = useCallback(() => setReloadKey((k) => k + 1), [])

  // 轮询：部署中或有待审批单时 5s 刷新，否则 30s
  useEffect(() => {
    const period = inflight && (inflight.status === 'deploying' || inflight.status === 'pending_approval' || inflight.status === 'approved') ? 5000 : 30000
    const timer = setInterval(reload, period)
    return () => clearInterval(timer)
  }, [inflight?.status, reload])

  // 分支选择默认 main（或第一个 default 分支）
  useEffect(() => {
    if (!branch && branches.length > 0) {
      const def = branches.find((b) => b.isDefault) ?? branches[0]
      setBranch(def.name)
      setSelectedSha(def.commitId)
    }
  }, [branches, branch])

  const onBranchChange = (name: string) => {
    setBranch(name)
    const b = branches.find((x) => x.name === name)
    setSelectedSha(b?.commitId ?? '')
  }

  // Span 契约：commitSpan 是 [基线 SHA, 目标 SHA] 两元素（基线=上次成功部署），
  // 不是逐 commit 清单；部署区间真正的 commit 明细后续从 GitLab compare 接口补。
  const pendingSpan = useMemo<CommitSpanEntry[]>(() => {
    if (inflight) return inflight.commitSpan ?? []
    return []
  }, [inflight])

  async function createRequest(approvalRequired: boolean) {
    if (!projectId || !branch || !selectedSha) return
    const ok = await confirmDialog({
      title: t('projectDeploy.confirmTitle', { defaultValue: '发起部署？' }),
      description: t('projectDeploy.confirmBody', {
        defaultValue: `将对 ${branch}@${shortSha(selectedSha)} 发起${approvalRequired ? '（需审批）' : ''}部署：构建镜像并在部署机 compose 上线。`,
      }),
      confirmLabel: t('projectDeploy.create', { defaultValue: '发起部署' }),
      cancelLabel: t('projectDeploy.cancelConfirm', { defaultValue: '取消' }),
    })
    if (!ok) return
    setCreating(true)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests`, { branch, sha: selectedSha, approvalRequired })
      showToast(t('projectDeploy.created', { defaultValue: '部署单已创建' }), 'success')
      reload()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setCreating(false)
    }
  }

  async function requestAction(id: string, action: 'approve' | 'reject' | 'cancel' | 'trigger') {
    if (!projectId) return
    setActionBusy(`${id}:${action}`)
    try {
      await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(id)}/${action}`, {})
      showToast(t('projectDeploy.actionDone', { defaultValue: '操作已执行' }), 'success')
      reload()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setActionBusy(null)
    }
  }

  const health = agg?.health ?? null
  const healthUp = health?.status === 'up'
  const healthUnknown = !health || health.status === 'unknown'
  const appUrl = agg?.deployHostConfigured && agg.deployPort ? `http://${window.location.hostname}:${agg.deployPort}` : null

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="shrink-0 px-6 pt-5 pb-3">
        <h1 className="text-xl font-semibold text-neutral-900 dark:text-zinc-100">
          {t('projectNav.deploy', { defaultValue: '部署' })}
        </h1>
        <p className="mt-0.5 text-sm text-neutral-500 dark:text-zinc-500">
          {t('projectDeploy.subtitle', { defaultValue: '生产环境部署：分支选择 · 审批 · 流水线 · 台账' })}
        </p>
      </div>

      <div className="flex-1 overflow-y-auto px-6 pb-8">
        <div className="space-y-6">
          {/* 空态：从未部署 */}
          {agg && !lastDeployed && !inflight && (
            <section className={cn(cardCls, 'mx-auto max-w-lg p-8 text-center')}>
              <div className="mx-auto flex size-14 items-center justify-center rounded-2xl bg-sky-50 text-sky-600 dark:bg-sky-950/60 dark:text-sky-400">
                <Rocket className="size-7" />
              </div>
              <h2 className="mt-4 text-base font-semibold text-neutral-900 dark:text-zinc-100">
                {t('projectDeploy.emptyTitle', { defaultValue: '该项目还没有部署过' })}
              </h2>
              <p className="mt-1 text-xs text-neutral-500 dark:text-zinc-500">
                {t('projectDeploy.emptyBody', {
                  defaultValue: '在下方选择目标分支，发起首次部署。部署后应用将运行在分配的端口上。',
                })}
              </p>
              <div className="mt-4 grid grid-cols-2 gap-2 text-left">
                <div className="rounded-lg border border-neutral-200/60 bg-neutral-50/60 p-2.5 dark:border-zinc-800 dark:bg-zinc-900/40">
                  <div className="text-[10px] text-neutral-400 dark:text-zinc-500">{t('projectDeploy.assignedPort', { defaultValue: '预分配端口' })}</div>
                  <div className="font-mono text-sm font-semibold text-sky-600 dark:text-sky-400">:{agg.deployPort || '—'}</div>
                </div>
                <div className="rounded-lg border border-neutral-200/60 bg-neutral-50/60 p-2.5 dark:border-zinc-800 dark:bg-zinc-900/40">
                  <div className="text-[10px] text-neutral-400 dark:text-zinc-500">{t('projectDeploy.remoteBinding', { defaultValue: '远端绑定' })}</div>
                  <div className="text-sm font-semibold text-neutral-800 dark:text-zinc-200">
                    {branches.length > 0 ? t('projectDeploy.bound', { defaultValue: '已连接 GitLab' }) : t('projectDeploy.unbound', { defaultValue: '未绑定' })}
                  </div>
                </div>
              </div>
            </section>
          )}

          {/* 部署中驾驶舱（置顶） */}
          {inflight && inflight.status !== 'success' && (
            <section className="rounded-lg border border-sky-300 bg-sky-50/50 p-4 dark:border-sky-700 dark:bg-sky-950/30">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex items-center gap-2.5">
                  <Loader2 className="size-4 animate-spin text-sky-600 dark:text-sky-400" />
                  <span className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
                    {t('projectDeploy.inflight', { defaultValue: '部署进行中' })} · {inflight.branch}@{shortSha(inflight.sha)}
                  </span>
                  <span className={cn('rounded-full px-2 py-0.5 text-[11px] font-medium', statusCls(inflight.status))}>
                    {statusText[inflight.status] ?? inflight.status}
                  </span>
                </div>
                <div className="flex items-center gap-2">
                  {(inflight.status === 'pending_approval' || inflight.status === 'approved') && (
                    <>
                      <button type="button" onClick={() => void requestAction(inflight.id, 'approve')} disabled={actionBusy !== null} className={cn(primaryButton, 'px-3 py-1.5 text-xs')}>
                        <ShieldCheck className="size-3.5" />
                        {t('projectDeploy.approve', { defaultValue: '批准' })}
                      </button>
                      <button type="button" onClick={() => void requestAction(inflight.id, 'reject')} disabled={actionBusy !== null} className={cn(neutralButton, 'px-3 py-1.5 text-xs')}>
                        {t('projectDeploy.reject', { defaultValue: '驳回' })}
                      </button>
                    </>
                  )}
                  {inflight.status === 'approved' && (
                    <button type="button" onClick={() => void requestAction(inflight.id, 'trigger')} disabled={actionBusy !== null} className={cn(primaryButton, 'px-3 py-1.5 text-xs')}>
                      <Rocket className="size-3.5" />
                      {t('projectDeploy.trigger', { defaultValue: '触发流水线' })}
                    </button>
                  )}
                  <button type="button" onClick={() => void requestAction(inflight.id, 'cancel')} disabled={actionBusy !== null} className={cn(neutralButton, 'px-3 py-1.5 text-xs')}>
                    {t('projectDeploy.cancel', { defaultValue: '取消' })}
                  </button>
                </div>
              </div>
              {inflight.commitSpan?.length > 0 && (
                <div className="mt-3 space-y-1 font-mono text-[11px] text-neutral-600 dark:text-zinc-400">
                  {inflight.commitSpan.slice(0, 5).map((c) => (
                    <div key={c.sha} className="truncate">
                      {c.shortSha || c.sha.slice(0, 8)} · {c.title}
                    </div>
                  ))}
                  {inflight.commitSpan.length > 5 && (
                    <div className="text-neutral-400">… +{inflight.commitSpan.length - 5}</div>
                  )}
                </div>
              )}
            </section>
          )}

          {/* 环境卡（运行版本 / 健康探针 / 端口 / 最近部署） */}
          {lastDeployed && (
            <section className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <div className={cn(cardCls, 'p-4')}>
                <div className="flex items-center justify-between text-xs text-neutral-500 dark:text-zinc-400">
                  <span>{t('projectDeploy.liveVersion', { defaultValue: '运行版本' })}</span>
                  <Tag className="size-4" />
                </div>
                <div className="mt-2 flex items-baseline gap-2">
                  <span className="font-mono text-xl font-bold text-neutral-900 dark:text-zinc-100">{shortSha(lastDeployed.sha)}</span>
                  <span className="rounded bg-sky-50 px-1.5 py-0.5 font-mono text-xs text-sky-700 dark:bg-sky-950/60 dark:text-sky-400">{lastDeployed.branch}</span>
                </div>
                <p className="mt-2 text-[11px] text-neutral-400 dark:text-zinc-500">
                  {t('projectDeploy.deployedBy', { defaultValue: '部署于' })} {formatTime(lastDeployed.finishedAt || lastDeployed.createdAt)} · {lastDeployed.createdBy}
                </p>
              </div>

              <div className={cn(cardCls, 'p-4')}>
                <div className="flex items-center justify-between text-xs text-neutral-500 dark:text-zinc-400">
                  <span>{t('projectDeploy.health', { defaultValue: '健康探针' })}</span>
                </div>
                <div className="mt-2">
                  {healthUnknown ? (
                    <span className="inline-flex items-center gap-1.5 text-lg font-semibold text-neutral-500 dark:text-zinc-400">
                      {t('projectDeploy.healthUnknown', { defaultValue: '未知' })}
                    </span>
                  ) : healthUp ? (
                    <span className="inline-flex items-center gap-1.5 text-lg font-semibold text-emerald-600 dark:text-emerald-400">
                      <span className="size-2.5 rounded-full bg-emerald-500" />
                      UP
                    </span>
                  ) : (
                    <span className="inline-flex items-center gap-1.5 text-lg font-semibold text-red-600 dark:text-red-400">
                      <span className="size-2.5 rounded-full bg-red-500" />
                      {t('projectDeploy.healthDown', { defaultValue: '异常' })}
                    </span>
                  )}
                </div>
                <p className="mt-2 truncate font-mono text-[11px] text-neutral-400 dark:text-zinc-500">
                  {health?.reason ?? (health?.latencyMs != null ? `${health.latencyMs}ms` : '')}
                </p>
              </div>

              <div className={cn(cardCls, 'p-4')}>
                <div className="flex items-center justify-between text-xs text-neutral-500 dark:text-zinc-400">
                  <span>{t('projectDeploy.hostPort', { defaultValue: '部署机 & 端口' })}</span>
                  <Server className="size-4" />
                </div>
                <div className="mt-2 flex items-baseline gap-2">
                  <span className="font-mono text-xl font-bold text-neutral-900 dark:text-zinc-100">:{agg?.deployPort ?? '—'}</span>
                </div>
                <p className="mt-2 truncate text-[11px] text-neutral-400 dark:text-zinc-500">
                  {agg?.deployHostConfigured
                    ? t('projectDeploy.hostConfigured', { defaultValue: '部署机已配置' })
                    : t('projectDeploy.hostNotConfigured', { defaultValue: '未配置部署机地址（MULTIGENT_DEPLOY_HOST）' })}
                </p>
              </div>

              <div className={cn(cardCls, 'flex flex-col justify-between p-4')}>
                <div className="flex items-center justify-between text-xs text-neutral-500 dark:text-zinc-400">
                  <span>{t('projectDeploy.openApp', { defaultValue: '访问应用' })}</span>
                  <ExternalLink className="size-4" />
                </div>
                {appUrl ? (
                  <a href={appUrl} target="_blank" rel="noreferrer" className={cn(primaryButton, 'mt-2 justify-center')}>
                    {t('projectDeploy.openNewTab', { defaultValue: '新窗口打开' })}
                  </a>
                ) : (
                  <p className="mt-2 text-[11px] text-neutral-400 dark:text-zinc-500">
                    {t('projectDeploy.openUnavailable', { defaultValue: '配置部署机地址后可直达' })}
                  </p>
                )}
              </div>
            </section>
          )}

          {/* 发起部署 */}
          <section className={cardCls}>
            <div className={cn(cardHeadCls, 'flex flex-wrap items-center justify-between gap-3')}>
              <div className="flex items-center gap-2.5">
                <div className="flex size-7 items-center justify-center rounded-lg bg-sky-500/10 text-sky-600 dark:text-sky-400">
                  <Send className="size-4" />
                </div>
                <div>
                  <h3 className={cardTitleCls}>{t('projectDeploy.consoleTitle', { defaultValue: '发起部署' })}</h3>
                  <p className="text-[11px] text-neutral-400 dark:text-zinc-500">
                    {t('projectDeploy.consoleSub', { defaultValue: '选择目标分支与基线，合并可攒批一次部署（发布火车）' })}
                  </p>
                </div>
              </div>
              {inflight && (
                <span className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-neutral-100 px-3 py-1.5 text-xs text-neutral-500 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-400">
                  {t('projectDeploy.locked', { defaultValue: '部署进行中 · 并发互斥已锁定' })}
                </span>
              )}
            </div>
            <div className="space-y-4 p-5">
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                <div>
                  <label className="mb-1.5 block text-xs font-medium text-neutral-500 dark:text-zinc-400">
                    {t('projectDeploy.targetBranch', { defaultValue: '目标分支' })}
                  </label>
                  <select
                    value={branch}
                    onChange={(e) => onBranchChange(e.target.value)}
                    disabled={Boolean(inflight) || branches.length === 0}
                    className="w-full appearance-none rounded-lg border border-neutral-300/80 bg-white px-3 py-2 text-sm font-medium text-neutral-800 shadow-sm outline-none hover:border-neutral-400 focus:border-sky-500 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800/80 dark:text-zinc-200"
                  >
                    {branches.length === 0 && <option value="">{t('projectDeploy.noBranches', { defaultValue: '（无远端绑定）' })}</option>}
                    {branches.map((b) => (
                      <option key={b.name} value={b.name}>
                        {b.name}
                        {b.isDefault ? ` (${t('projectDeploy.defaultBranch', { defaultValue: '默认' })})` : ''}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label className="mb-1.5 block text-xs font-medium text-neutral-500 dark:text-zinc-400">
                    {t('projectDeploy.targetSha', { defaultValue: '目标 Commit 基线' })}
                  </label>
                  <div className="flex items-center gap-2 rounded-lg border border-neutral-200/80 bg-neutral-50/80 px-3 py-2 font-mono text-sm text-neutral-700 dark:border-zinc-800 dark:bg-zinc-800/40 dark:text-zinc-300">
                    <GitCommitHorizontal className="size-4 text-neutral-400" />
                    <span className="font-semibold text-sky-600 dark:text-sky-400">{shortSha(selectedSha) || '—'}</span>
                  </div>
                </div>
              </div>

              {pendingSpan.length > 0 && (
                <div className="rounded-lg border border-sky-100 bg-sky-50/40 p-4 dark:border-sky-950/60 dark:bg-sky-950/20">
                  <div className="text-xs font-semibold text-neutral-800 dark:text-zinc-100">
                    {t('projectDeploy.pendingChanges', { defaultValue: '待发布变更' })} · {pendingSpan.length} commits
                  </div>
                  <div className="mt-2 space-y-1 font-mono text-xs">
                    {pendingSpan.slice(0, 8).map((c) => (
                      <div key={c.sha} className="flex items-center justify-between gap-3 truncate">
                        <span className="text-sky-600 dark:text-sky-400">{c.shortSha || c.sha.slice(0, 8)}</span>
                        <span className="flex-1 truncate text-neutral-700 dark:text-zinc-300">{c.title}</span>
                        <span className="text-neutral-400 dark:text-zinc-500">{c.author}</span>
                      </div>
                    ))}
                  </div>
                </div>
              )}

              <div className="flex flex-wrap items-center justify-between gap-3 pt-1">
                <div className="flex items-center gap-2 text-xs text-neutral-500 dark:text-zinc-400">
                  <ShieldCheck className="size-4 text-emerald-500" />
                  {t('projectDeploy.auditNote', { defaultValue: '部署单写入不可变审计台账，同项目并发互斥' })}
                </div>
                <div className="flex items-center gap-2">
                  <button
                    type="button"
                    onClick={() => void createRequest(false)}
                    disabled={Boolean(inflight) || creating || !selectedSha || branches.length === 0}
                    className={primaryButton}
                  >
                    {creating ? <Loader2 className="size-4 animate-spin" /> : <Rocket className="size-4" />}
                    {t('projectDeploy.create', { defaultValue: '发起部署' })}
                  </button>
                  <button
                    type="button"
                    onClick={() => void createRequest(true)}
                    disabled={Boolean(inflight) || creating || !selectedSha || branches.length === 0}
                    className={neutralButton}
                  >
                    <ShieldCheck className="size-4" />
                    {t('projectDeploy.createWithApproval', { defaultValue: '发起（需审批）' })}
                  </button>
                </div>
              </div>
            </div>
          </section>

          {/* 部署历史台账 */}
          <DeployLedger projectId={projectId} reloadKey={reloadKey} onAction={requestAction} actionBusy={actionBusy} />

          {/* GitLab 流水线 */}
          {agg && agg.pipelines.length > 0 && (
            <section className={cardCls}>
              <div className={cn(cardHeadCls, 'flex items-center justify-between')}>
                <h3 className={cardTitleCls}>{t('projectDeploy.pipelines', { defaultValue: '流水线（GitLab）' })}</h3>
              </div>
              <div className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
                {agg.pipelines.map((p) => (
                  <div key={p.id} className="flex flex-wrap items-center justify-between gap-2 px-5 py-3 text-xs">
                    <div className="flex items-center gap-2 font-mono">
                      <span className="text-neutral-500 dark:text-zinc-400">#{p.id}</span>
                      <span className={pipelineStatusCls(p.status)}>{p.status}</span>
                      <span className="text-neutral-700 dark:text-zinc-300">{p.ref}</span>
                      <span className="text-neutral-400">{shortSha(p.sha)}</span>
                    </div>
                    <div className="flex flex-wrap items-center gap-1.5">
                      {p.jobs.slice(0, 6).map((j) => (
                        <span
                          key={j.name}
                          title={j.runnerDescription || undefined}
                          className={cn(
                            'rounded px-1.5 py-0.5 text-[10px] font-medium',
                            j.status === 'success'
                              ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300'
                              : j.status === 'failed'
                                ? 'bg-red-50 text-red-700 dark:bg-red-950/40 dark:text-red-300'
                                : 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400',
                          )}
                        >
                          {j.name}
                        </span>
                      ))}
                    </div>
                  </div>
                ))}
              </div>
            </section>
          )}
        </div>
      </div>
    </div>
  )
}

function DeployLedger({
  projectId,
  reloadKey,
  onAction,
  actionBusy,
}: {
  projectId: string
  reloadKey: number
  onAction: (id: string, action: 'approve' | 'reject' | 'cancel' | 'trigger') => Promise<void>
  actionBusy: string | null
}) {
  const { t } = useTranslation()
  const state = useApiJson<{ requests: DeployRequest[] } | DeployRequest[]>(
    projectId ? `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests` : null,
    reloadKey,
  )
  const raw = state.status === 'ok' ? state.data : null
  const rows = Array.isArray(raw) ? raw : (raw?.requests ?? [])
  const [expanded, setExpanded] = useState<string | null>(null)

  return (
    <section className={cardCls}>
      <div className={cn(cardHeadCls, 'flex items-center justify-between')}>
        <div className="flex items-center gap-2.5">
          <div className="flex size-7 items-center justify-center rounded-lg bg-neutral-500/10 text-neutral-700 dark:text-zinc-300">
            <History className="size-4" />
          </div>
          <h3 className={cardTitleCls}>{t('projectDeploy.ledger', { defaultValue: '部署历史台账' })}</h3>
        </div>
        <span className="font-mono text-xs text-neutral-400">{rows.length}</span>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-left text-sm">
          <thead>
            <tr className="border-b border-neutral-200/80 bg-neutral-50/80 text-[11px] uppercase tracking-wider text-neutral-500 dark:border-zinc-700/60 dark:bg-zinc-900/40 dark:text-zinc-500">
              <th className="px-5 py-2.5 font-medium">{t('projectDeploy.colRequest', { defaultValue: '部署单' })}</th>
              <th className="px-4 py-2.5 font-medium">{t('projectDeploy.colTarget', { defaultValue: '目标' })}</th>
              <th className="px-4 py-2.5 font-medium">{t('projectDeploy.colActor', { defaultValue: '发起人' })}</th>
              <th className="px-4 py-2.5 font-medium">{t('projectDeploy.colStatus', { defaultValue: '状态' })}</th>
              <th className="px-5 py-2.5 text-right font-medium">{t('projectDeploy.colActions', { defaultValue: '操作' })}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
            {rows.length === 0 && (
              <tr>
                <td colSpan={5} className="px-5 py-8 text-center text-xs text-neutral-400 dark:text-zinc-500">
                  {t('projectDeploy.noRequests', { defaultValue: '暂无部署记录' })}
                </td>
              </tr>
            )}
            {rows.map((r) => (
              <>
                <tr key={r.id} className="cursor-pointer transition-colors hover:bg-sky-50/40 dark:hover:bg-sky-900/[0.06]" onClick={() => setExpanded(expanded === r.id ? null : r.id)}>
                  <td className="px-5 py-3">
                    <div className="flex items-center gap-2 font-mono text-xs font-semibold text-neutral-800 dark:text-zinc-200">
                      <GitBranch className="size-3.5 text-neutral-400" />
                      {r.id}
                    </div>
                    <div className="mt-0.5 text-[11px] text-neutral-400">{formatTime(r.createdAt)}</div>
                  </td>
                  <td className="px-4 py-3 font-mono text-xs">
                    <span className="text-sky-600 dark:text-sky-400">{r.branch}@{shortSha(r.sha)}</span>
                    {r.commitSpan?.length > 0 && <span className="ml-1.5 rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">+{r.commitSpan.length}</span>}
                  </td>
                  <td className="px-4 py-3 text-xs text-neutral-700 dark:text-zinc-300">{r.createdBy}</td>
                  <td className="px-4 py-3">
                    <span className={cn('inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium', statusCls(r.status))}>
                      {r.status === 'success' && <CheckCircle2 className="size-3" />}
                      {(r.status === 'failed' || r.status === 'rejected') && <XCircle className="size-3" />}
                      {(r.status === 'deploying' || r.status === 'approved') && <Clock className="size-3" />}
                      {statusText[r.status] ?? r.status}
                    </span>
                  </td>
                  <td className="px-5 py-3 text-right">
                    {r.status === 'pending_approval' && (
                      <button
                        type="button"
                        onClick={(e) => { e.stopPropagation(); void onAction(r.id, 'approve') }}
                        disabled={actionBusy !== null}
                        className="mr-2 text-xs font-medium text-sky-600 hover:text-sky-500 dark:text-sky-400"
                      >
                        {t('projectDeploy.approve', { defaultValue: '批准' })}
                      </button>
                    )}
                    {r.status === 'approved' && (
                      <button
                        type="button"
                        onClick={(e) => { e.stopPropagation(); void onAction(r.id, 'trigger') }}
                        disabled={actionBusy !== null}
                        className="mr-2 text-xs font-medium text-sky-600 hover:text-sky-500 dark:text-sky-400"
                      >
                        <Terminal className="mr-1 inline size-3" />
                        {t('projectDeploy.trigger', { defaultValue: '触发' })}
                      </button>
                    )}
                  </td>
                </tr>
                {expanded === r.id && (
                  <tr key={`${r.id}-detail`}>
                    <td colSpan={5} className="bg-neutral-50/60 px-5 py-3 dark:bg-zinc-900/40">
                      <div className="space-y-1 font-mono text-[11px] text-neutral-600 dark:text-zinc-400">
                        <div>SHA {r.sha}</div>
                        {r.pipelineId ? <div>pipeline #{r.pipelineId}</div> : null}
                        {r.health?.status ? <div>health: {r.health.status}{r.health.latencyMs != null ? ` (${r.health.latencyMs}ms)` : ''}{r.health.reason ? ` — ${r.health.reason}` : ''}</div> : null}
                        {r.commitSpan?.slice(0, 10).map((c) => (
                          <div key={c.sha} className="truncate">{c.shortSha || c.sha.slice(0, 8)} · {c.title} · {c.author}</div>
                        ))}
                      </div>
                    </td>
                  </tr>
                )}
              </>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}
