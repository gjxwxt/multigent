// Client-side transparency for delivery-plan freeze refusals (S2 operability
// slice 1). The backend already fails closed with field-level diagnostics;
// this module ONLY re-renders that message in a structured way (which field,
// expected shape, one-line fix) — it never alters the decision itself and the
// request_changes resubmit path stays the existing review channel.

export type FreezeRejectionHint = {
  field: string
  expected: string
  fix: string
}

type Pattern = {
  match: RegExp
  build: (m: RegExpMatchArray) => FreezeRejectionHint
}

// Keep the patterns in sync with internal/workflow/plan_source.go and
// internal/workflow/plan.go — the backend owns the wording; we only match it.
const PATTERNS: Pattern[] = [
  {
    match: /^(delivery plan(?: JSON)?|requirement_items) is not parseable \((.+?)\); payload starts with/i,
    build: (m) => ({
      field: m[1],
      expected: m[1].toLowerCase() === 'requirement_items'
        ? 'JSON 数组，每项 {"id","text","source"}（对象数组，不是裸字符串数组）'
        : 'JSON 对象：planId/workPackages（可选 sharedContract）+ 独立的 requirement_items 快照，不是裸字符串',
      fix: m[1].toLowerCase() === 'requirement_items'
        ? '把 requirement_items 作为对象数组重新输出（id 稳定、text 完整、source 可追溯），走 request_changes 重发。'
        : '把 delivery_plan 作为结构化 JSON 对象重新输出（含 workPackages 数组），走 request_changes 重发。',
    }),
  },
  {
    match: /^delivery plan source is empty/i,
    build: () => ({
      field: 'delivery_plan',
      expected: '结构化 delivery_plan（或 batch_plan）JSON 输出',
      fix: '上游未产出任何计划 JSON；先用 request_changes 让契约批步骤补齐 delivery_plan 再批冻结。',
    }),
  },
  {
    match: /^delivery plan carries no work packages/i,
    build: () => ({
      field: 'delivery_plan.workPackages',
      expected: '至少一个工作包（批量计划必须分解需求）',
      fix: '在 delivery_plan 中补 workPackages 数组（每项含 id），再走 request_changes 重发。',
    }),
  },
  {
    match: /^delivery plan work package #(\d+) has no id \(accepts id, branchId or wpId\)/i,
    build: (m) => ({
      field: `delivery_plan.workPackages[${Number(m[1]) - 1}].id`,
      expected: '小写字母/数字/中划线/下划线组成的稳定 id',
      fix: `第 ${m[1]} 个工作包缺 id；补上后走 request_changes 重发。`,
    }),
  },
  {
    match: /^delivery plan work package id "(.+?)" is not sanitized/i,
    build: (m) => ({
      field: `delivery_plan.workPackages.id ("${m[1]}")`,
      expected: '仅小写字母、数字、中划线（-）、下划线（_）',
      fix: `把 "${m[1]}" 改成合规 id（例如把冒号/斜杠换成中划线），走 request_changes 重发。`,
    }),
  },
  {
    match: /^delivery plan work package "(.+?)" references unknown requirement item "(.+?)"/i,
    build: (m) => ({
      field: `delivery_plan.workPackages["${m[1]}"].requirementRefs`,
      expected: '引用的 requirement id 必须存在于 requirement_items 快照',
      fix: `工作包 "${m[1]}" 引用了不存在的需求 "${m[2]}"；对齐 id 后走 request_changes 重发。`,
    }),
  },
  {
    match: /^delivery plan work package "(.+?)" references unknown shared contract/i,
    build: (m) => ({
      field: `delivery_plan.workPackages["${m[1]}"].contractRefs`,
      expected: '引用的 shared contract id 必须存在于契约产物',
      fix: `工作包 "${m[1]}" 引用了不存在的契约；对齐契约 id 后走 request_changes 重发。`,
    }),
  },
  {
    match: /^delivery plan has no requirement-items snapshot/i,
    build: () => ({
      field: 'requirement_items',
      expected: '人工批准过的需求锚点快照（除纯 infra 计划外必填）',
      fix: '契约批步骤未重发 requirement_items；用 request_changes 要求重发结构化锚点后再批冻结。',
    }),
  },
  {
    match: /^requirement_items contains an entry without an id/i,
    build: () => ({
      field: 'requirement_items[].id',
      expected: '每个锚点都要有稳定 id',
      fix: '补齐每项的 id 字段后走 request_changes 重发。',
    }),
  },
  {
    match: /^requirement_items parsed to an empty anchor list/i,
    build: () => ({
      field: 'requirement_items',
      expected: '非空锚点数组',
      fix: 'requirement_items 解析为空；输出至少一条锚点后走 request_changes 重发。',
    }),
  },
]

// Returns null for anything that is not a recognized freeze refusal — callers
// then render the raw server message unchanged (honest passthrough).
export function describeFreezeRejection(message: string): FreezeRejectionHint | null {
  // The backend wraps freeze refusals as `delivery_plan_freeze_rejected: <err>`
  // (workflow_handlers.go / workflow_trigger_handlers.go); the underlying
  // patterns below match the UNWRAPPED error text, so strip the wrapper first
  // (review round 2 finding: without this the hints were dead code on the
  // only production path).
  const WRAPPER = 'delivery_plan_freeze_rejected: '
  let trimmed = (message ?? '').trim()
  if (trimmed.startsWith(WRAPPER)) trimmed = trimmed.slice(WRAPPER.length).trim()
  for (const p of PATTERNS) {
    const m = trimmed.match(p.match)
    if (m) return p.build(m)
  }
  return null
}
