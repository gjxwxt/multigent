import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, CircleCheck, CircleDashed } from 'lucide-react'
import { cn } from '../../lib/cn'
import { evaluateDeliveryNotice, remoteDependentSteps, type ProjectRemoteState, type WorkflowStepLike } from '../../lib/delivery-mode'

/** Structural subset of the dialog's own workflow type, so this component does
 * not have to import from its caller (and the caller can keep richer fields). */
type WorkflowLike = { id: string; name: string; steps?: WorkflowStepLike[] }

type Props = {
  /** Steps of the workflow about to be used, including the template of a chosen task template. */
  steps?: WorkflowStepLike[]
  project?: ProjectRemoteState
  workflows: WorkflowLike[]
  onSelectWorkflow: (id: string) => void
}

const TONE_STYLES: Record<string, string> = {
  green: 'border-emerald-200 bg-emerald-50/70 text-emerald-900 dark:border-emerald-900/60 dark:bg-emerald-950/30 dark:text-emerald-200',
  amber: 'border-amber-300 bg-amber-50/80 text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-200',
  red: 'border-red-300 bg-red-50/90 text-red-900 dark:border-red-900/70 dark:bg-red-950/35 dark:text-red-200',
  neutral: 'border-neutral-200 bg-neutral-50 text-neutral-700 dark:border-zinc-700 dark:bg-zinc-950/40 dark:text-zinc-300',
}

function stepLabels(steps: { id: string; title?: string }[], t: (k: string, o?: Record<string, unknown>) => string): string {
  return steps.map((s) => s.title || t(`workflows.deliveryMode.step.${s.id}`, { defaultValue: s.id })).join('、')
}

export function DeliveryModeNotice({ steps, project, workflows, onSelectWorkflow }: Props) {
  const { t } = useTranslation()
  const notice = useMemo(() => evaluateDeliveryNotice(steps, project), [steps, project])

  // Templates that declare no remote dependency, offered as a way out of the
  // warning rather than as a verdict that the current one is wrong.
  const localOptions = useMemo(
    () => workflows.filter((wf) => wf.id !== 'project-initialization-v1' && remoteDependentSteps(wf.steps).length === 0),
    [workflows],
  )

  if (notice.state === 'none') return null

  if (notice.state === 'undeclared') {
    return (
      <p className="mt-1 flex items-start gap-1.5 text-xs text-neutral-500 dark:text-zinc-400">
        <CircleDashed className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
        {t('workflows.deliveryMode.undeclared')}
      </p>
    )
  }

  const blocking = notice.tone === 'red'
  const Icon = notice.tone === 'green' ? CircleCheck : AlertTriangle
  const label = stepLabels(notice.steps, t)

  return (
    <div
      role={blocking ? 'alert' : 'status'}
      className={cn('mt-2 rounded-lg border p-3 text-xs', TONE_STYLES[notice.tone] ?? TONE_STYLES.neutral)}
    >
      <div className="flex items-start gap-2">
        <Icon className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
        <div className="min-w-0 space-y-1">
          <p className="font-medium">
            {t(`workflows.deliveryMode.${notice.state}.title`)}
          </p>
          <p className="leading-relaxed">
            {t(`workflows.deliveryMode.${notice.state}.body`, { steps: label })}
          </p>
          {notice.state === 'ready' && notice.namespace ? (
            <p className="font-mono text-[11px] opacity-80">{notice.namespace}</p>
          ) : null}
          {notice.state === 'localBranch' ? (
            <p className="leading-relaxed opacity-90">{t('workflows.deliveryMode.pushStillWorks')}</p>
          ) : null}
          {notice.state === 'bindingUnknown' ? (
            <p className="leading-relaxed opacity-90">{t('workflows.deliveryMode.bindingUnknownHint')}</p>
          ) : null}

          {notice.tone !== 'green' && localOptions.length > 0 ? (
            <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
              {localOptions.map((wf) => (
                <button
                  key={wf.id}
                  type="button"
                  onClick={() => onSelectWorkflow(wf.id)}
                  className="rounded border border-current/30 px-1.5 py-0.5 text-[11px] hover:opacity-80 focus:outline-none focus:ring-1 focus:ring-current"
                >
                  {wf.name}
                </button>
              ))}
              <button
                type="button"
                onClick={() => onSelectWorkflow('')}
                className="rounded border border-current/30 px-1.5 py-0.5 text-[11px] hover:opacity-80 focus:outline-none focus:ring-1 focus:ring-current"
              >
                {t('workflows.noWorkflow')}
              </button>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  )
}
