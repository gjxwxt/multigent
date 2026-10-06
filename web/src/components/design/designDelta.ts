// Design delta audit shared types and pure helpers (docs/
// design-gate-delta-audit-proposal.md v2.6). Kept out of the component file:
// the payload builder and action seeding are unit-testable logic, and the
// component file stays component-only for react-refresh.

export type DesignDeltaSide = {
  present?: boolean
  reading?: string
  ref?: string
}

export type DesignDeltaItem = {
  delta_id: string
  direction: 'prototype_extra' | 'prototype_missing' | 'both_diverge' | string
  requirement_side?: DesignDeltaSide
  prototype_side?: DesignDeltaSide
  diverge_fields?: string
  intent_signal?: string
  source_class?: string
  source_evidence?: string
  suggested_action?: string
  suggested_spec?: string
  confidence?: string
  element_fingerprint?: string
}

export type DesignDeltaDecision = {
  delta_id: string
  action: 'adopt' | 'drop' | 'skip' | 'note' | string
  note?: string
  // pick-side verdict for both_diverge items: requirement | prototype |
  // other. 'other' means NEITHER side is taken as-is — sideNote carries the
  // fresh authoritative description. Carried so ATD/implementation know which
  // side is authoritative without parsing note text.
  side?: 'requirement' | 'prototype' | 'other' | string
  // Fresh description for side === 'other': what should actually be built.
  sideNote?: string
}

export type DesignDeltaAuditData = {
  html: string
  htmlSha256: string
  fetchedAt: string
  deltas: DesignDeltaItem[]
  skippedHint?: boolean
  prevDecisions?: DesignDeltaDecision[]
  auditRounds?: number
  // Human re-audit reminders that produced THIS report (round ≥ 2). Echoed
  // back so the reminder panel can prefill the next round's reminders.
  hints?: string[]
}

export type DesignDeltaAction = 'adopt' | 'drop' | 'skip' | 'note'

export type DesignDeltaDecisionState = {
  [deltaId: string]: { action: DesignDeltaAction; note: string; side?: 'requirement' | 'prototype' | 'other'; sideNote?: string }
}

// defaultActionFor seeds each item with the model's suggested action — the
// human can change it, but pre-seeding makes "skip everything by accident"
// impossible while keeping one-click flow.
export function defaultActionFor(d: DesignDeltaItem): DesignDeltaAction {
  switch (d.suggested_action) {
    case 'adopt_into_requirement': return 'adopt'
    case 'drop_from_prototype': return 'drop'
    case 'rework_design': return 'note'
    case 'waive_this_round': return 'drop'
    default: return 'skip'
  }
}

// defaultSideFor maps the audit's both_diverge suggestion onto the three-way
// side picker. Only specify_fresh pre-picks 其他 — its suggested_spec drafts
// the fresh behavior the human edits; picking a side is otherwise always a
// human verdict, so anything unrecognized falls back to 原型侧.
export function defaultSideFor(d: DesignDeltaItem): 'requirement' | 'prototype' | 'other' {
  if (d.direction !== 'both_diverge') return 'prototype'
  switch (d.suggested_action) {
    case 'pick_requirement_side': return 'requirement'
    case 'specify_fresh': return 'other'
    default: return 'prototype'
  }
}

// Side verdicts are mandatory for both_diverge (a diverge without an
// authoritative side leaves the conflict unresolved for implementation), so
// seedDecisionState pre-picks a side — the audit's suggestion when it has
// one, else the prototype side; the human changes it or picks 其他 and
// describes fresh behavior.
export function seedDecisionState(deltas: DesignDeltaItem[]): DesignDeltaDecisionState {
  const seed: DesignDeltaDecisionState = {}
  for (const d of deltas) {
    const base = { action: defaultActionFor(d), note: '' }
    if (d.direction === 'both_diverge') {
      seed[d.delta_id] = { ...base, side: defaultSideFor(d), sideNote: '' }
    } else {
      seed[d.delta_id] = base
    }
  }
  return seed
}

/**
 * Builds the confirm-time payload merged into the workflow review outputs:
 * design_delta_decisions = { html, htmlSha256, fetchedAt, deltas, decisions,
 * hints, auditRounds }. The server re-validates the hash (same-source freeze)
 * before persisting. hints/auditRounds carry the audit PROVENANCE — a report
 * produced under human re-audit reminders stays auditable as such.
 */
