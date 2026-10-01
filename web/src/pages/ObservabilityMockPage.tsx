import { useTranslation } from 'react-i18next'
import { Activity, AlertTriangle, CheckCircle2, CircleDot, ExternalLink, RadioTower } from 'lucide-react'
import { cn } from '../lib/cn'
import {
  alertEvents,
  apiErrorTrend,
  frontendErrorRows,
  serviceRows,
  sloCards,
  sliTone,
} from '../mocks/observability'

const cardCls = 'rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40'
const cardHeadCls = 'border-b border-neutral-100 px-5 py-3 dark:border-zinc-800'
const cardTitleCls = 'text-sm font-semibold text-neutral-800 dark:text-zinc-100'

function Sparkline({ points, tone }: { points: number[]; tone: 'good' | 'warn' | 'bad' }) {
  const max = Math.max(...points, 0.001)
  const width = 120
  const height = 28
  const step = width / (points.length - 1)
  const path = points.map((p, i) => `${i === 0 ? 'M' : 'L'}${(i * step).toFixed(1)},${(height - (p / max) * (height - 4) - 2).toFixed(1)}`).join(' ')
  const stroke = tone === 'good' ? 'stroke-emerald-500' : tone === 'warn' ? 'stroke-amber-500' : 'stroke-red-500'
  return (
    <svg width={width} height={height} className="shrink-0">
      <path d={path} fill="none" strokeWidth="1.5" className={stroke} />
    </svg>
  )
}

function SliBadge({ tone, children }: { tone: 'good' | 'warn' | 'bad'; children: React.ReactNode }) {
  const cls =
    tone === 'good'
      ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-300'
      : tone === 'warn'
        ? 'bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-300'
        : 'bg-red-100 text-red-800 dark:bg-red-950/60 dark:text-red-300'
  return <span className={cn('inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium', cls)}>{children}</span>
}

