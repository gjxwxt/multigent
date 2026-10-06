import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Loader2, RefreshCw, X } from 'lucide-react'
import { apiPost } from '../../lib/api'
import { cn } from '../../lib/cn'
import { overlayDismissProps } from '../ui/overlay'
import {
  buildDesignDeltaPayload,
  buildDesignReworkMessage,
  seedDecisionState,
  type DesignDeltaAction,
  type DesignDeltaAuditData,
  type DesignDeltaDecision,
  type DesignDeltaDecisionState,
  type DesignDeltaItem,
  type DesignDeltaSide,
} from './designDelta'

// Design delta audit adjudication modal (docs/design-gate-delta-audit-proposal.md
// v2.6): stage two of the two-phase design confirm. The OD modal stays pure;
// pressing its confirm triggers POST design/audit-delta (zero-write first hit)
// and this modal renders the report for human adjudication. Confirming here
// submits the workflow review with the adjudication bundle (same-source HTML +
// sha256) merged into the outputs — the server re-validates the hash before
// freezing.
//
// Phase-1 interaction scope (v2.6 ablation): prototype_extra gets the full
// three-button adjudication; prototype_missing gets rework/waive/skip;
// both_diverge is READ-ONLY plus skip (side-card picking ships in phase 2).

export function DesignDeltaAuditModal({
  project,
  taskID,
  taskTitle,
  prevDecisions,
  onSubmit,
  onRework,
  onClose,
}: {
  project: string
  taskID: string
  taskTitle?: string
  /** Prior-round decisions for differential re-audit context (round ≥ 2). */
  prevDecisions?: DesignDeltaDecision[]
  /** Submits the workflow review with the bundle merged into outputs. */
  onSubmit: (deltaPayload: string, audit: DesignDeltaAuditData) => Promise<void> | void
  /** 续聊式打回：sends the rework asks to the SAME OD conversation; the task
      stays at the design gate. Required — rework mode replaces approve. */
  onRework: (reworkMessage: string, staged: DesignDeltaDecision[]) => Promise<void> | void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [audit, setAudit] = useState<DesignDeltaAuditData | null>(null)
  const [loading, setLoading] = useState(true)
  const [stage, setStage] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [auditFailed, setAuditFailed] = useState(false)
  const [state, setState] = useState<DesignDeltaDecisionState>({})
  const [openNotes, setOpenNotes] = useState<Record<string, boolean>>({})
  const [showPrev, setShowPrev] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  // Re-audit with reminders: panel visibility + the reminder text (prefilled
  // from the previous round so reminders are editable, not silently stacked).
  const [reauditOpen, setReauditOpen] = useState(false)
  const [hints, setHints] = useState('')
  const [reauditing, setReauditing] = useState(false)
  // Prior decisions are read once per mount; the ref keeps the audit effect
  // from re-firing when the parent re-renders with a fresh array identity.
  const prevRef = useRef(prevDecisions)

  // Loading progress steps (work/design-gate-delta-mockup.html audit-loading
  // form): a vertical ○/●/✓ checklist mirroring what the server actually does
  // — fetch HTML, read the OD session's human turns, then the model call.
  // The first two finish in seconds; the model call dominates, so step 3
  // stays "doing" until the response lands. Time-driven handoffs at 2.5s/5s
  // walk stage 0→1→2 — the endpoint is a single POST with no per-step stream.
  const loadSteps = [
    t('designGate.delta.loading.fetch', { defaultValue: '抓取当前原型（与即将冻结的快照同源）' }),
    t('designGate.delta.loading.turns', { defaultValue: '读取 OD 设计会话中你的输入' }),
    t('designGate.delta.loading.audit', { defaultValue: 'Agent 审计：原型 vs 需求差异比对' }),
  ]
  const stepIcon = (i: number) => (stage > i ? '✓' : stage === i ? '●' : '○')

  // First hit: fire the zero-write audit once on mount. Stage rotation starts
  // from the initial state (stage 0) — no synchronous setState in the effect.
  useEffect(() => {
    let alive = true
    const stageTimers = [
      window.setTimeout(() => alive && setStage(1), 2500),
      window.setTimeout(() => alive && setStage(2), 5000),
    ]
    apiPost<DesignDeltaAuditData>(
      `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/audit-delta`,
      // Round ≥2 replays prior adjudications for differential re-audit; the
      // reference is read once per mount (reopening the modal refetches).
      { prevDecisions: prevRef.current || [] },
      { suppressToast: true },
    )
      .then((data) => {
        if (!alive) return
        stageTimers.forEach((t) => window.clearTimeout(t))
        setAudit(data)
        setState(seedDecisionState(data.deltas || []))
        setLoading(false)
      })
      .catch((e) => {
        if (!alive) return
        stageTimers.forEach((t) => window.clearTimeout(t))
        setError(e instanceof Error ? e.message : String(e))
        setAuditFailed(true)
        setLoading(false)
      })
    return () => { alive = false; stageTimers.forEach((t) => window.clearTimeout(t)) }
  }, [project, taskID])

  const grouped = useMemo(() => {
    const deltas = audit?.deltas || []
    return {
      extras: deltas.filter((d) => d.direction === 'prototype_extra'),
      missings: deltas.filter((d) => d.direction === 'prototype_missing'),
      diverges: deltas.filter((d) => d.direction === 'both_diverge'),
    }
  }, [audit])

  // Rework mode: at least one missing item is flagged 打回设计补齐 — the
  // footer's primary action becomes the rework send (stay at the gate, OD
  // amends the prototype) instead of approve-and-flow.
  const hasRework = useMemo(
    () =>
      (audit?.deltas || []).some(
        (d) => d.direction === 'prototype_missing' && state[d.delta_id]?.action === 'note',
      ),
    [audit, state],
  )
  const reworkCount = useMemo(
    () =>
      (audit?.deltas || []).filter(
        (d) => d.direction === 'prototype_missing' && state[d.delta_id]?.action === 'note',
      ).length,
    [audit, state],
  )

  const allHandled = useMemo(() => {
    const deltas = audit?.deltas || []
    // A diverge adjudicated 其他 without a fresh description is NOT handled —
    // implementation would have no authoritative behavior to build against.
    return deltas.every((d) => {
      const s = state[d.delta_id]
      if (!s?.action) return false
      if (d.direction === 'both_diverge' && s.side === 'other' && !s.sideNote?.trim()) return false
      return true
    })
  }, [audit, state])

  const otherWithoutNoteCount = useMemo(
    () =>
      (audit?.deltas || []).filter(
        (d) => d.direction === 'both_diverge' && state[d.delta_id]?.side === 'other' && !state[d.delta_id]?.sideNote?.trim(),
      ).length,
    [audit, state],
  )

  const skipCount = useMemo(
    () => Object.values(state).filter((s) => s.action === 'skip').length,
    [state],
  )

  function pick(id: string, action: DesignDeltaAction) {
    setState((prev) => ({ ...prev, [id]: { ...prev[id], action, side: undefined } }))
  }

  function pickSide(id: string, side: 'requirement' | 'prototype' | 'other') {
    setState((prev) => {
      const cur = prev[id] || { action: 'skip' as DesignDeltaAction, note: '' }
      return { ...prev, [id]: { ...cur, side } }
    })
  }

  function setSideNote(id: string, sideNote: string) {
    setState((prev) => ({ ...prev, [id]: { ...prev[id], sideNote } }))
  }

  function setNote(id: string, note: string) {
    setState((prev) => ({ ...prev, [id]: { ...prev[id], note } }))
  }

  function toggleNote(id: string) {
    setOpenNotes((prev) => ({ ...prev, [id]: !prev[id] }))
  }

  // Re-audit: replay every verdict made so far (staged + current state) as
  // prevDecisions so differential re-audit carries them, plus the human's
  // reminders steering what to re-check. Empty reminders are allowed — a
  // plain re-roll is legitimate (audit variance is real). The response
  // replaces the report and reseeds; rounds/hints ride the audit data.
  async function reaudit() {
    if (!audit || reauditing || submitting) return
    setReauditing(true)
    setError(null)
    // Same step-by-step loading screen as the first hit: the re-audit is the
    // same server pipeline (fetch → read turns → model), so it earns the
    // same progress view instead of a bare spinner in the footer.
    setStage(0)
    setLoading(true)
    const stageTimers = [
      window.setTimeout(() => setStage(1), 2500),
      window.setTimeout(() => setStage(2), 5000),
    ]
    try {
      const replayed: DesignDeltaDecision[] = [
        ...(prevRef.current || []),
        ...Object.entries(state).map(([delta_id, s]) => ({
          delta_id,
          action: s.action,
          note: s.note || '',
          side: s.side || '',
          sideNote: s.sideNote || '',
        })),
      ]
      const hintList = hints.split('\n').map((h) => h.trim()).filter(Boolean)
      const data = await apiPost<DesignDeltaAuditData>(
        `/api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/audit-delta`,
        { prevDecisions: replayed, prevHtml: audit.html, hints: hintList },
        { suppressToast: true },
      )
      setAudit(data)
      setState(seedDecisionState(data.deltas || []))
      setHints(hintList.join('\n'))
      setReauditOpen(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setAuditFailed(true)
    } finally {
      stageTimers.forEach((t) => window.clearTimeout(t))
      setReauditing(false)
      setLoading(false)
    }
  }

  async function submit() {
    if (!audit || !allHandled || submitting) return
    setSubmitting(true)
    try {
      await onSubmit(buildDesignDeltaPayload(audit, state), audit)
    } finally {
      setSubmitting(false)
    }
  }

  // Rework path: everything EXCEPT the note-flagged items is a verdict that
  // must survive to the next round — stage it, then send one completion
  // message (the note items + their notes) back to the same OD conversation.
  async function submitRework() {
    if (!audit || !allHandled || submitting) return
    setSubmitting(true)
    try {
      const reworkIds = new Set(
        audit.deltas
          .filter((d) => d.direction === 'prototype_missing' && state[d.delta_id]?.action === 'note')
          .map((d) => d.delta_id),
      )
      const staged: DesignDeltaDecision[] = audit.deltas
        .filter((d) => !reworkIds.has(d.delta_id))
        .map((d) => ({
          delta_id: d.delta_id,
          action: state[d.delta_id]?.action || 'skip',
          note: state[d.delta_id]?.note || '',
          side: state[d.delta_id]?.side || '',
          sideNote: state[d.delta_id]?.sideNote || '',
        }))
      await onRework(buildDesignReworkMessage(audit.deltas, state), staged)
    } finally {
      setSubmitting(false)
    }
  }

  const sideBlock = (label: string, side?: DesignDeltaSide) => (
    <div className="min-w-0 flex-1 rounded-lg border border-neutral-200 bg-neutral-50 p-2.5 dark:border-zinc-700 dark:bg-zinc-800/60">
      <div className="flex items-center gap-1.5">
        <span className={cn('rounded px-1.5 py-0.5 text-[10px] font-medium',
          side?.present
            ? 'bg-neutral-200 text-neutral-700 dark:bg-zinc-700 dark:text-zinc-200'
            : 'bg-neutral-100 text-neutral-400 line-through dark:bg-zinc-800 dark:text-zinc-500')}>
          {side?.present
            ? t('designGate.delta.side.present', { defaultValue: '有' })
            : t('designGate.delta.side.absent', { defaultValue: '无' })}
        </span>
        <span className="text-[11px] font-semibold text-neutral-500 dark:text-zinc-400">{label}</span>
      </div>
      <p className="mt-1.5 text-xs leading-relaxed text-neutral-700 dark:text-zinc-300">
        {side?.reading || t('designGate.delta.side.none', { defaultValue: '—' })}
      </p>
      {side?.ref && (
        <p className="mt-1 truncate font-mono text-[10px] text-neutral-400 dark:text-zinc-500" title={side.ref}>
          {side.ref}
        </p>
      )}
    </div>
  )

  const choiceBtn = (id: string, action: DesignDeltaAction, label: string, tone: 'primary' | 'danger' | 'neutral') => {
    const active = state[id]?.action === action
    return (
      <button
        key={action}
        type="button"
        onClick={() => pick(id, action)}
        className={cn('rounded-lg border px-2.5 py-1.5 text-xs font-medium transition-colors',
          active
            ? tone === 'primary'
              ? 'border-sky-500 text-sky-700 dark:border-sky-400 dark:text-sky-300'
              : tone === 'danger'
                ? 'border-red-500 text-red-700 dark:border-red-400 dark:text-red-300'
                : 'border-neutral-500 text-neutral-700 dark:border-zinc-400 dark:text-zinc-200'
            : 'border-neutral-200 bg-white text-neutral-500 hover:border-neutral-400 hover:text-neutral-700 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400 dark:hover:border-zinc-500 dark:hover:text-zinc-200')}
      >
        {active ? '● ' : ''}{label}
      </button>
    )
  }

  const deltaCard = (d: DesignDeltaItem) => {
    const dirLabel = d.direction === 'prototype_extra'
      ? t('designGate.delta.dir.extra', { defaultValue: '原型新增' })
      : d.direction === 'prototype_missing'
        ? t('designGate.delta.dir.missing', { defaultValue: '原型缺失' })
        : t('designGate.delta.dir.diverge', { defaultValue: '双侧偏离' })
    const sel = state[d.delta_id]
    return (
      <div key={d.delta_id} className="relative rounded-xl border border-neutral-200 p-3.5 dark:border-zinc-700">
        <div className="flex items-start justify-between gap-2">
          <p className="min-w-0 flex-1 text-sm font-medium text-neutral-800 dark:text-zinc-100">
            {d.requirement_side?.reading || d.prototype_side?.reading || d.delta_id}
          </p>
          <span className="shrink-0 rounded-full border border-neutral-200 px-2 py-0.5 text-[10px] font-medium text-neutral-500 dark:border-zinc-600 dark:text-zinc-400">
            {dirLabel}
          </span>
        </div>
        <div className="mt-2.5 flex flex-col gap-2 sm:flex-row">
          {sideBlock(t('designGate.delta.side.req', { defaultValue: '需求侧' }), d.requirement_side)}
          {sideBlock(t('designGate.delta.side.proto', { defaultValue: '原型侧' }), d.prototype_side)}
        </div>
        {d.direction === 'both_diverge' && d.diverge_fields && (
          <p className="mt-2 text-xs text-neutral-600 dark:text-zinc-300">
            <span className="font-semibold">{t('designGate.delta.divergeFields', { defaultValue: '不一致点：' })}</span>
            {d.diverge_fields}
          </p>
        )}
        {/* Evidence (collapsed): source classification is honest about the
            degraded benchmark — no session history means no claimed source. */}
        {d.source_class === 'uncertain' && (
          <p className="mt-1.5 text-[10px] text-neutral-400 dark:text-zinc-500">
            {t('designGate.delta.sourceUncertain', {
              defaultValue: '来源：无法判定（会话历史暂不可读）——请自行判断该元素是否是你要求的。',
            })}
          </p>
        )}
        {/* Actions pinned bottom-right; note disclosure progressive. */}
        <div className="mt-3 flex flex-wrap items-center justify-end gap-1.5">
          {!openNotes[d.delta_id] && (
            <button
              type="button"
              onClick={() => toggleNote(d.delta_id)}
              className="mr-auto text-[11px] text-neutral-400 hover:text-sky-600 dark:text-zinc-500 dark:hover:text-sky-400"
            >
              ＋ {t('designGate.delta.addNote', { defaultValue: '添加备注（可选）' })}
            </button>
          )}
          {openNotes[d.delta_id] && (
            <textarea
              value={sel?.note || ''}
              onChange={(e) => setNote(d.delta_id, e.target.value)}
              rows={2}
              placeholder={d.suggested_spec
                ? t('designGate.delta.notePlaceholderSpec', { defaultValue: '备注（可选；Agent 建议：' }) + d.suggested_spec + '）'
                : t('designGate.delta.notePlaceholder', { defaultValue: '备注（可选）：补充你的意图说明' })}
              className="w-full rounded-lg border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-800 outline-none focus:border-sky-400 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-100"
            />
          )}
          {d.direction === 'prototype_extra' && (
            <>
              {choiceBtn(d.delta_id, 'adopt', t('designGate.delta.action.adopt', { defaultValue: '纳入需求' }), 'primary')}
              {choiceBtn(d.delta_id, 'drop', t('designGate.delta.action.drop', { defaultValue: '不纳入需求' }), 'danger')}
              {choiceBtn(d.delta_id, 'skip', t('designGate.delta.action.skip', { defaultValue: '跳过（暂不裁决）' }), 'neutral')}
            </>
          )}
          {d.direction === 'prototype_missing' && (
            <>
              {choiceBtn(d.delta_id, 'note', t('designGate.delta.action.rework', { defaultValue: '打回设计补齐' }), 'primary')}
              {choiceBtn(d.delta_id, 'drop', t('designGate.delta.action.waive', { defaultValue: '本次豁免（本轮不做）' }), 'neutral')}
              {choiceBtn(d.delta_id, 'skip', t('designGate.delta.action.skip', { defaultValue: '跳过（暂不裁决）' }), 'neutral')}
            </>
          )}
          {d.direction === 'both_diverge' && (
            choiceBtn(d.delta_id, 'skip', t('designGate.delta.action.skip', { defaultValue: '跳过（暂不裁决）' }), 'neutral')
          )}
          {/* both_diverge side verdicts live in the same pinned action row as
              the other verdict buttons — one visual language per card. 其他
              keeps the amber tone (it is the neither-side escape). */}
          {d.direction === 'both_diverge' &&
            (['requirement', 'prototype', 'other'] as const).map((sideOpt) => {
              const active = (sel?.side || 'prototype') === sideOpt
              const label = sideOpt === 'requirement'
                ? t('designGate.delta.side.req', { defaultValue: '需求侧' })
                : sideOpt === 'prototype'
                  ? t('designGate.delta.side.proto', { defaultValue: '原型侧' })
                  : t('designGate.delta.side.other', { defaultValue: '其他' })
              return (
                <button
                  key={sideOpt}
                  type="button"
                  onClick={() => pickSide(d.delta_id, sideOpt)}
                  className={cn('rounded-lg border px-2.5 py-1.5 text-xs font-medium transition-colors',
                    active
                      ? sideOpt === 'other'
                        ? 'border-amber-500 text-amber-700 dark:border-amber-400 dark:text-amber-300'
                        : 'border-sky-500 text-sky-700 dark:border-sky-400 dark:text-sky-300'
                      : 'border-neutral-200 bg-white text-neutral-500 hover:border-neutral-400 hover:text-neutral-700 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-400')}
                >
                  {active ? '● ' : ''}{label}
                </button>
              )
            })}
          {/* 'other' = neither side wins: the fresh description below is the
              authoritative behavior. Empty input blocks confirm — a diverge
              adjudicated 其他 with no description leaves implementation
              nothing to build against. */}
          {d.direction === 'both_diverge' && sel?.side === 'other' && (
            <div className="w-full">
              <label className="text-[11px] font-medium text-neutral-600 dark:text-zinc-300">
                {t('designGate.delta.sideOtherLabel', { defaultValue: '应该以什么为准（必填）' })}
              </label>
              <textarea
                value={sel?.sideNote || ''}
                onChange={(e) => setSideNote(d.delta_id, e.target.value)}
                rows={2}
                placeholder={t('designGate.delta.sideOtherPlaceholder', {
                  defaultValue: '描述两侧都不采用的最终行为，实现将以此为准（例如：心跳周期改为 60s 上报一次，断线时连续 3 次失败才告警）',
                })}
                className="mt-1 w-full rounded-lg border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-800 outline-none focus:border-amber-400 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-100"
              />
            </div>
          )}
          {d.direction === 'both_diverge' && (
            <p className="w-full text-right text-[10px] text-neutral-400 dark:text-zinc-500">
              {sel?.side === 'other'
                ? t('designGate.delta.sidePick.other', { defaultValue: '两侧描述均不作依据；实现以下方描述为准（描述为空无法确认流转）' })
                : sel?.side === 'requirement'
                  ? t('designGate.delta.sidePick.req', { defaultValue: '实现以需求描述为准；原型侧表现不作依据（可备注细化）' })
                  : t('designGate.delta.sidePick.proto', { defaultValue: '实现以原型表现为准；需求文本不作依据（可备注细化）' })}
            </p>
          )}
          {sel?.action === 'adopt' && (
            <p className="w-full text-right text-[10px] text-neutral-400 dark:text-zinc-500">
              {sel.note
                ? t('designGate.delta.adoptWithNote', { defaultValue: '纳入：将按你的备注写入需求口径' })
                : t('designGate.delta.adoptNoNote', { defaultValue: '纳入：将按元素可见行为规格化（可添加备注细化）' })}
            </p>
          )}
          {sel?.action === 'skip' && (
            <p className="w-full text-right text-[10px] text-neutral-400 dark:text-zinc-500">
              {t('designGate.delta.skipConsequence', {
                defaultValue: '未裁决：不生成用例、不写入实现规格；初审若发现被实现将回流人工裁决。',
              })}
            </p>
          )}
          {sel?.action === 'drop' && d.direction === 'prototype_missing' && (
            <p className="w-full text-right text-[10px] text-neutral-400 dark:text-zinc-500">
              {t('designGate.delta.waiveConsequence', {
                defaultValue: '豁免留痕：本轮不实现该需求点；实现步若擅自实现将判缺陷。',
              })}
            </p>
          )}
          {sel?.action === 'note' && d.direction === 'prototype_missing' && (
            <p className="w-full text-right text-[10px] text-amber-600 dark:text-amber-400">
              {t('designGate.delta.reworkConsequence', {
                defaultValue: '将向同一 OD 会话发送补齐消息（保留上下文续改）；重新审计时已裁决项自动沿用。',
              })}
            </p>
          )}
        </div>
      </div>
    )
  }

  const groupHeader = (label: string, n: number) => n > 0 && (
    <div className="mt-4 flex items-center gap-2 first:mt-0">
      <span className="text-xs font-semibold text-neutral-600 dark:text-zinc-300">{label}</span>
      <span className="rounded-full bg-neutral-100 px-1.5 py-0.5 text-[10px] text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
        {t('designGate.delta.itemCount', { count: n, defaultValue: '{{n}} 项', n })}
      </span>
    </div>
  )

  return (
    <div className="fixed inset-0 z-[95] flex items-center justify-center bg-black/50 p-4 backdrop-blur-[1px]" role="presentation" {...overlayDismissProps(onClose)}>
      <div
        className="flex h-[min(85vh,860px)] w-[min(880px,94vw)] animate-scale-in flex-col overflow-hidden rounded-xl border border-neutral-200 bg-white shadow-2xl dark:border-zinc-700 dark:bg-zinc-900"
        role="dialog"
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header: plain title, task name, close */}
        <div className="flex shrink-0 items-center justify-between gap-3 border-b border-neutral-100 px-5 py-3 dark:border-zinc-800">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
              {t('designGate.delta.title', { defaultValue: '差异裁决' })}
              {(audit?.auditRounds || 1) > 1 && (
                <span className="ml-2 rounded-full bg-sky-50 px-1.5 py-0.5 text-[10px] font-medium text-sky-700 dark:bg-sky-950/40 dark:text-sky-300">
                  {t('designGate.delta.roundN', { n: audit!.auditRounds!, defaultValue: '第 {{n}} 轮审计' })}
                </span>
              )}
              {taskTitle ? <span className="ml-2 truncate font-normal text-xs text-neutral-500 dark:text-zinc-400">{taskTitle}</span> : null}
            </h2>
            {audit && (
              <p className="mt-0.5 text-[11px] text-neutral-400 dark:text-zinc-500">
                {t('designGate.delta.fetchedAt', {
                  defaultValue: '本次裁决针对 {{time}} 抓取的原型；若此后又修改过原型，请关闭后重新确认以重跑审计。',
                  time: new Date(audit.fetchedAt).toLocaleTimeString(),
                })}
              </p>
            )}
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md p-1 text-neutral-400 transition-colors hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
            aria-label="close"
          >
            <X className="size-4" />
          </button>
        </div>

        {/* Body */}
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          {loading && (
            <div className="flex h-full flex-col items-center justify-center gap-5">
              <Loader2 className="size-8 animate-spin text-sky-500" />
              <p className="text-sm font-semibold text-neutral-800 dark:text-zinc-100">
                {t('designGate.delta.loading.title', { defaultValue: '正在校验原型与需求的一致性' })}
              </p>
              <div className="flex min-w-[260px] flex-col gap-2.5">
                {loadSteps.map((label, i) => (
                  <div
                    key={i}
                    className={cn(
                      'flex items-center gap-2.5 text-[13px] transition-colors',
                      stage > i
                        ? 'text-emerald-600 dark:text-emerald-400'
                        : stage === i
                          ? 'font-semibold text-neutral-800 dark:text-zinc-100'
                          : 'text-neutral-400 dark:text-zinc-500',
                    )}
                  >
                    <span className="w-[18px] text-center">{stepIcon(i)}</span>
                    {label}
                  </div>
                ))}
              </div>
              <p className="text-[11px] text-neutral-400 dark:text-zinc-500">
                {t('designGate.delta.loading.eta', { defaultValue: '此步通常需要数秒到数十秒' })}
              </p>
            </div>
          )}
          {!loading && auditFailed && (
            <div className="mx-auto mt-10 max-w-md rounded-xl border border-neutral-200 bg-neutral-50 p-5 text-center dark:border-zinc-700 dark:bg-zinc-800/60">
              <p className="text-sm font-semibold text-neutral-800 dark:text-zinc-100">
                {t('designGate.delta.failedTitle', { defaultValue: '差异审计未完成' })}
              </p>
              <p className="mt-2 break-all text-xs text-red-600 dark:text-red-400">{error}</p>
              <div className="mt-4 flex justify-center gap-2">
                <button
                  type="button"
                  onClick={() => window.location.reload()}
                  className="rounded-lg border border-neutral-300 px-3 py-1.5 text-xs font-medium text-neutral-600 hover:bg-neutral-50 dark:border-zinc-600 dark:text-zinc-300 dark:hover:bg-zinc-800"
                >
                  <RefreshCw className="mr-1 inline size-3" />
                  {t('designGate.delta.retry', { defaultValue: '重试' })}
                </button>
              </div>
            </div>
          )}
          {!loading && audit && (
            <>
              {/* Prior-round adjudications: collapsed read-only context. */}
              {(audit.prevDecisions?.length || 0) > 0 && (
                <div className="mb-3 rounded-lg border border-neutral-200 dark:border-zinc-700">
                  <button
                    type="button"
                    onClick={() => setShowPrev((v) => !v)}
                    className="flex w-full items-center justify-between px-3 py-2 text-xs font-medium text-neutral-600 dark:text-zinc-300"
                  >
                    <span>
                      {t('designGate.delta.prevRound', {
                        defaultValue: '上轮裁决（{{n}} 项）——已自动沿用，仅本轮新增/失效项需要处置',
                        n: audit.prevDecisions!.length,
                      })}
                    </span>
                    <span>{showPrev ? '▾' : '▸'}</span>
                  </button>
                  {showPrev && (
                    <div className="border-t border-neutral-100 px-3 py-2 dark:border-zinc-800">
                      {audit.prevDecisions!.map((p, i) => (
                        <p key={i} className="text-[11px] text-neutral-500 dark:text-zinc-400">
                          · {p.delta_id}: {p.action}{p.note ? ` — ${p.note}` : ''}
                        </p>
                      ))}
                    </div>
                  )}
                </div>
              )}

              {/* Zero-difference light path. */}
              {audit.deltas.length === 0 ? (
                <div className="mx-auto mt-10 max-w-md rounded-xl border border-emerald-200 bg-emerald-50/60 p-5 text-center dark:border-emerald-800 dark:bg-emerald-950/30">
                  <p className="text-sm font-semibold text-emerald-700 dark:text-emerald-300">
                    {t('designGate.delta.cleanTitle', { defaultValue: 'Agent 未发现差异' })}
                  </p>
                  <p className="mt-2 text-xs leading-relaxed text-neutral-500 dark:text-zinc-400">
                    {t('designGate.delta.cleanDesc', {
                      defaultValue: '审计未发现原型与需求之间的差异。这不等于保证无差异（Agent 审计会漏报）；确认后仍可在初审与测试环节发现问题。',
                    })}
                  </p>
                </div>
              ) : (
                <>
                  {skipCount >= 5 && (
                    <p className="mb-3 rounded-lg bg-amber-50 px-3 py-2 text-[11px] text-amber-700 dark:bg-amber-950/40 dark:text-amber-300">
                      {t('designGate.delta.skipThreshold', {
                        defaultValue: '{{n}} 项暂不裁决：它们将随「未裁决」状态下传，初审会对照实现情况回流人工。',
                        n: skipCount,
                      })}
                    </p>
                  )}
                  {groupHeader(t('designGate.delta.group.extra', { defaultValue: '原型新增（需求里没有的东西）' }), grouped.extras.length)}
                  {grouped.extras.map(deltaCard)}
                  {groupHeader(t('designGate.delta.group.missing', { defaultValue: '原型缺失（需求要求但原型未体现）' }), grouped.missings.length)}
                  {grouped.missings.map(deltaCard)}
                  {groupHeader(t('designGate.delta.group.diverge', { defaultValue: '双侧偏离（都有但描述不一致）' }), grouped.diverges.length)}
                  {grouped.diverges.map(deltaCard)}
                </>
              )}
            </>
          )}
        </div>

        {/* Footer: escape hatch + re-audit left; rework-or-confirm right.
            Rework mode (any missing item flagged 打回设计补齐) swaps the
            primary action: the verdict goes back to the SAME OD conversation
            and the task stays at the gate — approve is not offered alongside
            it, because sending rework asks and approving the prototype are
            contradictory verdicts on the same round. 重新审计 is the third
            feedback channel: reminders steer the NEXT audit (what might have
            been missed); it never touches OD or the task state. */}
        {reauditOpen && !loading && (
          <div className="shrink-0 border-t border-neutral-100 bg-neutral-50/60 px-5 py-3 dark:border-zinc-800 dark:bg-zinc-800/40">
            <label className="text-xs font-semibold text-neutral-700 dark:text-zinc-200">
              {t('designGate.delta.reauditLabel', { defaultValue: '提醒下一轮审计（每行一条，可留空直接重审）' })}
            </label>
            <textarea
              value={hints}
              onChange={(e) => setHints(e.target.value)}
              rows={3}
              disabled={reauditing}
              placeholder={t('designGate.delta.reauditPlaceholder', {
                defaultValue: '例：请专门核对登录后的管理控制台页面；心跳上报的阈值参数我怀疑没对照到',
              })}
              className="mt-1.5 w-full rounded-lg border border-neutral-200 bg-white px-2.5 py-1.5 text-xs text-neutral-800 outline-none focus:border-sky-400 disabled:opacity-60 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-100"
            />
            <div className="mt-2 flex items-center justify-end gap-2">
              <button
                type="button"
                onClick={() => setReauditOpen(false)}
                disabled={reauditing}
                className="rounded-lg border border-neutral-300 bg-white px-3 py-1.5 text-xs font-medium text-neutral-600 hover:bg-neutral-50 disabled:opacity-60 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
              >
                {t('designGate.delta.reauditCancel', { defaultValue: '取消' })}
              </button>
              <button
                type="button"
                onClick={() => void reaudit()}
                disabled={reauditing}
                className="inline-flex items-center gap-1.5 rounded-lg bg-sky-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-sky-700 disabled:opacity-60"
              >
                {reauditing ? <Loader2 className="size-3.5 animate-spin" /> : <RefreshCw className="size-3.5" />}
                {reauditing
                  ? t('designGate.delta.reauditing', { defaultValue: '重新审计中…' })
                  : t('designGate.delta.reauditGo', { defaultValue: '重新审计' })}
              </button>
            </div>
          </div>
        )}
        <div className="flex shrink-0 items-center justify-between gap-3 border-t border-neutral-100 px-5 py-3 dark:border-zinc-800">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onClose}
              className="rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm font-medium text-neutral-600 hover:bg-neutral-50 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
            >
              {t('designGate.delta.cancel', { defaultValue: '返回设计' })}
            </button>
            {!loading && (
              <button
                type="button"
                onClick={() => setReauditOpen((v) => !v)}
                disabled={reauditing || submitting}
                title={t('designGate.delta.reauditTitle', {
                  defaultValue: '感觉有方面没审到？带上提醒重新审计（已裁决项自动沿用，不影响 OD 原型）',
                })}
                className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-300 bg-white px-2.5 py-2 text-xs font-medium text-neutral-500 hover:border-sky-400 hover:text-sky-700 disabled:opacity-60 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-400 dark:hover:border-sky-500 dark:hover:text-sky-300"
              >
                <RefreshCw className="size-3.5" />
                {t('designGate.delta.reauditBtn', { defaultValue: '重新审计' })}
              </button>
            )}
          </div>
          <div className="flex items-center gap-3">
            {!loading && audit && (
            <span className="hidden text-xs text-neutral-400 sm:inline dark:text-zinc-500">
              {otherWithoutNoteCount > 0
                ? t('designGate.delta.sideOtherBlocked', {
                    n: otherWithoutNoteCount,
                    defaultValue: '{{n}} 项选了「其他」但描述为空——请填写两侧都不采用时应以什么为准',
                  })
                : hasRework
                  ? t('designGate.delta.reworkProgress', {
                      n: reworkCount,
                      defaultValue: '{{n}} 项将打回设计补齐；其余裁决已暂存，重新审计时自动沿用',
                    })
                  : t('designGate.delta.progress', { done: audit.deltas.length, total: audit.deltas.length, defaultValue: '共 {{total}} 项' })}
            </span>
            )}
            <button
              type="button"
              disabled={loading || !audit || !allHandled || submitting}
              onClick={() => void (hasRework ? submitRework() : submit())}
              title={otherWithoutNoteCount > 0
                ? t('designGate.delta.sideOtherBlocked', {
                    n: otherWithoutNoteCount,
                    defaultValue: '{{n}} 项选了「其他」但描述为空——请填写两侧都不采用时应以什么为准',
                  })
                : hasRework
                  ? t('designGate.delta.reworkReady', { defaultValue: '将打回项发送到同一设计会话补齐；本轮不流转' })
                  : allHandled
                    ? t('designGate.delta.confirmReady', { defaultValue: '裁决完成，冻结原型快照并随裁决记录流转' })
                    : t('designGate.delta.confirmBlocked', { defaultValue: '请先处置全部差异项（跳过也算已处置）' })}
              className={cn('rounded-lg px-4 py-2 text-sm font-semibold text-white disabled:opacity-50',
                hasRework ? 'bg-amber-600 hover:bg-amber-700' : 'bg-sky-600 hover:bg-sky-700')}
            >
              {submitting
                ? t('designGate.delta.submitting', { defaultValue: '提交中…' })
                : hasRework
                  ? t('designGate.delta.reworkBtn', { n: reworkCount, defaultValue: '打回设计重改（{{n}} 项）' })
                  : t('designGate.delta.confirm', { defaultValue: '确认流转' })}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