export function buildDesignDeltaPayload(
  audit: Pick<DesignDeltaAuditData, 'html' | 'htmlSha256' | 'fetchedAt' | 'deltas' | 'hints' | 'auditRounds'>,
  state: DesignDeltaDecisionState,
): string {
  const decisions: DesignDeltaDecision[] = audit.deltas
    .map((d) => ({
      delta_id: d.delta_id,
      action: state[d.delta_id]?.action || 'skip',
      note: state[d.delta_id]?.note || '',
      side: state[d.delta_id]?.side || '',
      sideNote: state[d.delta_id]?.sideNote || '',
    }))
  return JSON.stringify({
    html: audit.html,
    htmlSha256: audit.htmlSha256,
    fetchedAt: audit.fetchedAt,
    deltas: audit.deltas,
    decisions,
    hints: audit.hints || [],
    auditRounds: audit.auditRounds || 1,
  })
}

/**
 * Builds the rework message body sent back to the SAME OD conversation when
 * the human pressed 打回设计重改: only the note-flagged missing items, each
 * with its requirement reading and the human's note, phrased as concrete
 * completion asks. Kept imperative and short — the OD agent continues the
 * existing prototype rather than regenerating.
 */
export function buildDesignReworkMessage(deltas: DesignDeltaItem[], state: DesignDeltaDecisionState): string {
  const items = deltas
    .filter((d) => d.direction === 'prototype_missing' && state[d.delta_id]?.action === 'note')
    .map((d, i) => {
      const req = d.requirement_side?.reading?.trim() || ''
      const note = state[d.delta_id]?.note?.trim() || ''
      const n = i + 1
      let line = `${n}. ${req || d.delta_id}`
      if (note) line += `（补充要求：${note}）`
      return line
    })
  // both_diverge items adjudicated 其他 (neither side) also go back: the
  // fresh description in sideNote is exactly a design-change ask.
  const divergeOthers = deltas
    .filter((d) => d.direction === 'both_diverge' && state[d.delta_id]?.side === 'other')
    .map((d, i) => {
      const req = d.requirement_side?.reading?.trim() || ''
      const proto = d.prototype_side?.reading?.trim() || ''
      const fresh = state[d.delta_id]?.sideNote?.trim() || ''
      const n = items.length + i + 1
      let line = `${n}. 双侧偏离「${req || proto || d.delta_id}」两侧描述均不采用，应改为：${fresh || '（待补充说明）'}`
      if (proto) line += `（当前原型表现：${proto}）`
      return line
    })
  const body = [...items, ...divergeOthers]
  if (body.length === 0) return ''
  return [
    items.length > 0
      ? '差异审计发现以下需求点在原型中缺失，请在现有原型基础上补齐（保留既有页面与风格，不要整体重做）：'
      : '差异审计中以下双侧偏离项两侧描述均不采用，请在现有原型基础上调整（保留既有页面与风格，不要整体重做）：',
    ...body,
    '补齐完成后请更新原型文件。',
  ].join('\n')
}

/**
 * Adjudications staged across a rework round: the human's verdicts on items
 * that were NOT sent back survive in sessionStorage and seed the next audit
 * as prevDecisions (differential re-audit carries them automatically).
 * sessionStorage (not localStorage): staging is per-browser-session intent,
 * and a stale carry-over across days would surprise more than help.
 */export function stagedDecisionsKey(project: string, taskID: string): string {
  return `multigent-design-delta-staged:${project}:${taskID}`
}

export function stageDecisions(project: string, taskID: string, decisions: DesignDeltaDecision[]): void {
  try {
    sessionStorage.setItem(stagedDecisionsKey(project, taskID), JSON.stringify(decisions))
  } catch {
    // storage unavailable (private mode etc.): rework still works, the next
    // audit just re-asks everything.
  }
}

export function loadStagedDecisions(project: string, taskID: string): DesignDeltaDecision[] {
  try {
    const raw = sessionStorage.getItem(stagedDecisionsKey(project, taskID))
    if (!raw) return []
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter(
      (d): d is DesignDeltaDecision =>
        !!d && typeof d === 'object' && typeof (d as DesignDeltaDecision).delta_id === 'string',
    )
  } catch {
    return []
  }
}

export function clearStagedDecisions(project: string, taskID: string): void {
  try {
    sessionStorage.removeItem(stagedDecisionsKey(project, taskID))
  } catch {
    // ignore
  }
}
