// RealityDeck：4 张客观事实卡。
// 规范 v2 §1.2 禁止项：runner 节点名 / IP / 部署耗时 / 容器实例数——后端无此数据；
// 第 4 卡 = 最近流水线（真实 id/状态/webUrl），替代 v1 的"访问应用"大按钮卡（已上移 Header）。
import { Activity, GitCommitHorizontal, History, Tag } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '../../../lib/cn'
import {
  cardCls,
  formatRelativeTime,
  formatTime,
  pipelineStatusTextCls,
  shortSha,
  type DeployAggregate,
} from './deploy-shared'

function CardShell({ label, icon, children }: { label: string; icon: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className={cn(cardCls, 'p-4')}>
      <div className="flex items-center justify-between text-xs text-neutral-500 dark:text-zinc-400">
        <span>{label}</span>
        {icon}
      </div>
      {children}
    </div>
  )
}

export function RealityDeck({ agg, lastDeployed }: { agg: DeployAggregate | null; lastDeployed: DeployAggregate['lastDeployed'] }) {
  const { t } = useTranslation()
  const health = agg?.health ?? null
  const up = health?.status === 'up'
  const unknown = !health || health.status === 'unknown'
  const latestPipeline = agg?.pipelines?.[0] ?? null

  return (
    <section className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <CardShell label={t('projectDeploy.liveVersion', { defaultValue: '运行版本' })} icon={<Tag className="size-4" />}>
        {lastDeployed ? (
          <>
            <div className="mt-2 flex items-baseline gap-2">
              <span className="font-mono text-xl font-bold text-neutral-900 dark:text-zinc-100">{shortSha(lastDeployed.sha)}</span>
              <span className="rounded bg-sky-50 px-1.5 py-0.5 font-mono text-xs text-sky-700 dark:bg-sky-950/60 dark:text-sky-400">{lastDeployed.branch}</span>
            </div>
            <p className="mt-2 text-[11px] text-neutral-400 dark:text-zinc-500">
              {t('projectDeploy.deployedBy', { defaultValue: '部署于' })} {formatRelativeTime(lastDeployed.createdAt)}（{formatTime(lastDeployed.createdAt)}）· {lastDeployed.createdBy}
            </p>
          </>
        ) : (
          <p className="mt-2 text-lg font-semibold text-neutral-400 dark:text-zinc-500">{t('projectDeploy.neverDeployed', { defaultValue: '尚未部署' })}</p>
        )}
      </CardShell>

      <CardShell label={t('projectDeploy.health', { defaultValue: '健康探针' })} icon={<Activity className="size-4" />}>
        <div className="mt-2">
          {unknown ? (
            <span className="inline-flex items-center gap-1.5 text-lg font-semibold text-neutral-500 dark:text-zinc-400">
              {t('projectDeploy.healthUnknown', { defaultValue: '未知' })}
            </span>
          ) : up ? (
            <span className="inline-flex items-center gap-1.5 text-lg font-semibold text-emerald-600 dark:text-emerald-400">
              <span className="relative flex size-2.5">
                <span className="absolute inline-flex size-full animate-ping rounded-full bg-emerald-400 opacity-60" />
                <span className="relative inline-flex size-2.5 rounded-full bg-emerald-500" />
              </span>
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
      </CardShell>

      <CardShell label={t('projectDeploy.hostPort', { defaultValue: '部署机 & 端口' })} icon={<GitCommitHorizontal className="size-4" />}>
        <div className="mt-2 flex items-baseline gap-2">
          <span className="font-mono text-xl font-bold text-neutral-900 dark:text-zinc-100">:{agg?.deployPort ?? '—'}</span>
        </div>
        <p className="mt-2 truncate text-[11px] text-neutral-400 dark:text-zinc-500">
          {agg?.deployHostConfigured
            ? t('projectDeploy.hostConfigured', { defaultValue: '部署机已配置' })
            : t('projectDeploy.hostNotConfigured', { defaultValue: '未配置部署机地址（MULTIGENT_DEPLOY_HOST）' })}
        </p>
      </CardShell>

      <CardShell label={t('projectDeploy.latestPipeline', { defaultValue: '最近流水线' })} icon={<History className="size-4" />}>
        {latestPipeline ? (
          <>
            <div className="mt-2 flex items-baseline gap-2">
              <span className="font-mono text-xl font-bold text-neutral-900 dark:text-zinc-100">#{latestPipeline.id}</span>
              <span className={cn('font-mono text-xs font-semibold', pipelineStatusTextCls(latestPipeline.status))}>{latestPipeline.status}</span>
            </div>
            <p className="mt-2 truncate font-mono text-[11px] text-neutral-400 dark:text-zinc-500">
              {latestPipeline.ref} · {shortSha(latestPipeline.sha)}
            </p>
          </>
        ) : (
          <p className="mt-2 text-[11px] text-neutral-400 dark:text-zinc-500">{t('projectDeploy.noPipelines', { defaultValue: '暂无流水线' })}</p>
        )}
      </CardShell>
    </section>
  )
}
