import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Archive, ChevronRight, ChevronsDownUp, ChevronsUpDown, Pencil, Trash2, X } from 'lucide-react'
import { cn } from '../../lib/cn'
import { useFormatDateTime } from '../../lib/format-datetime'
import { formatGoDuration, taskElapsedLabel } from '../../lib/task-duration'
import {
  type TaskRow,
  statusColor,
  priorityLabel,
  isTerminal,
  taskIdentityLabel,
} from './TaskModals'
import { TaskLabel } from './TaskLabel'
import { TaskResourcesPopover } from './TaskResourcesPopover'

type Props = {
  tasks: TaskRow[]
  checked: Set<string>
  allChecked: boolean
  someChecked: boolean
  onToggleAll: () => void
  onToggleOne: (id: string) => void
  onRowClick: (task: TaskRow) => void
  onEdit: (task: TaskRow) => void
  onCancel: (task: TaskRow, e: React.MouseEvent) => void
  onArchive: (task: TaskRow, e: React.MouseEvent) => void
  onDelete: (task: TaskRow, e: React.MouseEvent) => void
  showProject?: boolean
  canMutateTask?: (task: TaskRow) => boolean
  canDeleteTask?: (task: TaskRow) => boolean
}

/* ── tree flattening (prototype: work/task-tree-prototype.html) ─── */

/**
 * One 22px tree-line column:
 *  - pass:    full-height vertical line (ancestor above still has siblings below)
 *  - blank:   no line (the ancestor owning this column was the last sibling —
 *             its connector line already ended at its own row center)
 *  - corner:  own row, last child → vertical stops at center + horizontal stub
 *  - tee:     own row, not last → full-height vertical + horizontal stub
 */
type RailSeg = 'pass' | 'blank' | 'corner' | 'tee'

type FlatRow = {
  row: TaskRow
  rails: RailSeg[]
  hasKids: boolean
  childCount: number
  /** parentId is set but the parent row is not in this dataset (other page,
   *  filtered out, archived) — render as a top-level row, never drop it. */
  orphan: boolean
}

function flattenTaskTree(tasks: TaskRow[], collapsed: Set<string>): FlatRow[] {
  const present = new Set(tasks.map((t) => t.id))
  const kidsByParent = new Map<string, TaskRow[]>()
  for (const t of tasks) {
    if (!t.parentId || t.parentId === t.id || !present.has(t.parentId)) continue
    const list = kidsByParent.get(t.parentId)
    if (list) list.push(t)
    else kidsByParent.set(t.parentId, [t])
  }

  const out: FlatRow[] = []
  const emitted = new Set<string>()

  // railCount: total 22px columns this row occupies (ancestor columns + its
  // own connector); ancestorCols lists the inherited modes, isLast decides
  // the connector shape. Root rows (railCount 0) hang nothing.
  const walk = (row: TaskRow, railCount: number, ancestorCols: RailSeg[], isLast: boolean) => {
    if (emitted.has(row.id)) return
    emitted.add(row.id)
    const kids = kidsByParent.get(row.id) ?? []
    const isCollapsed = kids.length > 0 && collapsed.has(row.id)
    const rails: RailSeg[] = railCount === 0 ? [] : [...ancestorCols, isLast ? 'corner' : 'tee']
    out.push({
      row,
      rails,
      hasKids: kids.length > 0,
      childCount: kids.length,
      orphan: railCount === 0 && !!row.parentId && row.parentId !== row.id && !present.has(row.parentId),
    })
    if (isCollapsed) return
    const total = kids.length
    kids.forEach((kid, i) => {
      // My connector column becomes the child's last inherited column: the
      // vertical continues below only while I still have siblings coming.
      const childAncestorCols: RailSeg[] = railCount === 0 ? [] : [...ancestorCols, isLast ? 'blank' : 'pass']
      walk(kid, railCount + 1, childAncestorCols, i === total - 1)
    })
  }

  for (const t of tasks) {
    if (t.parentId && t.parentId !== t.id && present.has(t.parentId)) continue
    walk(t, 0, [], false)
  }
  // Defensive: a pathological parentId cycle (every row has a present parent)
  // would leave rows unemitted — render them flat rather than losing rows.
  for (const t of tasks) {
    if (!emitted.has(t.id)) walk(t, 0, [], false)
  }
  return out
}

/* ── batch (wave) helpers ─── */