export default function ObservabilityMockPage() {
  const { t } = useTranslation()

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="shrink-0 px-6 pt-5 pb-3">
        <div className="flex items-center justify-between">
          <div>
            <h1 className="flex items-center gap-2 text-xl font-semibold text-neutral-900 dark:text-zinc-100">
              <Activity className="size-5 text-sky-600 dark:text-sky-400" />
              {t('observability.title', { defaultValue: '监控中心' })}
            </h1>
            <p className="mt-0.5 text-sm text-neutral-500 dark:text-zinc-500">
              {t('observability.subtitle', { defaultValue: '服务可用性 · 接口成功率 SLO · 前端报错 · 告警事件' })}
            </p>
          </div>
          <span className="inline-flex items-center gap-1.5 rounded-full bg-amber-100 px-3 py-1 text-xs font-medium text-amber-800 dark:bg-amber-950/60 dark:text-amber-300">
            <RadioTower className="size-3.5" />
            {t('observability.mockBadge', { defaultValue: '演示数据（未接真实数据源）' })}
          </span>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto px-6 pb-8">
        <div className="space-y-6">
          {/* SLO 卡片行：97/95/99 口径 = 接口成功率目标 vs 当前 */}
          <section className="grid grid-cols-1 gap-4 md:grid-cols-3">
            {sloCards.map((c) => {
              const tone = sliTone(c.current, c.target)
              return (
                <div key={c.api} className={cn(cardCls, 'p-4')}>
                  <div className="text-xs font-medium text-neutral-500 dark:text-zinc-400">{c.api}</div>
                  <div className="mt-2 flex items-baseline gap-2">
                    <span
                      className={cn(
                        'text-2xl font-bold font-mono',
                        tone === 'good' ? 'text-emerald-600 dark:text-emerald-400' : tone === 'warn' ? 'text-amber-600 dark:text-amber-400' : 'text-red-600 dark:text-red-400',
                      )}
                    >
                      {c.current.toFixed(2)}%
                    </span>
                    <span className="text-xs text-neutral-400 dark:text-zinc-500">/ 目标 {c.target}%</span>
                  </div>
                  <div className="mt-2 flex items-center justify-between">
                    <span className="text-[11px] text-neutral-400 dark:text-zinc-500">{c.window}</span>
                    <SliBadge tone={tone}>{tone === 'good' ? '达标' : tone === 'warn' ? '接近阈值' : '未达标'}</SliBadge>
                  </div>
                  <div className="mt-2 border-t border-neutral-100 pt-2 dark:border-zinc-800">
                    <Sparkline points={apiErrorTrend[c.api.split('（')[0]] ?? [1, 1, 1]} tone={tone} />
                  </div>
                </div>
              )
            })}
          </section>

          {/* 服务可用性 */}
          <section className={cardCls}>
            <div className={cn(cardHeadCls, 'flex items-center justify-between')}>
              <h3 className={cardTitleCls}>{t('observability.services', { defaultValue: '服务可用性' })}</h3>
              <span className="text-xs text-neutral-400 dark:text-zinc-500">{serviceRows.filter((s) => s.online).length}/{serviceRows.length} 在线</span>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <thead>
                  <tr className="border-b border-neutral-200/80 bg-neutral-50/80 text-xs uppercase tracking-wide text-neutral-500 dark:border-zinc-700/60 dark:bg-zinc-900/40 dark:text-zinc-500">
                    <th className="px-5 py-2.5 font-medium">服务</th>
                    <th className="px-4 py-2.5 font-medium">地址</th>
                    <th className="px-4 py-2.5 font-medium">状态</th>
                    <th className="px-4 py-2.5 font-medium">延迟</th>
                    <th className="px-4 py-2.5 font-medium">最近探测</th>
                    <th className="px-4 py-2.5 font-medium">30 天可用率</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
                  {serviceRows.map((s) => (
                    <tr key={s.name} className="transition-colors hover:bg-sky-50/40 dark:hover:bg-sky-900/[0.06]">
                      <td className="px-5 py-2.5 font-medium text-neutral-800 dark:text-zinc-200">{s.name}</td>
                      <td className="px-4 py-2.5 font-mono text-xs text-sky-600 dark:text-sky-400">
                        <a href={s.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:underline">
                          {s.url}
                          <ExternalLink className="size-3" />
                        </a>
                      </td>
                      <td className="px-4 py-2.5">
                        {s.online ? (
                          <span className="inline-flex items-center gap-1.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
                            <span className="size-1.5 rounded-full bg-emerald-500" />
                            UP
                          </span>
                        ) : (
                          <span className="inline-flex items-center gap-1.5 text-xs font-medium text-red-600 dark:text-red-400">
                            <span className="size-1.5 rounded-full bg-red-500" />
                            DOWN
                          </span>
                        )}
                      </td>
                      <td className="px-4 py-2.5 font-mono text-xs text-neutral-600 dark:text-zinc-400">{s.online ? `${s.latencyMs}ms` : '—'}</td>
                      <td className="px-4 py-2.5 text-xs text-neutral-500 dark:text-zinc-500">{s.lastProbe}</td>
                      <td className="px-4 py-2.5 font-mono text-xs text-neutral-600 dark:text-zinc-400">{s.uptime30d.toFixed(2)}%</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          {/* 前端报错 + 告警事件 双列 */}
          <section className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <div className={cardCls}>
              <div className={cardHeadCls}>
                <h3 className={cardTitleCls}>{t('observability.frontendErrors', { defaultValue: '前端报错 Top' })}</h3>
              </div>
              <div className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
                {frontendErrorRows.map((e) => (
                  <div key={e.message} className="flex items-start justify-between gap-3 px-5 py-3">
                    <div className="min-w-0">
                      <div className="truncate font-mono text-xs text-neutral-800 dark:text-zinc-200">{e.message}</div>
                      <div className="mt-1 flex items-center gap-3 text-[11px] text-neutral-400 dark:text-zinc-500">
                        <span>{e.count} 次</span>
                        <span>最近 {e.lastSeen}</span>
                        <span className="rounded bg-neutral-100 px-1.5 py-0.5 font-mono dark:bg-zinc-800">{e.release}</span>
                      </div>
                    </div>
                    <Sparkline points={e.trend} tone="warn" />
                  </div>
                ))}
              </div>
            </div>

            <div className={cardCls}>
              <div className={cardHeadCls}>
                <h3 className={cardTitleCls}>{t('observability.alerts', { defaultValue: '最近告警事件' })}</h3>
                <span className="text-[11px] text-neutral-400 dark:text-zinc-500">
                  {t('observability.alertBridgeHint', { defaultValue: '上线 alert-bridge 后：agent 可认领排障任务' })}
                </span>
              </div>
              <div className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
                {alertEvents.map((a) => (
                  <div key={a.id} className="flex items-start justify-between gap-3 px-5 py-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span
                          className={cn(
                            'rounded px-1.5 py-0.5 text-[10px] font-bold',
                            a.severity === 'P1'
                              ? 'bg-red-100 text-red-700 dark:bg-red-950/60 dark:text-red-300'
                              : a.severity === 'P2'
                                ? 'bg-amber-100 text-amber-700 dark:bg-amber-950/60 dark:text-amber-300'
                                : 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-300',
                          )}
                        >
                          {a.severity}
                        </span>
                        <span className="font-mono text-xs text-neutral-700 dark:text-zinc-300">{a.service}</span>
                        <span className="text-[11px] text-neutral-400 dark:text-zinc-500">{a.at}</span>
                      </div>
                      <div className="mt-1 text-xs text-neutral-600 dark:text-zinc-400">{a.summary}</div>
                    </div>
                    <span className="flex shrink-0 items-center gap-1 text-[11px]">
                      {a.status === 'resolved' ? (
                        <span className="inline-flex items-center gap-1 text-emerald-600 dark:text-emerald-400">
                          <CheckCircle2 className="size-3" />
                          已恢复
                        </span>
                      ) : a.status === 'acknowledged' ? (
                        <span className="inline-flex items-center gap-1 text-sky-600 dark:text-sky-400">
                          <CircleDot className="size-3" />
                          {a.claimedBy ?? '已认领'}
                        </span>
                      ) : (
                        <span className="inline-flex items-center gap-1 text-red-600 dark:text-red-400">
                          <AlertTriangle className="size-3" />
                          触发中
                        </span>
                      )}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          </section>
        </div>
      </div>
    </div>
  )
}
