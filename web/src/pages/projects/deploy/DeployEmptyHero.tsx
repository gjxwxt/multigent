// DeployEmptyHero：从未部署且无进行中部署单时的首屏引导。
// 规范 v2 §1.2：不放 runner 名/IP（部署拓扑属部署环境配置）；点击 CTA 平滑滚动到初始化配置卡。
import { ArrowDown, Cable, Rocket, ServerCog } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { DeployAggregate } from './deploy-shared'

export function DeployEmptyHero({ agg, onConfigure }: { agg: DeployAggregate | null; onConfigure: () => void }) {
  const { t } = useTranslation()

  const steps = [
    {
      icon: ServerCog,
      title: t('projectDeploy.heroStepPort', { defaultValue: '预分配端口' }),
      body: t('projectDeploy.heroStepPortBody', { defaultValue: '保存项目时自动分配部署端口并镜像到 CI 变量' }),
    },
    {
      icon: Cable,
      title: t('projectDeploy.heroStepBind', { defaultValue: '远端绑定' }),
      body: t('projectDeploy.heroStepBindBody', { defaultValue: '项目绑定 Git 仓库与部署机 Compose 目录' }),
    },
    {
      icon: Rocket,
      title: t('projectDeploy.heroStepLaunch', { defaultValue: '一键上线' }),
      body: t('projectDeploy.heroStepLaunchBody', { defaultValue: '发起部署单，流水线构建镜像并 Compose 上线' }),
    },
  ]

  return (
    <section className="flex min-h-[75vh] flex-col items-center justify-center py-12 text-center">
      <div className="flex size-14 items-center justify-center rounded-2xl bg-sky-100 dark:bg-sky-950/60">
        <Rocket className="size-7 text-sky-600 dark:text-sky-400" />
      </div>
      <h2 className="mt-4 text-lg font-semibold text-neutral-900 dark:text-zinc-100">
        {t('projectDeploy.heroTitle', { defaultValue: '让这个项目跑起来' })}
      </h2>
      <p className="mt-1.5 max-w-md text-sm leading-relaxed text-neutral-500 dark:text-zinc-400">
        {t('projectDeploy.heroBody', {
          defaultValue: '配置部署端口与远端目录后，即可发起第一张部署单：构建镜像、Compose 上线、探针验活，全程留痕可审计。',
        })}
      </p>

      <div className="mt-8 grid w-full max-w-2xl grid-cols-1 gap-3 sm:grid-cols-3">
        {steps.map((s) => (
          <div key={s.title} className="rounded-lg border border-neutral-200/80 bg-white p-4 text-left dark:border-zinc-700/60 dark:bg-zinc-900/40">
            <s.icon className="size-4 text-sky-600 dark:text-sky-400" />
            <h3 className="mt-2 text-xs font-semibold text-neutral-800 dark:text-zinc-200">{s.title}</h3>
            <p className="mt-1 text-[11px] leading-relaxed text-neutral-500 dark:text-zinc-500">{s.body}</p>
          </div>
        ))}
      </div>

      {agg && (
        <button
          type="button"
          onClick={onConfigure}
          className="mt-8 inline-flex items-center gap-2 rounded-lg bg-sky-600 px-5 py-2.5 text-sm font-medium text-white hover:bg-sky-700"
        >
          <ArrowDown className="size-4" />
          {t('projectDeploy.heroConfigure', { defaultValue: '开始配置' })}
        </button>
      )}
    </section>
  )
}
