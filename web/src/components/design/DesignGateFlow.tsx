import { useCallback, useState } from 'react'
import { apiPost } from '../../lib/api'
import { DesignSourceChoiceModal } from './DesignSourceChoiceModal'
import { DesignReviewModal } from './DesignReviewModal'

type StartResponse = {
  projectId: string
  proxyUrl: string
  launchUrl: string
  studioUrl: string
  regenerated?: boolean
  conversationId?: string
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
  submitReview,
  onClose,
}: {
  project: string
  taskID: string
  taskTitle?: string
  busy?: boolean
  /** Submits the workflow review; decision/comments merged by the caller. */
  submitReview: (outputs: Record<string, string>, decision: 'approve' | 'request_changes') => Promise<void> | void
  onClose: () => void
}) {
  const [stage, setStage] = useState<'choose' | 'review'>('choose')
  const [busyLocal, setBusyLocal] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [session, setSession] = useState<StartResponse | null>(null)
  const [conversationId, setConversationId] = useState<string | undefined>(undefined)

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
      setConversationId(data.conversationId || undefined)
      setStage('review')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusyLocal(false)
    }
  }, [designBase])

  async function confirmExisting(odProjectId: string) {
    setBusyLocal(true)
    setError(null)
    try {
      await submitReview(
        {
          approved_design_source: 'existing',
          approved_design_project_id: odProjectId,
        },
        'approve',
      )
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
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
        },
        'approve',
      )
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusyLocal(false)
    }
  }

  async function rework() {
    setBusyLocal(true)
    setError(null)
    try {
      await submitReview({}, 'request_changes')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusyLocal(false)
    }
  }

  return stage === 'choose' ? (
    <DesignSourceChoiceModal
      project={project}
      taskID={taskID}
      taskTitle={taskTitle}
      busy={busy || busyLocal}
      error={error}
      onClose={onClose}
      onConfirmExisting={(id) => void confirmExisting(id)}
      onStartGenerate={() => void startGenerate()}
    />
  ) : (
    <DesignReviewModal
      project={project}
      taskID={taskID}
      taskTitle={taskTitle}
      projectId={session?.projectId || ''}
      proxyUrl={session?.proxyUrl || ''}
      launchUrl={session?.launchUrl || ''}
      conversationId={conversationId}
      onConversationId={setConversationId}
      onConfirm={() => void confirmGenerated()}
      onRework={() => void rework()}
      onClose={onClose}
    />
  )
}
