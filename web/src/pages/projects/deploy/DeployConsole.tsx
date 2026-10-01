// DeployConsole：发起部署控制台（分支/commit 选择 + 审批策略 + 环境变量）。
// 规范 v2 §1.5：两按钮分裂合并为单一审批策略选择器；approverId 定向审批待 B3 后端前置，此处不做。
import { useEffect, useMemo, useState } from 'react'
import { Rocket } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '../../../lib/cn'
import { cardCls, cardHeadCls, cardTitleCls, primaryButton, shortSha, type BranchInfo } from './deploy-shared'
import { EnvVarsEditor, sanitizeVarRows, type DeployVarRow } from './EnvVarsEditor'

export function DeployConsole({
  branches,
  branch,
  selectedSha,
  deployPort,
  creating,
  onBranchChange,
  onCreate,
}: {
  branches: BranchInfo[]
  branch: string
  selectedSha: string
  deployPort?: number
  creating: boolean
  onBranchChange: (name: string) => void
  onCreate: (approvalRequired: boolean, vars: Record<string, string>) => Promise<void>
}) {
  const { t } = useTranslation()
  const [approvalRequired, setApprovalRequired] = useState(false)
  const branchInfo = useMemo(() => branches.find((b) => b.name === branch) ?? null, [branches, branch])
  const commitOptions = useMemo(
    // BranchInfo 自带 commitId/commitTitle——单分支只有 tip 一个可选项，如实展示。
    () => (branchInfo ? [{ sha: branchInfo.commitId, title: branchInfo.commitTitle }] : []),
    [branchInfo],
  )
  // 规范 v2 §1.5：系统预留行 APP_PORT 灰底锁定只读展示真实分配端口；提交时 sanitizeVarRows 剔除。
  // deployPort 来自聚合端点异步到达，useState 初始化器只跑一次会漏种——用 effect 在到达时补种。
  const [varRows, setVarRows] = useState<DeployVarRow[]>([])
  useEffect(() => {
    if (!deployPort) return
    setVarRows((rows) =>
      rows.some((r) => r.locked) ? rows : [{ key: 'APP_PORT', value: String(deployPort), locked: true }, ...rows],
    )
  }, [deployPort])

  const submit = () => {
    const vars = sanitizeVarRows(varRows)
    void onCreate(approvalRequired, vars)
  }

  return (
    <section className={cardCls}>
      <div className={cardHeadCls}>
        <h3 className={cardTitleCls}>{t('projectDeploy.consoleTitle', { defaultValue: '发起部署' })}</h3>
      </div>
      <div className="space-y-4 p-5">
        {/* 分支与版本 */}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label className="block">
            <span className="mb-1 block text-xs font-medium text-neutral-500 dark:text-zinc-400">
              {t('projectDeploy.colBranch', { defaultValue: '分支' })}
            </span>
            <select
              value={branch}
              onChange={(e) => onBranchChange(e.target.value)}
              disabled={branches.length === 0}
              className="w-full rounded-lg border border-neutral-200 bg-white px-3 py-2 text-sm text-neutral-800 outline-none focus:border-sky-400 disabled:bg-neutral-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200 dark:disabled:bg-zinc-800/60"
            >
              {branches.length === 0 ? (
                <option value="">{t('projectDeploy.branchesUnavailable', { defaultValue: '当前项目尚未接入远端 Git 仓库' })}</option>
              ) : (
                branches.map((b) => (
                  <option key={b.name} value={b.name}>
                    {b.name}{b.isDefault ? t('projectDeploy.defaultBranch', { defaultValue: '（默认）' }) : ''}
                  </option>
                ))
              )}
            </select>
            {branches.length === 0 && (
              <p className="mt-1 text-xs leading-relaxed text-neutral-400 dark:text-zinc-500">
                {t('projectDeploy.branchesUnavailableHint', {
                  defaultValue: '部署流水线由 GitLab CI 驱动：需先将仓库镜像推送到 GitLab，并在项目设置完成远端验证（remote verify）。',
                })}
              </p>
            )}
          </label>
          <label className="block">
            <span className="mb-1 block text-xs font-medium text-neutral-500 dark:text-zinc-400">
              {t('projectDeploy.colCommit', { defaultValue: '版本' })}
            </span>
            <select
              value={selectedSha}
              onChange={() => {/* 单选项占位：commit 明细清单待 B4 compare API */}}
              disabled={commitOptions.length === 0}
              className="w-full rounded-lg border border-neutral-200 bg-white px-3 py-2 text-sm text-neutral-800 outline-none focus:border-sky-400 disabled:bg-neutral-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200 dark:disabled:bg-zinc-800/60"
            >
              {commitOptions.length === 0 ? (
                <option value="">{t('projectDeploy.noCommits', { defaultValue: '（无可用提交）' })}</option>
              ) : (
                commitOptions.map((c) => (
                  <option key={c.sha} value={c.sha}>
                    {shortSha(c.sha)} {c.title}
                  </option>
                ))
              )}
            </select>
          </label>
        </div>

        {/* 审批策略（单 select，替代原型两按钮分裂） */}
        <label className="block">
          <span className="mb-1 block text-xs font-medium text-neutral-500 dark:text-zinc-400">
            {t('projectDeploy.approvalPolicy', { defaultValue: '审批策略' })}
          </span>
          <select
            value={approvalRequired ? 'required' : 'free'}
            onChange={(e) => setApprovalRequired(e.target.value === 'required')}
            className="w-full rounded-lg border border-neutral-200 bg-white px-3 py-2 text-sm text-neutral-800 outline-none focus:border-sky-400 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200"
          >
            <option value="free">{t('projectDeploy.approvalFreeOption', { defaultValue: '免审批 · 直接进入部署' })}</option>
            <option value="required">{t('projectDeploy.approvalRequiredOption', { defaultValue: '需审批 · 审批人批准后自动触发' })}</option>
          </select>
        </label>

        {/* 环境变量 */}
        <div>
          <span className="mb-1.5 block text-xs font-medium text-neutral-500 dark:text-zinc-400">
            {t('projectDeploy.varsTitle', { defaultValue: '部署环境变量' })}
          </span>
          <EnvVarsEditor rows={varRows} onChange={setVarRows} />
        </div>

        <div className="flex items-center justify-end border-t border-neutral-100 pt-3 dark:border-zinc-800">
          <button
            type="button"
            onClick={submit}
            disabled={creating || !branch || !selectedSha}
            className={cn(primaryButton)}
          >
            <Rocket className="size-4" />
            {creating
              ? t('projectDeploy.creating', { defaultValue: '发起中…' })
              : t('projectDeploy.launch', { defaultValue: '发起部署' })}
          </button>
        </div>
      </div>
    </section>
  )
}
