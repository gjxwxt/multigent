// 部署中心 Header：标题 + 健康徽章 + 刷新/访问应用。
// 规范 v2 §1.1：探活内嵌 aggregate，"刷新状态"就是重新 GET；不放独立的 /deploy/probe（端点不存在）。
import { ExternalLink, RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '../../../lib/cn'
import { neutralButton, type DeployAggregate } from './deploy-shared'

export function DeployHeader({
  agg,
  onRefresh,
  busy,
}: {
  agg: DeployAggregate | null
  onRefresh: () => void
  busy: boolean
}) {
  const { t } = useTranslation()
  const health = agg?.health ?? null
  const up = health?.status === 'up'
  const down = health?.status === 'down'
  const unknown = !up && !down
  // 访问应用：部署端口经宿主发布，与控制台同主机可达（部署拓扑实测口径）。
  const appUrl = agg?.deployHostConfigured && agg.deployPort ? `http://${window.location.hostname}:${agg.deployPort}` : null

  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold text-neutral-900 dark:text-zinc-100">
          {t('projectDeploy.headerTitle', { defaultValue: '生产环境部署' })}
        </h1>
        {up && (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-50 px-2.5 py-1 text-[11px] font-semibold text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-300">
            <span className="relative flex size-2">
              <span className="absolute inline-flex size-full animate-ping rounded-full bg-emerald-400 opacity-60" />
              <span className="relative inline-flex size-2 rounded-full bg-emerald-500" />
            </span>
            {t('projectDeploy.liveBadge', { defaultValue: 'Production Live' })}
          </span>
        )}
        {down && (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-red-50 px-2.5 py-1 text-[11px] font-semibold text-red-700 dark:bg-red-950/60 dark:text-red-300">
            <span className="size-2 rounded-full bg-red-500" />
            {t('projectDeploy.degradedBadge', { defaultValue: 'Service Degraded' })}
          </span>
        )}
        {unknown && (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-neutral-100 px-2.5 py-1 text-[11px] font-semibold text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
            {t('projectDeploy.unknownBadge', { defaultValue: '探针未配置' })}
          </span>
        )}
      </div>
      <div className="flex items-center gap-2">
        <button type="button" onClick={onRefresh} disabled={busy} className={neutralButton}>
          <RefreshCw className={cn('size-3.5', busy && 'animate-spin')} />
          {t('projectDeploy.refreshStatus', { defaultValue: '刷新状态' })}
        </button>
        {appUrl ? (
          <a
            href={appUrl}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-2 rounded-lg bg-sky-600 px-3.5 py-1.5 text-sm font-medium text-white shadow-sm hover:bg-sky-700"
          >
            <ExternalLink className="size-3.5" />
            {t('projectDeploy.openAppHeader', { defaultValue: '访问运行中应用' })}
          </a>
        ) : (
          <span
            title={t('projectDeploy.hostNotConfiguredShort', { defaultValue: '未配置部署机地址' })}
            className="inline-flex cursor-not-allowed items-center gap-2 rounded-lg border border-neutral-200 bg-neutral-50 px-3.5 py-1.5 text-sm text-neutral-400 dark:border-zinc-700 dark:bg-zinc-800/60 dark:text-zinc-500"
          >
            <ExternalLink className="size-3.5" />
            {t('projectDeploy.openAppHeader', { defaultValue: '访问运行中应用' })}
          </span>
        )}
      </div>
    </div>
  )
}
