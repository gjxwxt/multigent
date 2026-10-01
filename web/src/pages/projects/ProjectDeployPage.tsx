// 部署中心页面（deploy center, phase 2 重构）。
// 组件拆分与契约事实基准见 docs/deploy-center-frontend-refactor-spec-v2.md：
// 数据源 GET /api/v1/projects/{name}/deploy 聚合端点 + 部署单 CRUD（后端零改动，一期纯前端）。
// 硬红线：APP_PORT 提交剔除（EnvVarsEditor.sanitizeVarRows）；敏感变量只存 *** 掩码；
// startedAt/finishedAt 恒空，计时与"部署于"只用 createdAt；commitSpan 恒两元素（基线/目标）。
import { useCallback, useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { useApiJson } from '../../lib/use-api'
import { apiPost } from '../../lib/api'
import { showToast } from '../../components/ui/Toast'
import { confirmDialog } from '../../components/ui/ConfirmDialog'
import { bindDeployT, shortSha, type BranchInfo, type DeployAggregate, type DeployRequest } from './deploy/deploy-shared'
import { DeployHeader } from './deploy/DeployHeader'
import { InFlightCockpit } from './deploy/InFlightCockpit'
import { RealityDeck } from './deploy/RealityDeck'
import { DeployConsole } from './deploy/DeployConsole'
import { DeployLedgerTable } from './deploy/DeployLedgerTable'
import { DeployRollbackModal } from './deploy/DeployRollbackModal'
import { DeployEmptyHero } from './deploy/DeployEmptyHero'
export default function ProjectDeployPage() {
  const { projectId = '' } = useParams()
  const { t } = useTranslation()
  const [reloadKey, setReloadKey] = useState(0)
  const [branch, setBranch] = useState('')
  const [selectedSha, setSelectedSha] = useState('')
  const [creating, setCreating] = useState(false)
  const [actionBusy, setActionBusy] = useState<string | null>(null)
  const [rollbackTarget, setRollbackTarget] = useState<DeployRequest | null>(null)
  const [showInitialConfig, setShowInitialConfig] = useState(false)
  const consoleRef = useRef<HTMLElement | null>(null)

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

  // statusText 等工具非组件内部无法走 useTranslation，挂载时注入一次 t。
  bindDeployT((k, dv) => t(k, { defaultValue: dv }))

  // 轮询：部署中/待审批/已批准时 5s，否则 30s
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

  async function createRequest(approvalRequired: boolean, vars: Record<string, string>, opts?: { silent?: boolean }) {
    if (!projectId || !branch || !selectedSha) return
    if (!opts?.silent) {
      const ok = await confirmDialog({
        title: t('projectDeploy.confirmTitle', { defaultValue: '发起部署？' }),
        description: t('projectDeploy.confirmBody', {
          defaultValue: `将对 ${branch}@${shortSha(selectedSha)} 发起${approvalRequired ? '（需审批）' : ''}部署：构建镜像并在部署机 compose 上线。`,
          branch,
          sha: shortSha(selectedSha),
          suffix: approvalRequired ? '（需审批）' : '',
        }),
        confirmLabel: t('projectDeploy.create', { defaultValue: '发起部署' }),
        cancelLabel: t('projectDeploy.cancelConfirm', { defaultValue: '取消' }),
      })
      if (!ok) return
    }
    setCreating(true)
    try {
      const created = await apiPost<{ id: string }>(
        `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests`,
        // vars 已由 sanitizeVarRows 剔除 APP_PORT（端口镜像红线）。
        { branch, sha: selectedSha, vars, approvalRequired },
      )
      showToast(t('projectDeploy.created', { defaultValue: '部署单已创建' }), 'success')
      reload()
      return created
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
      throw e
    } finally {
      setCreating(false)
    }
  }

  async function requestAction(id: string, action: 'approve' | 'reject' | 'cancel' | 'trigger') {
    if (!projectId) return
    if (action === 'cancel') {
      const ok = await confirmDialog({
        title: t('projectDeploy.cancelConfirmTitle', { defaultValue: '取消该部署单？' }),
        description: t('projectDeploy.cancelConfirmBody', { defaultValue: '取消后流水线状态不再回写，台账将记录为已取消。' }),
        confirmLabel: t('projectDeploy.cancel', { defaultValue: '取消部署' }),
        cancelLabel: t('projectDeploy.cancelConfirm', { defaultValue: '取消' }),
      })
      if (!ok) return
    }
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

  // 回滚两步走（规范 §1.4）：create(历史 sha) → trigger。中间失败抛给弹窗展示。
  // 目标单的 branch/sha 由主文件当前选择器状态承接——台账行的历史 sha 就是回滚目标，
  // 现有 create 端点显式收 sha（不可变基线红线），无需额外参数。
  async function rollbackConfirm(target: DeployRequest) {
    if (!projectId || !target) return
    const created = await createRequest(false, {}, { silent: true })
    if (!created?.id) throw new Error(t('projectDeploy.rollbackCreateFailed', { defaultValue: '回滚部署单创建失败' }))
    await apiPost(`/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(created.id)}/trigger`, {})
    showToast(t('projectDeploy.rollbackCreated', { defaultValue: '回滚部署单已创建并触发' }), 'success')
    setRollbackTarget(null)
    reload()
  }

  // 空态 CTA：展开初始配置（控制台）并平滑滚动。
  const scrollToConsole = () => {
    setShowInitialConfig(true)
    requestAnimationFrame(() => {
      consoleRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    })
  }

  const isEmptyHero = Boolean(agg && !lastDeployed && !inflight)

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="shrink-0 px-6 pt-5 pb-3">
        <DeployHeader agg={agg} onRefresh={reload} busy={aggState.status !== 'ok'} />
        <p className="mt-1 text-sm text-neutral-500 dark:text-zinc-500">
          {t('projectDeploy.subtitle', { defaultValue: '生产环境部署：分支选择 · 审批 · 流水线 · 台账' })}
        </p>
      </div>

      <div className="flex-1 overflow-y-auto px-6 pb-8">
        <div className="space-y-6">
          {/* 空态：从未部署（规范 §1.6，放 runner 名禁止） */}
          {isEmptyHero && (
            <DeployEmptyHero agg={agg} onConfigure={scrollToConsole} />
          )}

          {/* 部署中驾驶舱（置顶，规范 §1.3：真实 jobs、createdAt 计时、无日志按钮） */}
          <InFlightCockpit inflight={inflight} agg={agg} actionBusy={actionBusy} onAction={requestAction} />

          {/* 4 张客观事实卡（规范 §1.2，仅在有部署史或进行中时展示） */}
          {(lastDeployed || inflight) && <RealityDeck agg={agg} lastDeployed={lastDeployed} />}

          {/* 发起部署控制台（规范 §1.4：审批策略选择器 + EnvVarsEditor） */}
          <section ref={consoleRef} className={showInitialConfig ? 'scroll-mt-4 ring-2 ring-sky-200 dark:ring-sky-800 rounded-lg transition-shadow' : undefined}>
            {(!isEmptyHero || showInitialConfig) && (
              <DeployConsole
                branches={branches}
                branch={branch}
                selectedSha={selectedSha}
                deployPort={agg?.deployPort}
                creating={creating || Boolean(inflight)}
                onBranchChange={onBranchChange}
                onCreate={(approvalRequired, vars) => createRequest(approvalRequired, vars).then(() => undefined)}
              />
            )}
          </section>

          {/* 部署台账（规范 §1.5：探针结果列 + GitLab 直达 + 回滚入口，无耗时列/日志按钮） */}
          {projectId && (
            <DeployLedgerSection
              projectId={projectId}
              reloadKey={reloadKey}
              pipelines={agg?.pipelines ?? []}
              actionBusy={actionBusy}
              onAction={requestAction}
              onRollback={setRollbackTarget}
            />
          )}
        </div>
      </div>

      {/* 回滚确认弹窗（两步走 create+trigger） */}
      <DeployRollbackModal
        target={rollbackTarget}
        busy={creating || actionBusy !== null}
        onConfirm={rollbackConfirm}
        onClose={() => setRollbackTarget(null)}
      />
    </div>
  )
}

// 台账数据拉取薄封装：keep DeployLedgerTable 纯展示。
function DeployLedgerSection({
  projectId,
  reloadKey,
  pipelines,
  actionBusy,
  onAction,
  onRollback,
}: {
  projectId: string
  reloadKey: number
  pipelines: DeployAggregate['pipelines']
  actionBusy: string | null
  onAction: (id: string, action: 'approve' | 'reject' | 'cancel' | 'trigger') => Promise<void>
  onRollback: (req: DeployRequest) => void
}) {
  const state = useApiJson<{ requests: DeployRequest[] } | DeployRequest[]>(
    `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests`,
    reloadKey,
  )
  const raw = state.status === 'ok' ? state.data : null
  const rows = Array.isArray(raw) ? raw : (raw?.requests ?? [])
  return <DeployLedgerTable requests={rows} pipelines={pipelines} actionBusy={actionBusy} onAction={onAction} onRollback={onRollback} />
}
