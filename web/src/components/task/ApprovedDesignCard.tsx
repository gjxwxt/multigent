import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ExternalLink, Palette, RefreshCw } from 'lucide-react'
import { apiFetch } from '../../lib/api'
import { cn } from '../../lib/cn'

type DesignStatusResponse = {
  projectId: string
  runStatus?: string
  proxyUrl?: string
  launchUrl?: string
}

/**
 * The design gate freezes the approved design into the workflow outputs at
 * confirm time; downstream steps carry them as inputs. Values survive as
 * instance outputValues (structured) or as the outputArtifact JSON.
 */
export type ApprovedDesignValues = {
  projectId: string
  source: string
  html: string
  snapshotPath: string
}

type StepOutputsLike = { outputValues?: Record<string, string> | null; outputArtifact?: string | null }

function designValuesFromRecord(values: Record<string, string>): ApprovedDesignValues | null {
  const projectId = String(values.approved_design_project_id ?? '').trim()
  if (!projectId) return null
  return {
    projectId,
    source: String(values.approved_design_source ?? '').trim(),
    html: String(values.approved_design_html ?? '').trim(),
    snapshotPath: String(values.approved_design_snapshot_path ?? '').trim(),
  }
}

// Newest step wins: scan from the end so a re-run design gate supersedes
// an older confirmation.
export function approvedDesignValuesFromSteps(steps: StepOutputsLike[] | undefined): ApprovedDesignValues | null {
  if (!steps?.length) return null
  for (let i = steps.length - 1; i >= 0; i--) {
    const step = steps[i]
    const values = step.outputValues && Object.keys(step.outputValues).length > 0
      ? step.outputValues
      : parseOutputArtifact(step.outputArtifact)
    const found = designValuesFromRecord(values)
    if (found) return found
  }
  return null
}

function parseOutputArtifact(artifact: string | null | undefined): Record<string, string> {
  const text = String(artifact ?? '').trim()
  if (!text.startsWith('{')) return {}
  try {
    const parsed = JSON.parse(text) as unknown
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      const out: Record<string, string> = {}
      for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
        out[key] = String(value ?? '')
      }
      return out
    }
  } catch {
    // Not JSON — no design values here.
  }
  return {}
}

/**
 * Follow-panel card for a confirmed design gate: shows the OpenDesign project
 * id plus a "view design" link. The link needs a fresh signed odt token, so it
 * is minted on view from design/status (tokens expire after 4h).
 */
export function ApprovedDesignCard({ project, taskID, values, className }: {
  project: string
  taskID: string
  values: ApprovedDesignValues
  className?: string
}) {
  const { t } = useTranslation()
  const [linkState, setLinkState] = useState<'loading' | 'ok' | 'error'>('loading')
  const [launchUrl, setLaunchUrl] = useState('')
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => {
    let cancelled = false
    setLinkState('loading')
    apiFetch<DesignStatusResponse>(
      `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/status`,
      { silentStatuses: [404, 500, 502, 503, 504] },
    )
      .then((data) => {
        if (cancelled) return
        if (data?.launchUrl) {
          setLaunchUrl(data.launchUrl)
          setLinkState('ok')
        } else {
          setLinkState('error')
        }
      })
      .catch(() => {
        if (!cancelled) setLinkState('error')
      })
    return () => {
      cancelled = true
    }
  }, [project, taskID, reloadKey])

  return (
    <section className={cn('mx-4 mb-4 rounded-xl border border-neutral-200 bg-white px-4 py-4 dark:border-zinc-700 dark:bg-zinc-950', className)}>
      <div className="flex flex-wrap items-center gap-2">
        <Palette className="size-4 shrink-0 text-sky-600 dark:text-sky-400" />
        <h4 className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
          {t('workflows.designGate.approved.title', { defaultValue: '已确认设计' })}
        </h4>
        {values.source === 'existing' && (
          <span className="rounded-full border border-neutral-200 bg-neutral-50 px-2 py-0.5 text-[11px] font-medium text-neutral-500 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400">
            {t('workflows.designGate.approved.sourceExisting', { defaultValue: '来源：已有设计' })}
          </span>
        )}
        {values.source === 'generated' && (
          <span className="rounded-full border border-neutral-200 bg-neutral-50 px-2 py-0.5 text-[11px] font-medium text-neutral-500 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400">
            {t('workflows.designGate.approved.sourceGenerated', { defaultValue: '来源：OD 生成' })}
          </span>
        )}
      </div>
      <div className="mt-2.5 space-y-1.5">
        <div className="flex items-baseline gap-2">
          <span className="shrink-0 text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.designGate.approved.projectId', { defaultValue: 'OpenDesign 项目' })}</span>
          <span className="break-all font-mono text-xs text-neutral-800 dark:text-zinc-200">{values.projectId}</span>
        </div>
        {values.snapshotPath && (
          <div className="flex items-baseline gap-2">
            <span className="shrink-0 text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.designGate.approved.snapshotPath', { defaultValue: '快照包' })}</span>
            <span className="break-all font-mono text-xs text-neutral-600 dark:text-zinc-400">{values.snapshotPath}</span>
          </div>
        )}
      </div>
      {values.html && (
        <p className="mt-2 flex items-start gap-1.5 text-xs leading-relaxed text-neutral-500 dark:text-zinc-400">
          <span className="mt-0.5 inline-block size-1.5 shrink-0 rounded-full bg-emerald-500" />
          {t('workflows.designGate.approved.frozenHtml', { defaultValue: '已冻结设计 HTML 快照，实现节点使用该副本，不受 OD 后续修改影响。' })}
        </p>
      )}
      <div className="mt-3 flex items-center gap-2">
        {linkState === 'ok' && (
          <a
            href={launchUrl}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1.5 rounded-lg border border-sky-600/30 bg-sky-50 px-3 py-1.5 text-xs font-semibold text-sky-700 transition hover:bg-sky-100 dark:border-sky-500/30 dark:bg-sky-950/40 dark:text-sky-300 dark:hover:bg-sky-950/70"
          >
            <ExternalLink className="size-3.5" />
            {t('workflows.designGate.approved.view', { defaultValue: '查看设计' })}
          </a>
        )}
        {linkState === 'loading' && (
          <span className="text-xs text-neutral-400 dark:text-zinc-500">{t('workflows.designGate.approved.linkLoading', { defaultValue: '正在获取设计链接…' })}</span>
        )}
        {linkState === 'error' && (
          <button
            type="button"
            onClick={() => setReloadKey((key) => key + 1)}
            className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-xs font-medium text-neutral-600 transition hover:bg-neutral-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800"
          >
            <RefreshCw className="size-3.5" />
            {t('workflows.designGate.approved.linkUnavailable', { defaultValue: '设计链接获取失败，点击重试。' })}
          </button>
        )}
      </div>
    </section>
  )
}
