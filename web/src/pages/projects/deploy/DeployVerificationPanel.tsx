// DeployVerificationPanel：部署后冒烟校验报告（三期a，确定性 HTTP 校验）。
// 数据源 GET /deploy/requests/{id}/verification（规格=回归资产，结果=平台执行快照）。
// 空态如实显示"未生成"（校验是 best-effort 加固，不是闸门）；失败项红色高亮 + 重放按钮。
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CircleCheck, CircleX, PlayCircle, RefreshCw, ShieldCheck } from 'lucide-react'
import { apiFetch, apiPost } from '../../../lib/api'
import { cn } from '../../../lib/cn'

type VerifyCheck = {
  name: string
  method: string
  path: string
  expectedStatus: number
  contains?: string
}

type VerifyResultEntry = {
  name: string
  method: string
  path: string
  passed: boolean
  httpStatus?: number
  expected: number
  latencyMs: number
  error?: string
  detail?: string
}

type VerifyState = {
  spec?: { checks: VerifyCheck[] }
  results?: VerifyResultEntry[]
  passed?: boolean
  generatedBy?: string
  generatedAt?: string
  executedAt?: string
  genError?: string
}

export function DeployVerificationPanel({ projectId, requestId }: { projectId: string; requestId: string }) {
  const { t } = useTranslation()
  const [state, setState] = useState<VerifyState | null>(null)
  const [exists, setExists] = useState<boolean | null>(null)
  const [busy, setBusy] = useState<'run' | 'gen' | null>(null)

  const load = useCallback(async () => {
    try {
      const res = await apiFetch<{ exists: boolean; verification?: VerifyState }>(
        `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(requestId)}/verification`,
        { suppressToast: true },
      )
      setExists(res.exists)
      setState(res.verification ?? null)
    } catch {
      setExists(null)
    }
  }, [projectId, requestId])

  useEffect(() => {
    void load()
  }, [load])

  const runReplay = async () => {
    setBusy('run')
    try {
      const res = await apiPost<{ verification: VerifyState }>(
        `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(requestId)}/verification/run`,
        {},
      )
      setState(res.verification)
    } catch {} finally {
      setBusy(null)
    }
  }

  const regen = async () => {
    setBusy('gen')
    try {
      const res = await apiPost<{ verification: VerifyState }>(
        `/api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(requestId)}/verification/generate`,
        {},
      )
      setState(res.verification)
    } catch {} finally {
      setBusy(null)
    }
  }

  if (exists === null) return null

  const results = state?.results ?? []
  const failedCount = results.filter((r) => !r.passed).length
  const spec = state?.spec

  return (
    <div className="mt-3 rounded-lg border border-neutral-200 bg-white p-3 dark:border-zinc-700 dark:bg-zinc-900/60">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="inline-flex items-center gap-1.5 text-[11px] font-semibold text-neutral-700 dark:text-zinc-200">
          <ShieldCheck className="size-3.5 text-sky-600 dark:text-sky-400" />
          {t('projectDeploy.verifyTitle', { defaultValue: '部署后冒烟校验' })}
          {state?.passed === true && (
            <span className="inline-flex items-center gap-1 rounded-full bg-emerald-50 px-1.5 py-px text-[10px] font-medium text-emerald-700 dark:bg-emerald-950/50 dark:text-emerald-300">
              <CircleCheck className="size-3" />
              {t('projectDeploy.verifyAllPassed', { defaultValue: '全部通过' })}
            </span>
          )}
          {state?.passed === false && (
            <span className="inline-flex items-center gap-1 rounded-full bg-red-50 px-1.5 py-px text-[10px] font-medium text-red-700 dark:bg-red-950/50 dark:text-red-300">
              <CircleX className="size-3" />
              {t('projectDeploy.verifyFailedCount', { defaultValue: '{{count}} 项失败', count: failedCount })}
            </span>
          )}
        </p>
        <div className="flex items-center gap-1.5">
          {spec && results.length > 0 && (
            <button
              type="button"
              onClick={() => void runReplay()}
              disabled={busy !== null}
              className="inline-flex items-center gap-1 rounded-md border border-neutral-200 px-1.5 py-0.5 text-[11px] text-neutral-700 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
            >
              <PlayCircle className="size-3" />
              {busy === 'run'
                ? t('projectDeploy.verifyRunning', { defaultValue: '执行中…' })
                : t('projectDeploy.verifyReplay', { defaultValue: '重放校验' })}
            </button>
          )}
          <button
            type="button"
            onClick={() => void regen()}
            disabled={busy !== null}
            className="inline-flex items-center gap-1 rounded-md border border-neutral-200 px-1.5 py-0.5 text-[11px] text-neutral-700 hover:bg-neutral-50 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
            title={t('projectDeploy.verifyRegenTitle', { defaultValue: '根据部署区间重新生成规格' })}
          >
            <RefreshCw className={cn('size-3', busy === 'gen' && 'animate-spin')} />
            {busy === 'gen'
              ? t('projectDeploy.verifyGenerating', { defaultValue: '生成中…' })
              : t('projectDeploy.verifyRegen', { defaultValue: '重新生成' })}
          </button>
        </div>
      </div>

      {/* 未生成 / 生成失败：如实展示，不伪造结果 */}
      {exists === false && (
        <p className="mt-2 text-[11px] text-neutral-400 dark:text-zinc-500">
          {t('projectDeploy.verifyNotGenerated', { defaultValue: '该部署单未生成校验规格（部署完成时自动生成，或点"重新生成"）' })}
        </p>
      )}
      {exists === true && !spec && (
        <p className="mt-2 text-[11px] text-amber-600 dark:text-amber-400">
          {t('projectDeploy.verifyGenFailed', { defaultValue: '规格生成失败' })}
          {state?.genError ? `：${state.genError}` : ''}
        </p>
      )}

      {/* 结果表 */}
      {results.length > 0 && (
        <div className="mt-2 space-y-1">
          {results.map((r, i) => (
            <div
              key={i}
              className={cn(
                'flex items-start gap-2 rounded-md border px-2 py-1.5 text-[11px]',
                r.passed
                  ? 'border-emerald-100 bg-emerald-50/50 dark:border-emerald-900/60 dark:bg-emerald-950/30'
                  : 'border-red-200 bg-red-50/60 dark:border-red-900/60 dark:bg-red-950/30',
              )}
            >
              {r.passed ? (
                <CircleCheck className="mt-px size-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" />
              ) : (
                <CircleX className="mt-px size-3.5 shrink-0 text-red-500" />
              )}
              <div className="min-w-0 flex-1">
                <p className="truncate font-medium text-neutral-800 dark:text-zinc-100">
                  {r.method} {r.path}
                  <span className="ml-1.5 font-normal text-neutral-400 dark:text-zinc-500">{r.name}</span>
                </p>
                <p className="font-mono text-[10px] text-neutral-500 dark:text-zinc-400">
                  {r.passed
                    ? `HTTP ${r.httpStatus} · ${r.latencyMs}ms`
                    : r.error || `HTTP ${r.httpStatus ?? '—'}（期望 ${r.expected}）`}
                </p>
                {!r.passed && r.detail && (
                  <p className="mt-0.5 line-clamp-2 font-mono text-[10px] text-red-500/80 dark:text-red-400/80">{r.detail}</p>
                )}
              </div>
            </div>
          ))}
          {state?.executedAt && (
            <p className="text-[10px] text-neutral-400 dark:text-zinc-600">
              {t('projectDeploy.verifyExecutedAt', { defaultValue: '执行于' })} {new Date(state.executedAt).toLocaleString()}
            </p>
          )}
        </div>
      )}

      {/* 有规格无结果（生成后未执行） */}
      {spec && results.length === 0 && (
        <p className="mt-2 text-[11px] text-neutral-400 dark:text-zinc-500">
          {t('projectDeploy.verifySpecOnly', { defaultValue: `已生成 ${spec.checks.length} 项校验规格，尚未执行`, count: spec.checks.length })}
        </p>
      )}
    </div>
  )
}
