import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronDown, ChevronUp, Plus, Trash2 } from 'lucide-react'
import { cn } from '../../lib/cn'

// Editor for a DeliveryPlan DRAFT (large-requirement wizard stage 1+2).
// The draft is created by POST /delivery/preplan and edited here before the
// task is created. Deep validation runs server-side at freeze time
// (contract_review); this editor enforces the same structural rules cheaply
// client-side (unique ids, no self-dependency, ≥1 acceptance criterion for
// non-infra packages) and computes the wave layout for display.

export type PlanRequirementItem = { id: string; text: string; source?: string }
export type PlanContractItem = { id: string; artifact: string; path?: string; owner?: string; evidence?: string }
export type PlanWorkPackage = {
  id: string
  title: string
  domain?: string
  details?: string
  dependsOn: string[]
  acceptanceCriteria: string[]
  agentBinding?: string
  actorRole?: string
  expectedDelivery?: string[]
  contractRefs?: string[]
}
export type DeliveryPlanDraft = {
  schemaVersion: number
  planId: string
  version: number
  requirementItems?: PlanRequirementItem[]
  sharedContract?: PlanContractItem[]
  workPackages: PlanWorkPackage[]
  integrationPolicy?: string
  qaPolicy?: string
}

const WP_ID_RE = /^[a-z0-9][a-z0-9_-]*$/

// Wave layout mirrors workflow.PlanWaves: wave k contains packages whose
// dependencies all live in earlier waves. Cycles surface as "no progress"
// and are reported by the caller via validation below.
export function planWaves(wps: PlanWorkPackage[]): string[][] {
  const byId = new Map(wps.map((w) => [w.id, w]))
  const waveOf = new Map<string, number>()
  let changed = true
  let guard = 0
  while (changed && guard <= wps.length + 1) {
    changed = false
    guard++
    for (const wp of wps) {
      if (waveOf.has(wp.id)) continue
      const deps = (wp.dependsOn ?? []).filter((d) => byId.has(d))
      if (deps.every((d) => waveOf.has(d))) {
        waveOf.set(wp.id, deps.length === 0 ? 0 : Math.max(...deps.map((d) => waveOf.get(d)!)) + 1)
        changed = true
      }
    }
  }
  const waves: string[][] = []
  for (const [id, wave] of waveOf) {
    ;(waves[wave] ??= []).push(id)
  }
  return waves.filter(Boolean)
}

export function validatePlanDraft(plan: DeliveryPlanDraft, t: (k: string, o?: Record<string, unknown>) => string): string[] {
  const errors: string[] = []
  const ids = new Set<string>()
  const reqIds = new Set((plan.requirementItems ?? []).map((r) => r.id))
  for (const wp of plan.workPackages) {
    if (!wp.id.trim()) errors.push(t('tasks.preplan.error.emptyId'))
    else if (!WP_ID_RE.test(wp.id.trim())) errors.push(t('tasks.preplan.error.badId', { id: wp.id }))
    if (ids.has(wp.id.trim())) errors.push(t('tasks.preplan.error.dupId', { id: wp.id }))
    ids.add(wp.id.trim())
    if (!wp.title.trim()) errors.push(t('tasks.preplan.error.noTitle', { id: wp.id || '?' }))
    if ((wp.acceptanceCriteria ?? []).length === 0) {
      errors.push(t('tasks.preplan.error.noAC', { id: wp.id || '?' }))
    } else {
      for (const ac of wp.acceptanceCriteria) {
        if (!ac.startsWith('infra:') && !reqIds.has(ac)) {
          errors.push(t('tasks.preplan.error.unknownAC', { id: wp.id || '?', ac }))
        }
      }
    }
    for (const dep of wp.dependsOn ?? []) {
      if (dep === wp.id.trim()) errors.push(t('tasks.preplan.error.selfDep', { id: wp.id }))
    }
  }
  const waves = planWaves(plan.workPackages)
  if (waves.reduce((n, w) => n + w.length, 0) !== plan.workPackages.length) {
    errors.push(t('tasks.preplan.error.cycle'))
  }
  return errors
}

type Props = {
  plan: DeliveryPlanDraft
  onChange: (plan: DeliveryPlanDraft) => void
  agentNames: string[]
}

