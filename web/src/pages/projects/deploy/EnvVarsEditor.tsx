// EnvVarsEditor：部署环境变量编辑器。
// 规范 v2 §1.5 红线：
// 1. APP_PORT 是 ensureDeployPort 落库并镜像到 GitLab CI 的端口，前端 vars 若提交 APP_PORT
//    会覆盖镜像值导致 deploy job 绑错端口 → 预置行锁定 + 提交时剔除。
// 2. 敏感 key（TOKEN/SECRET/PASSWORD/KEY）平台只存 "***" 掩码：眼睛切换只对"本次新输入"生效，
//    掩码回显行禁止切换明文。
import { useState } from 'react'
import { Eye, EyeOff, Lock, Plus, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '../../../lib/cn'
import { isSensitiveVarKey } from './deploy-shared'

export type DeployVarRow = { key: string; value: string; locked?: boolean; maskedEcho?: boolean }

export function EnvVarsEditor({
  rows,
  onChange,
}: {
  rows: DeployVarRow[]
  onChange: (rows: DeployVarRow[]) => void
}) {
  const { t } = useTranslation()
  // 眼睛开关按行存（只对本次新输入生效；maskedEcho 行显示 *** 且锁死）。
  const [reveal, setReveal] = useState<Record<number, boolean>>({})

  const update = (i: number, patch: Partial<DeployVarRow>) => {
    onChange(rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)))
  }
  const remove = (i: number) => {
    onChange(rows.filter((_, idx) => idx !== i))
  }
  const add = () => onChange([...rows, { key: '', value: '' }])

  return (
    <div className="space-y-2">
      {rows.map((row, i) => {
        const sensitive = isSensitiveVarKey(row.key)
        const locked = Boolean(row.locked)
        const maskedEcho = Boolean(row.maskedEcho)
        const hidden = sensitive && !reveal[i] && !maskedEcho
        return (
          <div key={i} className="flex items-center gap-2">
            <div className="relative flex-1">
              <input
                value={row.key}
                onChange={(e) => update(i, { key: e.target.value })}
                placeholder={t('projectDeploy.varKey', { defaultValue: '变量名（如 LOG_LEVEL）' })}
                disabled={locked}
                className={cn(
                  'w-full rounded-lg border border-neutral-200 bg-white px-3 py-1.5 font-mono text-xs text-neutral-800 outline-none focus:border-sky-400 disabled:bg-neutral-50 disabled:text-neutral-500 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200 dark:disabled:bg-zinc-800/60',
                )}
              />
              {locked && (
                <Lock className="pointer-events-none absolute right-2.5 top-1/2 size-3.5 -translate-y-1/2 text-neutral-400 dark:text-zinc-500" />
              )}
            </div>
            <div className="relative flex-1">
              <input
                value={maskedEcho ? '***' : hidden ? '••••••••' : row.value}
                onChange={(e) => update(i, { value: e.target.value })}
                placeholder={t('projectDeploy.varValue', { defaultValue: '值' })}
                disabled={locked || maskedEcho}
                type={maskedEcho || hidden ? 'password' : 'text'}
                className={cn(
                  'w-full rounded-lg border border-neutral-200 bg-white px-3 py-1.5 font-mono text-xs text-neutral-800 outline-none focus:border-sky-400 disabled:bg-neutral-50 disabled:text-neutral-500 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200 dark:disabled:bg-zinc-800/60',
                  sensitive ? 'pr-9' : '',
                )}
              />
              {sensitive && !maskedEcho && !locked && (
                <button
                  type="button"
                  onClick={() => setReveal((s) => ({ ...s, [i]: !s[i] }))}
                  className="absolute right-2 top-1/2 -translate-y-1/2 text-neutral-400 hover:text-neutral-600 dark:text-zinc-500 dark:hover:text-zinc-300"
                  title={reveal[i] ? t('projectDeploy.maskValue', { defaultValue: '隐藏' }) : t('projectDeploy.revealValue', { defaultValue: '显示' })}
                >
                  {reveal[i] ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
                </button>
              )}
            </div>
            <button
              type="button"
              onClick={() => remove(i)}
              disabled={locked}
              className="rounded-lg p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-red-600 disabled:cursor-not-allowed disabled:opacity-40 dark:text-zinc-500 dark:hover:bg-zinc-800 dark:hover:text-red-400"
              title={locked ? t('projectDeploy.appPortLocked', { defaultValue: 'APP_PORT 由部署端口分配维护，提交时自动剔除' }) : t('projectDeploy.removeVar', { defaultValue: '删除' })}
            >
              <Trash2 className="size-3.5" />
            </button>
          </div>
        )
      })}
      <button
        type="button"
        onClick={add}
        className="inline-flex items-center gap-1.5 text-xs font-medium text-sky-600 hover:text-sky-700 dark:text-sky-400"
      >
        <Plus className="size-3.5" />
        {t('projectDeploy.addVar', { defaultValue: '添加变量' })}
      </button>
      <p className="text-[11px] leading-relaxed text-neutral-400 dark:text-zinc-500">
        {t('projectDeploy.varsHint', {
          defaultValue: '变量以 MULTIGENT_DEPLOY_VAR_ 前缀注入 GitLab CI。名称含 TOKEN/SECRET/PASSWORD/KEY 的视为敏感：仅推送 GitLab，平台只存 *** 掩码。',
        })}
      </p>
    </div>
  )
}

// 提交前净化：剔除 APP_PORT（红线）与空行；保留其余全部。
export function sanitizeVarRows(rows: DeployVarRow[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of rows) {
    const k = r.key.trim()
    if (!k || k === 'APP_PORT' || r.locked) continue
    out[k] = r.value
  }
  return out
}
