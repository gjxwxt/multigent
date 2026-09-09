import { useCallback, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { AlertTriangle } from 'lucide-react'
import { apiPost } from '../../lib/api'
import { DesignSourceChoiceModal } from './DesignSourceChoiceModal'
import { DesignReviewModal } from './DesignReviewModal'

type StartResponse = {
  projectId: string
  proxyUrl: string
  launchUrl: string
  studioUrl: string
  regenerated?: boolean
}

type WaiverPrompt = {
  source: 'existing' | 'generated'
  projectId: string
  errorMsg: string
}

/**
 * Two-stage design gate flow (docs §5/§8): source choice → studio review.
 * Mount it when the active human_review step carries Config designGate=true;
 * it owns the OD start call and turns confirmation into the standard review
 * submit with the flattened approved_design_* outputs.
 */
export function DesignGateFlow({
  project,
  taskID,
  taskTitle,
  busy,
  initialStage = 'choose',
  submitReview,
  onClose,
}: {
  project: string
  taskID: string
  taskTitle?: string
  busy?: boolean
  /** 'review' skips the source chooser — the follow page uses it for the
   * open-canvas shortcut when design/status already reports a session. */
  initialStage?: 'choose' | 'review'
  /** Submits the workflow review; decision/comments merged by the caller. */
  submitReview: (outputs: Record<string, string>, decision: 'approve' | 'request_changes') => Promise<void> | void
  onClose: () => void
}) {
  const [stage, setStage] = useState<'choose' | 'review'>(initialStage)
  const [busyLocal, setBusyLocal] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [waiverPrompt, setWaiverPrompt] = useState<WaiverPrompt | null>(null)
  const [waiverReason, setWaiverReason] = useState('')
  const [session, setSession] = useState<StartResponse | null>(null)
  const { t } = useTranslation()

  const designBase = `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design`

  // Generate is the only path that calls design/start: the backend creates the
  // OD project, kicks off the run, and persists the design reference on the
  // task; the response carries signed proxy/launch URLs for stage two.
  const startGenerate = useCallback(async () => {
    setBusyLocal(true)
    setError(null)
    try {
      const data = await apiPost<StartResponse>(`${designBase}/start`, {}, { suppressToast: true })
      setSession(data)
      setStage('review')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusyLocal(false)
    }
  }, [designBase])

  async function confirmExisting(odProjectId: string, projectName?: string) {
    setBusyLocal(true)
    setError(null)
    try {
      await submitReview(
        {
          approved_design_source: 'existing',
          approved_design_project_id: odProjectId,
          comments: t('designGate.autoComments.existing', {
            defaultValue: `选择已有设计「${projectName || odProjectId}」确认，进入实现编码。`,
            name: projectName || odProjectId,
          }),
        },
        'approve',
      )
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      setError(msg)
      if (msg.includes('design_snapshot_failed')) {
        setWaiverPrompt({
          source: 'existing',
          projectId: odProjectId,
          errorMsg: msg,
        })
      }
      setBusyLocal(false)
    }
  }

  async function confirmGenerated() {
    if (!session?.projectId) return
    setBusyLocal(true)
    setError(null)
    try {
      await submitReview(
        {
          approved_design_source: 'generated',
          approved_design_project_id: session.projectId,
          comments: t('designGate.autoComments.generated', {
            defaultValue: '确认 OD 生成的设计，进入实现编码。',
          }),
        },
        'approve',
      )
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      setError(msg)
      if (msg.includes('design_snapshot_failed')) {
        setWaiverPrompt({
          source: 'generated',
          projectId: session.projectId,
          errorMsg: msg,
        })
      }
      setBusyLocal(false)
    }
  }

  async function submitWaiver() {
    if (!waiverPrompt || !waiverReason.trim()) return
    setBusyLocal(true)
    try {
      await submitReview(
        {
          approved_design_source: waiverPrompt.source,
          approved_design_project_id: waiverPrompt.projectId,
          design_waiver_reason: waiverReason.trim(),
          design_waived: 'true',
          comments: t('designGate.autoComments.waived', {
            defaultValue: `设计快照异常，经人工特批豁免（理由：${waiverReason.trim()}），准入实现。`,
            reason: waiverReason.trim(),
          }),
        },
        'approve',
      )
      setWaiverPrompt(null)
      setWaiverReason('')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusyLocal(false)
    }
  }

  async function rework() {
    setBusyLocal(true)
    setError(null)
    try {
      await submitReview(
        { comments: t('designGate.autoComments.rework', {
          defaultValue: '设计不满足预期，打回需求阶段重新梳理。',
        }) },
        'request_changes',
      )
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusyLocal(false)
    }
  }

  return (
    <>
      {stage === 'choose' ? (
        <DesignSourceChoiceModal
          project={project}
          taskID={taskID}
          taskTitle={taskTitle}
          busy={busy || busyLocal}
          error={error}
          onClose={onClose}
          onConfirmExisting={(id, name) => void confirmExisting(id, name)}
          onStartGenerate={() => void startGenerate()}
        />
      ) : (
        <DesignReviewModal
          project={project}
          taskID={taskID}
          taskTitle={taskTitle}
          proxyUrl={session?.proxyUrl || ''}
          launchUrl={session?.launchUrl || ''}
          onConfirm={() => void confirmGenerated()}
          onRework={() => void rework()}
          onClose={onClose}
        />
      )}

      {waiverPrompt && (
        <div className="fixed inset-0 z-[100] flex items-center justify-center bg-black/60 p-4 backdrop-blur-[2px]">
          <div className="w-full max-w-lg rounded-xl border border-red-200 bg-white p-6 shadow-2xl dark:border-red-900/50 dark:bg-zinc-900 animate-scale-in">
            <div className="flex items-center gap-3 text-red-600 dark:text-red-400">
              <AlertTriangle className="size-6 shrink-0" />
              <h3 className="text-base font-semibold">
                {t('designGate.snapshotFailedTitle', { defaultValue: '设计原型快照抓取失败' })}
              </h3>
            </div>
            <p className="mt-2 text-xs text-neutral-600 dark:text-zinc-400 leading-relaxed">
              {t('designGate.snapshotFailedDesc', {
                defaultValue: '系统在原子化冻结该设计原型时发生异常。为防止后续开发使用空壳或漂移原型，流程已拦截。如需特批放行，请填写审计豁免理由：',
              })}
            </p>
            <div className="mt-3 rounded-md bg-red-50 p-2.5 text-xs text-red-700 dark:bg-red-950/30 dark:text-red-300 font-mono break-all">
              {waiverPrompt.errorMsg}
            </div>
            <div className="mt-4">
              <label className="block text-xs font-semibold text-neutral-700 dark:text-zinc-300">
                {t('designGate.waiverReasonLabel', { defaultValue: '特批豁免理由（必填，将记入审计日志）' })}
              </label>
              <textarea
                value={waiverReason}
                onChange={(e) => setWaiverReason(e.target.value)}
                rows={3}
                placeholder={t('designGate.waiverReasonPlaceholder', { defaultValue: '例如：OD 服务暂时离线，已人工核验本地导出的设计稿一致。' })}
                className="mt-1.5 w-full rounded-lg border border-neutral-300 bg-white px-3 py-2 text-xs text-neutral-900 outline-none focus:border-red-500 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-100 resize-y"
              />
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                disabled={busyLocal}
                onClick={() => {
                  setWaiverPrompt(null)
                  setWaiverReason('')
                }}
                className="rounded-lg border border-neutral-300 px-3.5 py-1.5 text-xs font-medium text-neutral-700 hover:bg-neutral-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
              >
                {t('common.cancel', { defaultValue: '取消' })}
              </button>
              <button
                type="button"
                disabled={busyLocal || !waiverReason.trim()}
                onClick={() => void submitWaiver()}
                className="rounded-lg bg-red-600 px-3.5 py-1.5 text-xs font-medium text-white hover:bg-red-700 disabled:opacity-50"
              >
                {busyLocal ? t('forms.working', { defaultValue: '提交中…' }) : t('designGate.confirmWaiver', { defaultValue: '确认特批豁免并流转' })}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}