/**
 * Wave number per work package: prefer the explicit `waveIndex` task var;
 * otherwise derive it from the dependency DAG carried in `wpDepends`
 * (wave(wp) = 1 + max wave of its deps; 1 when it has none). The backend
 * pins waves on the frozen delivery plan, so for fan-out tasks the derived
 * value matches the plan the human approved; the explicit var is
 * future-proofing for when the backend starts writing it.
 */
function computeWaveByWp(tasks: TaskRow[]): Map<string, number> {
  const depsByWp = new Map<string, string[]>()
  const explicit = new Map<string, number>()
  for (const t of tasks) {
    const wp = (t.vars?.wpId ?? '').trim()
    if (!wp) continue
    if (!depsByWp.has(wp)) depsByWp.set(wp, [])
    const wi = Number.parseInt(t.vars?.waveIndex ?? '', 10)
    if (Number.isFinite(wi) && wi > 0) explicit.set(wp, wi)
    const deps = (t.vars?.wpDepends ?? '').split(',').map((s) => s.trim()).filter(Boolean)
    if (deps.length > 0) depsByWp.set(wp, deps)
  }
  const memo = new Map<string, number>()
  const visiting = new Set<string>()
  const waveOf = (wp: string): number => {
    const forced = explicit.get(wp)
    if (forced != null) return forced
    const cached = memo.get(wp)
    if (cached != null) return cached
    if (visiting.has(wp)) return 1 // cycle guard: never hang
    visiting.add(wp)
    let w = 1
    for (const dep of depsByWp.get(wp) ?? []) {
      w = Math.max(w, (depsByWp.has(dep) ? waveOf(dep) : 1) + 1)
    }
    visiting.delete(wp)
    memo.set(wp, w)
    return w
  }
  const out = new Map<string, number>()
  for (const wp of depsByWp.keys()) out.set(wp, waveOf(wp))
  return out
}

/** Prototype palette: wave 1 sky, wave 2 violet, wave 3 emerald (the three
 *  execution waves of a delivery plan); higher/unknown waves stay neutral. */
const waveChipCls: Record<number, string> = {
  1: 'bg-sky-100 text-sky-700 dark:bg-sky-900/40 dark:text-sky-300',
  2: 'bg-violet-100 text-violet-700 dark:bg-violet-900/40 dark:text-violet-300',
  3: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300',
}
const waveChipNeutral = 'bg-neutral-100 text-neutral-600 dark:bg-zinc-800 dark:text-zinc-400'