export function DeliveryPlanEditor({ plan, onChange, agentNames }: Props) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState<string | null>(plan.workPackages[0]?.id ?? null)
  const [showContracts, setShowContracts] = useState(false)

  const waves = useMemo(() => planWaves(plan.workPackages), [plan.workPackages])
  const waveIndex = useMemo(() => {
    const m = new Map<string, number>()
    waves.forEach((wave, i) => wave.forEach((id) => m.set(id, i)))
    return m
  }, [waves])
  const errors = useMemo(() => validatePlanDraft(plan, t), [plan, t])

  function update(id: string, patch: Partial<PlanWorkPackage>) {
    onChange({ ...plan, workPackages: plan.workPackages.map((wp) => (wp.id === id ? { ...wp, ...patch } : wp)) })
  }
  function addPackage() {
    let n = plan.workPackages.length
    let id = `wp-${n}`
    while (plan.workPackages.some((w) => w.id === id)) id = `wp-${++n}`
    const next: PlanWorkPackage = { id, title: '', dependsOn: [], acceptanceCriteria: [], agentBinding: '', actorRole: 'developer-agent' }
    onChange({ ...plan, workPackages: [...plan.workPackages, next] })
    setExpanded(id)
  }
  function removePackage(id: string) {
    onChange({
      ...plan,
      workPackages: plan.workPackages
        .filter((w) => w.id !== id)
        .map((w) => ({ ...w, dependsOn: (w.dependsOn ?? []).filter((d) => d !== id) })),
    })
    if (expanded === id) setExpanded(null)
  }

  return (
    <div className="rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-zinc-700 dark:bg-zinc-950/40">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-sm font-medium text-neutral-700 dark:text-zinc-300">{t('tasks.preplan.editorTitle')}</span>
        <span className="text-xs text-neutral-500 dark:text-zinc-400">
          {t('tasks.preplan.waveSummary', { waves: waves.length, wps: plan.workPackages.length })}
        </span>
      </div>

      {(plan.requirementItems?.length ?? 0) > 0 && (
        <p className="mt-1 text-xs text-neutral-500 dark:text-zinc-400">
          {t('tasks.preplan.reqItemCount', { count: plan.requirementItems!.length })}
        </p>
      )}

      {errors.length > 0 && (
        <ul className="mt-2 space-y-0.5 rounded-md bg-red-50 p-2 text-xs text-red-700 dark:bg-red-950/30 dark:text-red-400">
          {errors.slice(0, 6).map((e, i) => (
            <li key={i}>· {e}</li>
          ))}
        </ul>
      )}

      <div className="mt-2 space-y-1.5">
        {plan.workPackages.map((wp) => {
          const wave = waveIndex.get(wp.id) ?? 0
          const open = expanded === wp.id
          return (
            <div key={wp.id} className="rounded-md border border-neutral-200 bg-white dark:border-zinc-700 dark:bg-zinc-900">
              <div className="flex items-center gap-2 p-2">
                <button
                  type="button"
                  onClick={() => setExpanded(open ? null : wp.id)}
                  className="flex min-w-0 flex-1 items-center gap-2 text-left"
                >
                  {open ? <ChevronUp className="size-3.5 shrink-0 text-neutral-400" /> : <ChevronDown className="size-3.5 shrink-0 text-neutral-400" />}
                  <span className="font-mono text-xs text-neutral-400 dark:text-zinc-500">{wp.id}</span>
                  <span className="min-w-0 truncate text-sm text-neutral-800 dark:text-zinc-200">{wp.title || t('tasks.preplan.untitled')}</span>
                  {wp.dependsOn.length > 0 && (
                    <span className="shrink-0 text-[11px] text-neutral-400 dark:text-zinc-500">← {wp.dependsOn.join(', ')}</span>
                  )}
                </button>
                <span className="shrink-0 rounded bg-sky-50 px-1.5 py-0.5 text-[11px] font-medium text-sky-700 dark:bg-sky-950/60 dark:text-sky-400">
                  {t('tasks.preplan.waveN', { n: wave + 1 })}
                </span>
                <button
                  type="button"
                  onClick={() => removePackage(wp.id)}
                  className="shrink-0 rounded p-1 text-neutral-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-950/40"
                  title={t('tasks.preplan.remove')}
                >
                  <Trash2 className="size-3.5" />
                </button>
              </div>
              {open && (
                <div className="space-y-2 border-t border-neutral-100 p-2 dark:border-zinc-800">
                  <div className="grid grid-cols-[92px_minmax(0,1fr)] items-center gap-2">
                    <span className="text-xs text-neutral-500 dark:text-zinc-400">{t('tasks.preplan.wpTitle')}</span>
                    <input value={wp.title} onChange={(e) => update(wp.id, { title: e.target.value })} className={inputCls} />
                  </div>
                  <div className="grid grid-cols-[92px_minmax(0,1fr)] items-center gap-2">
                    <span className="text-xs text-neutral-500 dark:text-zinc-400">{t('tasks.preplan.wpAgent')}</span>
                    <select value={wp.agentBinding ?? ''} onChange={(e) => update(wp.id, { agentBinding: e.target.value })} className={inputCls}>
                      <option value="">{t('tasks.preplan.wpAgentEmpty')}</option>
                      {agentNames.map((name) => (
                        <option key={name} value={name}>{name}</option>
                      ))}
                    </select>
                  </div>
                  <div className="grid grid-cols-[92px_minmax(0,1fr)] items-start gap-2">
                    <span className="pt-1.5 text-xs text-neutral-500 dark:text-zinc-400">{t('tasks.preplan.wpAC')}</span>
                    <div className="flex flex-wrap gap-1">
                      {(plan.requirementItems ?? []).map((req) => {
                        const active = wp.acceptanceCriteria.includes(req.id)
                        return (
                          <button
                            key={req.id}
                            type="button"
                            title={req.source ? `${req.text}（${req.source}）` : req.text}
                            onClick={() =>
                              update(wp.id, {
                                acceptanceCriteria: active
                                  ? wp.acceptanceCriteria.filter((a) => a !== req.id)
                                  : [...wp.acceptanceCriteria, req.id],
                              })
                            }
                            className={cn(
                              'rounded px-1.5 py-0.5 font-mono text-[11px] transition-colors',
                              active
                                ? 'bg-sky-600 text-white'
                                : 'bg-neutral-100 text-neutral-600 hover:bg-neutral-200 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700',
                            )}
                          >
                            {req.id}
                          </button>
                        )
                      })}
                      <button
                        type="button"
                        onClick={() => update(wp.id, { acceptanceCriteria: [...wp.acceptanceCriteria, 'infra:' + (wp.id || 'work')] })}
                        className="rounded bg-amber-50 px-1.5 py-0.5 font-mono text-[11px] text-amber-700 hover:bg-amber-100 dark:bg-amber-950/40 dark:text-amber-400"
                      >
                        + infra
                      </button>
                    </div>
                  </div>
                  <div className="grid grid-cols-[92px_minmax(0,1fr)] items-start gap-2">
                    <span className="pt-1.5 text-xs text-neutral-500 dark:text-zinc-400">{t('tasks.preplan.wpDeps')}</span>
                    <div className="flex flex-wrap gap-1">
                      {plan.workPackages.filter((w) => w.id !== wp.id).map((w) => {
                        const active = wp.dependsOn.includes(w.id)
                        return (
                          <button
                            key={w.id}
                            type="button"
                            onClick={() =>
                              update(wp.id, {
                                dependsOn: active ? wp.dependsOn.filter((d) => d !== w.id) : [...wp.dependsOn, w.id],
                              })
                            }
                            className={cn(
                              'rounded px-1.5 py-0.5 font-mono text-[11px] transition-colors',
                              active
                                ? 'bg-violet-600 text-white'
                                : 'bg-neutral-100 text-neutral-600 hover:bg-neutral-200 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700',
                            )}
                          >
                            {w.id}
                          </button>
                        )
                      })}
                    </div>
                  </div>
                  <div className="grid grid-cols-[92px_minmax(0,1fr)] items-center gap-2">
                    <span className="text-xs text-neutral-500 dark:text-zinc-400">{t('tasks.preplan.wpDelivery')}</span>
                    <input
                      value={(wp.expectedDelivery ?? []).join(', ')}
                      onChange={(e) =>
                        update(wp.id, {
                          expectedDelivery: e.target.value.split(',').map((s) => s.trim()).filter(Boolean),
                        })
                      }
                      placeholder="api/, db/"
                      className={cn(inputCls, 'font-mono text-xs')}
                    />
                  </div>
                  <div className="grid grid-cols-[92px_minmax(0,1fr)] items-start gap-2">
                    <span className="pt-1.5 text-xs text-neutral-500 dark:text-zinc-400">{t('tasks.preplan.wpDomain')}</span>
                    <textarea
                      value={wp.domain ?? ''}
                      onChange={(e) => update(wp.id, { domain: e.target.value })}
                      rows={2}
                      className={cn(inputCls, 'resize-y')}
                    />
                  </div>
                </div>
              )}
            </div>
          )
        })}
      </div>

      <div className="mt-2 flex items-center justify-between">
        <button
          type="button"
          onClick={addPackage}
          className="inline-flex items-center gap-1 rounded-md border border-dashed border-neutral-300 px-2 py-1 text-xs text-neutral-600 hover:border-sky-400 hover:text-sky-700 dark:border-zinc-600 dark:text-zinc-300 dark:hover:border-sky-600"
        >
          <Plus className="size-3" />
          {t('tasks.preplan.addWp')}
        </button>
        {(plan.sharedContract?.length ?? 0) > 0 && (
          <button type="button" onClick={() => setShowContracts(!showContracts)} className="text-xs text-neutral-500 hover:text-neutral-700 dark:text-zinc-400 dark:hover:text-zinc-200">
            {t('tasks.preplan.contracts', { count: plan.sharedContract!.length })}
          </button>
        )}
      </div>
      {showContracts && (
        <ul className="mt-1 space-y-0.5 rounded-md bg-neutral-100 p-2 text-xs text-neutral-600 dark:bg-zinc-900 dark:text-zinc-300">
          {plan.sharedContract!.map((c) => (
            <li key={c.id}>
              <span className="font-mono">{c.id}</span> · {c.artifact}
              {c.path ? <span className="ml-1 font-mono text-neutral-400 dark:text-zinc-500">{c.path}</span> : null}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

const inputCls =
  'w-full rounded-md border border-neutral-300 bg-white px-2 py-1 text-sm text-neutral-800 outline-none focus:border-sky-500 dark:border-zinc-600 dark:bg-zinc-950 dark:text-zinc-200'
