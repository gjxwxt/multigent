// DeployLedgerTable：部署台账（含行内详情展开 + 回滚/触发/GitLab 操作列）。
// 规范 v2 §1.6：探针结果列用 DeployHealthCell（失败单无快照如实显示 —）；
// 回滚仅 success 行可点；触发仅 approved 行可点；不放日志按钮（日志链路待 B2）。
import { Fragment, useState } from 'react'
import { ChevronDown, ChevronRight, ExternalLink, History, PlayCircle } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import {
  cardCls,
  cardHeadCls,
  cardTitleCls,
  DeployHealthCell,
  formatTime,
  shortSha,
  StatusBadge,
  type DeployRequest,
  type PipelineSummary,
} from './deploy-shared'

export function DeployLedgerTable({
  requests,
  pipelines,
  actionBusy,
  onAction,
  onRollback,
}: {
  requests: DeployRequest[]
  pipelines: PipelineSummary[]
  actionBusy: string | null
  onAction: (id: string, action: 'approve' | 'reject' | 'cancel' | 'trigger') => Promise<void>
  onRollback: (req: DeployRequest) => void
}) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  return (
    <section className={cardCls}>
      <div className={cardHeadCls}>
        <h3 className={cardTitleCls}>{t('projectDeploy.ledgerTitle', { defaultValue: '部署台账' })}</h3>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead className="text-[11px] uppercase tracking-wide text-neutral-400 dark:text-zinc-500">
            <tr className="border-b border-neutral-100 dark:border-zinc-800">
              <th className="px-5 py-2.5 font-medium">{t('projectDeploy.colRequest', { defaultValue: '部署单' })}</th>
              <th className="px-3 py-2.5 font-medium">{t('projectDeploy.colBranch', { defaultValue: '分支' })}</th>
              <th className="px-3 py-2.5 font-medium">{t('projectDeploy.colCommit', { defaultValue: '版本' })}</th>
              <th className="px-3 py-2.5 font-medium">{t('projectDeploy.colStatus', { defaultValue: '状态' })}</th>
              <th className="px-3 py-2.5 font-medium">{t('projectDeploy.colHealth', { defaultValue: '探针结果' })}</th>
              <th className="px-3 py-2.5 font-medium">{t('projectDeploy.colActor', { defaultValue: '发起人' })}</th>
              <th className="px-3 py-2.5 font-medium">{t('projectDeploy.colCreatedAt', { defaultValue: '发起时间' })}</th>
              <th className="px-5 py-2.5 text-right font-medium">{t('projectDeploy.colActions', { defaultValue: '操作' })}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-neutral-100 dark:divide-zinc-800/70">
            {requests.length === 0 ? (
              <tr>
                <td colSpan={8} className="px-5 py-6 text-center text-neutral-400 dark:text-zinc-500">
                  {t('projectDeploy.ledgerEmpty', { defaultValue: '暂无部署记录' })}
                </td>
              </tr>
            ) : (
              requests.map((r) => {
                const isOpen = Boolean(expanded[r.id])
                const webUrl = pipelines.find((p) => p.id === r.pipelineId)?.webUrl
                return (
                  <Fragment key={r.id}>
                    <tr className="align-middle hover:bg-neutral-50/60 dark:hover:bg-zinc-800/30">
                      <td className="px-5 py-2.5">
                        <button
                          type="button"
                          onClick={() => setExpanded((s) => ({ ...s, [r.id]: !s[r.id] }))}
                          className="inline-flex items-center gap-1 font-mono text-[11px] text-neutral-700 hover:text-sky-600 dark:text-zinc-300 dark:hover:text-sky-400"
                        >
                          {isOpen ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
                          {r.id}
                        </button>
                      </td>
                      <td className="px-3 py-2.5 font-mono text-neutral-700 dark:text-zinc-300">{r.branch}</td>
                      <td className="px-3 py-2.5 font-mono text-neutral-700 dark:text-zinc-300">{shortSha(r.sha)}</td>
                      <td className="px-3 py-2.5"><StatusBadge status={r.status} /></td>
                      <td className="px-3 py-2.5"><DeployHealthCell health={r.health} /></td>
                      <td className="px-3 py-2.5 text-neutral-600 dark:text-zinc-400">{r.createdBy}</td>
                      <td className="px-3 py-2.5 text-neutral-600 dark:text-zinc-400">{formatTime(r.createdAt)}</td>
                      <td className="px-5 py-2.5">
                        <div className="flex items-center justify-end gap-1.5">
                          {webUrl && (
                            <a href={webUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-[11px] text-sky-600 hover:text-sky-700 dark:text-sky-400" title={t('projectDeploy.openGitLab', { defaultValue: '在 GitLab 中查看流水线' })}>
                              <ExternalLink className="size-3" />
                              GitLab
                            </a>
                          )}
                          {r.status === 'approved' && (
                            <button
                              type="button"
                              onClick={() => void onAction(r.id, 'trigger')}
                              disabled={actionBusy !== null}
                              className="inline-flex items-center gap-1 rounded-md border border-neutral-200 px-1.5 py-0.5 text-[11px] text-neutral-700 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
                            >
                              <PlayCircle className="size-3" />
                              {t('projectDeploy.triggerShort', { defaultValue: '触发' })}
                            </button>
                          )}
                          {r.status === 'success' && (
                            <button
                              type="button"
                              onClick={() => onRollback(r)}
                              disabled={actionBusy !== null}
                              className="inline-flex items-center gap-1 rounded-md border border-amber-200 px-1.5 py-0.5 text-[11px] text-amber-700 hover:bg-amber-50 disabled:opacity-50 dark:border-amber-800/60 dark:text-amber-400 dark:hover:bg-amber-950/40"
                            >
                              <History className="size-3" />
                              {t('projectDeploy.rollback', { defaultValue: '回滚' })}
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                    {isOpen && (
                      <tr>
                        <td colSpan={8} className="bg-neutral-50/70 px-5 py-3 dark:bg-zinc-800/40">
                          <dl className="grid grid-cols-1 gap-x-6 gap-y-1.5 text-[11px] sm:grid-cols-2">
                            <div className="flex gap-1.5">
                              <dt className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.metaTarget', { defaultValue: '目标' })}</dt>
                              <dd className="font-mono text-neutral-700 dark:text-zinc-300">{r.branch}@{shortSha(r.sha)}</dd>
                            </div>
                            <div className="flex gap-1.5">
                              <dt className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.pipelineNo', { defaultValue: '流水线' })}</dt>
                              <dd className="font-mono text-neutral-700 dark:text-zinc-300">{r.pipelineId ? `#${r.pipelineId}` : '—'}</dd>
                            </div>
                            <div className="flex gap-1.5">
                              <dt className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.metaApproval', { defaultValue: '审批' })}</dt>
                              <dd className="text-neutral-700 dark:text-zinc-300">
                                {r.approval?.required ? t('projectDeploy.approvalRequired', { defaultValue: '需审批' }) : t('projectDeploy.approvalFree', { defaultValue: '免审批' })}
                                {r.approval?.state ? ` · ${r.approval.state}` : ''}
                              </dd>
                            </div>
                            <div className="flex gap-1.5">
                              <dt className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.deploySpan', { defaultValue: '部署区间' })}</dt>
                              <dd className="font-mono text-neutral-700 dark:text-zinc-300">
                                {(r.commitSpan ?? []).map((c) => shortSha(c.sha)).join(' → ') || '—'}
                              </dd>
                            </div>
                            <div className="flex gap-1.5 sm:col-span-2">
                              <dt className="text-neutral-400 dark:text-zinc-500">{t('projectDeploy.varsTitle', { defaultValue: '部署环境变量' })}</dt>
                              <dd className="font-mono text-neutral-700 dark:text-zinc-300">
                                {r.vars && Object.keys(r.vars).length > 0
                                  ? Object.entries(r.vars).map(([k, v]) => `${k}=${v}`).join(', ')
                                  : '—'}
                              </dd>
                            </div>
                          </dl>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                )
              })
            )}
          </tbody>
        </table>
      </div>
    </section>
  )
}