export function TaskTable({
  tasks,
  checked,
  allChecked,
  someChecked,
  onToggleAll,
  onToggleOne,
  onRowClick,
  onEdit,
  onCancel,
  onArchive,
  onDelete,
  showProject = false,
  canMutateTask = () => true,
  canDeleteTask = () => true,
}: Props) {
  const { t } = useTranslation()
  const fmt = useFormatDateTime()
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set())

  const flatRows = useMemo(() => flattenTaskTree(tasks, collapsed), [tasks, collapsed])
  // Parents within the current dataset — computed from `tasks` (not the
  // visible rows) so collapse-all also collapses nested parents that are
  // currently hidden behind a collapsed ancestor.
  const parentIds = useMemo(() => {
    const present = new Set(tasks.map((t) => t.id))
    const ids = new Set<string>()
    for (const t of tasks) {
      if (t.parentId && t.parentId !== t.id && present.has(t.parentId)) ids.add(t.parentId)
    }
    return [...ids]
  }, [tasks])
  // Tree-ness must come from the DATASET (parent relationships), not the
  // rendered rows: when every parent is collapsed no child row renders and
  // no visible row has a rail — deriving from flatRows would unmount the
  // caret column and the header expand-all button, destroying the only
  // way to expand again.
  const hasTree = parentIds.length > 0
  const waveByWp = useMemo(() => computeWaveByWp(tasks), [tasks])

  function toggleCollapsed(id: string) {
    setCollapsed((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  return (
    <div className="overflow-x-auto rounded-lg border border-neutral-200/80 dark:border-zinc-700/60">
      <table className={cn('w-full', showProject ? 'min-w-[1100px]' : 'min-w-[1000px]')}>
        <thead>
          <tr className="border-b border-neutral-200/80 bg-neutral-50/80 dark:border-zinc-700/60 dark:bg-zinc-900/40">
            <th className="w-10 px-3 py-2.5 text-center">
              <input
                type="checkbox"
                checked={allChecked}
                ref={(el) => { if (el) el.indeterminate = someChecked && !allChecked }}
                onChange={onToggleAll}
                className="size-3.5 rounded border-neutral-300 accent-sky-600 dark:border-zinc-600"
              />
            </th>
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              <div className="flex items-center gap-2">
                <span>{t('api.taskColTitle')}</span>
                {hasTree && parentIds.length > 0 && (
                  <span className="ml-auto flex items-center gap-0.5">
                    <button
                      type="button"
                      onClick={() => setCollapsed(new Set())}
                      className="rounded p-1 text-neutral-400 transition-colors hover:bg-neutral-200/70 hover:text-neutral-600 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
                      title={t('tasks.expandAll', { defaultValue: '全部展开' })}
                    >
                      <ChevronsUpDown className="size-3.5" strokeWidth={1.8} />
                    </button>
                    <button
                      type="button"
                      onClick={() => setCollapsed(new Set(parentIds))}
                      className="rounded p-1 text-neutral-400 transition-colors hover:bg-neutral-200/70 hover:text-neutral-600 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
                      title={t('tasks.collapseAll', { defaultValue: '全部折叠' })}
                    >
                      <ChevronsDownUp className="size-3.5" strokeWidth={1.8} />
                    </button>
                  </span>
                )}
              </div>
            </th>
            {showProject && (
              <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
                {t('workbench.filterProject')}
              </th>
            )}
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('tasks.colAssignee')}
            </th>
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('api.taskColStatus')}
            </th>
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('forms.priority')}
            </th>
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('api.taskColUpdated')}
            </th>
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('tasks.colEstimate')}
            </th>
            <th className="px-4 py-2.5 text-left text-xs font-semibold uppercase tracking-wider text-neutral-400 dark:text-zinc-500">
              {t('tasks.colElapsed')}
            </th>
            <th className="sticky right-0 bg-neutral-50/95 px-4 py-2.5 text-right text-xs font-semibold uppercase tracking-wider text-neutral-400 backdrop-blur-sm dark:bg-zinc-900/95 dark:text-zinc-500">
              {t('messages.actions')}
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-neutral-100 dark:divide-zinc-800/40">
          {flatRows.map(({ row, rails, hasKids, childCount, orphan }) => {
            const prio = priorityLabel[row.priority] ?? priorityLabel[2]
            const sCls = statusColor[row.status] ?? statusColor.pending
            const terminal = isTerminal(row.status)
            const isChecked = checked.has(row.id)
            const canMutate = canMutateTask(row)
            const canDelete = canDeleteTask(row)
            const isCollapsed = hasKids && collapsed.has(row.id)
            const wpId = (row.vars?.wpId ?? '').trim()
            const wave = wpId ? waveByWp.get(wpId) : undefined
            const depList = (row.vars?.wpDepends ?? '').split(',').map((s) => s.trim()).filter(Boolean)
            return (
              <tr
                key={row.id}
                onClick={() => onRowClick(row)}
                className={cn(
                  'group cursor-pointer transition-colors duration-100',
                  isChecked
                    ? 'bg-sky-50/60 dark:bg-sky-900/[0.10]'
                    : 'bg-white hover:bg-neutral-50/80 dark:bg-zinc-900/20 dark:hover:bg-zinc-800/30',
                )}
              >
                <td className="w-10 px-3 py-3 text-center align-middle" onClick={(e) => e.stopPropagation()}>
                  {(canMutate || canDelete) && (
                    <input
                      type="checkbox"
                      checked={isChecked}
                      onChange={() => onToggleOne(row.id)}
                      className="size-3.5 rounded border-neutral-300 accent-sky-600 dark:border-zinc-600"
                    />
                  )}
                </td>
                <td className="px-4 py-3 align-middle">
                  <div className="flex items-start">
                    {hasTree && (
                      /* Tree-line rail + collapse caret (prototype .indent/.rail/.caret).
                       * Negative margins undo the td padding so rail coordinates
                       * cover the FULL row height and consecutive rails join seamlessly. */
                      <span
                        className="relative flex shrink-0 items-stretch self-stretch"
                        style={{ marginLeft: -16, marginTop: -12, marginBottom: -12 }}
                        onClick={(e) => e.stopPropagation()}
                      >
                        {rails.map((seg, i) => (
                          <span key={i} className="relative w-[22px]">
                            {seg === 'pass' && <span className="absolute inset-y-0 left-[9px] w-px bg-zinc-300 dark:bg-zinc-700" />}
                            {seg === 'corner' && <span className="absolute left-[9px] top-0 bottom-1/2 w-px bg-zinc-300 dark:bg-zinc-700" />}
                            {seg === 'tee' && <span className="absolute inset-y-0 left-[9px] w-px bg-zinc-300 dark:bg-zinc-700" />}
                            {(seg === 'corner' || seg === 'tee') && <span className="absolute left-[10px] top-1/2 w-[12px] h-px bg-zinc-300 dark:bg-zinc-700" />}
                          </span>
                        ))}
                        <button
                          type="button"
                          onClick={(e) => { e.stopPropagation(); if (hasKids) toggleCollapsed(row.id) }}
                          tabIndex={hasKids ? 0 : -1}
                          className={cn(
                            'flex size-7 shrink-0 items-center justify-center self-center rounded-md transition-colors',
                            hasKids
                              ? 'cursor-pointer text-neutral-400 hover:bg-neutral-200/70 hover:text-neutral-700 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-zinc-200'
                              : 'pointer-events-none invisible',
                          )}
                          title={isCollapsed ? t('tasks.expandSubtree', { defaultValue: '展开子任务' }) : t('tasks.collapseSubtree', { defaultValue: '折叠子任务' })}
                        >
                          <ChevronRight className={cn('size-4 transition-transform duration-150', !isCollapsed && 'rotate-90')} strokeWidth={2} />
                        </button>
                      </span>
                    )}
                    <div className={cn('min-w-0 flex-1', hasTree && 'pl-2.5')}>
                      <div className="flex flex-wrap items-center gap-2">
                        <span className={cn('text-[11px] font-bold', prio.cls)}>{prio.text}</span>
                        <span className="text-[13px] font-medium text-neutral-900 dark:text-zinc-100">{row.title}</span>
                        {wpId && (
                          <span
                            className={cn('inline-flex max-w-[240px] items-center truncate rounded-full px-2 py-0.5 text-[10px] font-semibold', wave != null ? (waveChipCls[wave] ?? waveChipNeutral) : waveChipNeutral)}
                            title={wave != null ? t('tasks.waveBatch', { n: String(wave), defaultValue: '批次 {{n}}' }) : wpId}
                          >
                            <span className="truncate">{wave != null ? t('tasks.waveBatch', { n: String(wave), defaultValue: '批次 {{n}}' }) : t('tasks.batchPlain', { defaultValue: '批次' })} · {wpId}</span>
                          </span>
                        )}
                        {isCollapsed && (
                          <span className="rounded-full bg-sky-100 px-2 py-0.5 text-[10px] font-medium text-sky-700 dark:bg-sky-900/40 dark:text-sky-300">
                            {t('tasks.collapsedCount', { count: String(childCount), defaultValue: '+{{count}} 个子任务' })}
                          </span>
                        )}
                        {row.type && (
                          <span className="rounded border border-neutral-200 bg-neutral-50 px-1.5 py-0.5 text-[10px] font-medium text-neutral-500 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-500">
                            {t(`forms.taskType.${row.type}`, { defaultValue: row.type })}
                          </span>
                        )}
                        {row.labels?.map((l) => <TaskLabel key={l} label={l} />)}
                        {row.dueDate && (
                          <span className="text-[10px] text-neutral-400 dark:text-zinc-500">{row.dueDate}</span>
                        )}
                        {orphan && (
                          <span className="text-[10px] text-neutral-400 dark:text-zinc-500" title={t('tasks.parentTask')}>↳ {row.parentId}</span>
                        )}
                        {row.archived && (
                          <Archive className="size-3.5 text-neutral-400 dark:text-zinc-500" strokeWidth={1.5} />
                        )}
                      </div>
                      {depList.length > 0 && (
                        <div className="mt-0.5 text-[10px] text-neutral-400 dark:text-zinc-500">
                          {t('tasks.dependsOn', { deps: depList.join(', '), defaultValue: '依赖: {{deps}}' })}
                        </div>
                      )}
                      <span className="mt-0.5 block font-mono text-[11px] text-neutral-400 dark:text-zinc-500">{row.id}</span>
                    </div>
                  </div>
                </td>
                {showProject && (
                  <td className="whitespace-nowrap px-4 py-3 align-middle">
                    <Link
                      to={`/projects/${encodeURIComponent(row.project)}/tasks`}
                      onClick={(e) => e.stopPropagation()}
                      className="font-mono text-[13px] text-sky-700 underline-offset-2 hover:underline dark:text-sky-400"
                    >
                      {row.project}
                    </Link>
                    <span className="ml-2 font-mono text-[12px] text-neutral-400 dark:text-zinc-500">{row.agent}</span>
                  </td>
                )}
                <td className="whitespace-nowrap px-4 py-3 align-middle">
                  <span className="text-[13px] text-neutral-700 dark:text-zinc-400" title={row.assignee || `${row.project}/${row.agent}`}>
                    {taskIdentityLabel(row.assignee || `${row.project}/${row.agent}`, row.assigneeLabel)}
                  </span>
                </td>
                <td className="whitespace-nowrap px-4 py-3 align-middle">
                  <span className={cn('inline-block rounded-full px-2.5 py-0.5 text-[11px] font-semibold', sCls)}>
                    {t(`tasks.status.${row.status}`, { defaultValue: row.status })}
                  </span>
                </td>
                <td className="whitespace-nowrap px-4 py-3 align-middle">
                  <span className={cn('text-[12px] font-bold', prio.cls)}>{prio.text}</span>
                </td>
                <td className="whitespace-nowrap px-4 py-3 align-middle text-[13px] text-neutral-500 dark:text-zinc-500">
                  {fmt(row.updatedAt)}
                </td>
                <td className="whitespace-nowrap px-4 py-3 align-middle text-[12px] tabular-nums text-neutral-500 dark:text-zinc-500">
                  {row.estimateDuration ? formatGoDuration(row.estimateDuration) : '—'}
                </td>
                <td className="whitespace-nowrap px-4 py-3 align-middle text-[12px] tabular-nums text-neutral-500 dark:text-zinc-500">
                  {taskElapsedLabel(row) ?? '—'}
                </td>
                <td
                  className="sticky right-0 bg-white/95 px-4 py-3 align-middle backdrop-blur-sm group-hover:bg-neutral-50/95 dark:bg-zinc-900/95 dark:group-hover:bg-zinc-800/95"
                  onClick={(e) => e.stopPropagation()}
                >
                  <div className="flex items-center justify-end gap-1 whitespace-nowrap opacity-0 transition-opacity duration-100 group-hover:opacity-100">
                    {row.hasWorkflow && (
                      <Link
                        to={`/projects/${encodeURIComponent(row.project)}/tasks/${encodeURIComponent(row.id)}/follow`}
                        onClick={(e) => e.stopPropagation()}
                        className="rounded px-2 py-1 text-[11px] font-medium text-sky-700 transition-colors hover:bg-sky-50 dark:text-sky-400 dark:hover:bg-sky-900/30"
                        title={t('tasks.follow')}
                      >
                        {t('tasks.follow')}
                      </Link>
                    )}
                    {canMutate && (
                      <button
                        type="button"
                        onClick={(e) => { e.stopPropagation(); onEdit(row) }}
                        className="rounded p-1 text-neutral-500 transition-colors hover:bg-neutral-100 hover:text-neutral-700 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-zinc-300"
                        title={t('tasks.edit')}
                      >
                        <Pencil className="size-3.5" strokeWidth={1.8} />
                      </button>
                    )}
                    {(row.hasWorktree || row.hasPreview) && (
                      <TaskResourcesPopover taskId={row.id} project={row.project} hasWorktree={row.hasWorktree} hasPreview={row.hasPreview} />
                    )}
                    {canMutate && !terminal && !row.archived && (
                      <button
                        type="button"
                        onClick={(e) => onCancel(row, e)}
                        className="rounded p-1 text-amber-600 transition-colors hover:bg-amber-50 dark:text-amber-400 dark:hover:bg-amber-900/30"
                        title={t('tasks.cancel')}
                      >
                        <X className="size-3.5" strokeWidth={1.8} />
                      </button>
                    )}
                    {canMutate && !row.archived && (
                      <button
                        type="button"
                        onClick={(e) => onArchive(row, e)}
                        className="rounded p-1 text-neutral-500 transition-colors hover:bg-neutral-100 dark:text-zinc-500 dark:hover:bg-zinc-800"
                        title={t('tasks.archive')}
                      >
                        <Archive className="size-3.5" strokeWidth={1.8} />
                      </button>
                    )}
                    {canDelete && (
                      <button
                        type="button"
                        onClick={(e) => onDelete(row, e)}
                        className="rounded p-1 text-red-500 transition-colors hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-900/30"
                        title={t('tasks.delete')}
                      >
                        <Trash2 className="size-3.5" strokeWidth={1.8} />
                      </button>
                    )}
                  </div>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
