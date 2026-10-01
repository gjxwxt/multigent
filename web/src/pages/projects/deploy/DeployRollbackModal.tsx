// DeployRollbackModal：回滚确认弹窗。
// 规范 v2 §1.4：回滚遵循不可变原则——不执行 git reset，而是以目标单的 SHA
// 新建一张部署单（create）并在确认后立即触发（trigger），全程留痕台账。
import { useState } from 'react'
import { AlertTriangle, History } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { shortSha, type DeployRequest } from './deploy-shared'

export function DeployRollbackModal({
  target,
  busy,
  onConfirm,
  onClose,
}: {
  target: DeployRequest | null
  busy: boolean
  onConfirm: (req: DeployRequest) => Promise<void>
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [error, setError] = useState('')

  if (!target) return null

  const confirm = async () => {
    setError('')
    try {
      await onConfirm(target)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onClick={onClose}>
      <div
        className="w-full max-w-md rounded-xl border border-neutral-200 bg-white p-5 shadow-xl dark:border-zinc-700 dark:bg-zinc-900"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start gap-3">
          <div className="rounded-full bg-amber-100 p-2 dark:bg-amber-950/60">
            <AlertTriangle className="size-5 text-amber-600 dark:text-amber-400" />
          </div>
          <div className="min-w-0">
            <h3 className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">
              {t('projectDeploy.rollbackTitle', { defaultValue: '确认回滚生产环境？' })}
            </h3>
            <p className="mt-1 text-xs leading-relaxed text-neutral-500 dark:text-zinc-400">
              {t('projectDeploy.rollbackSubtitle', {
                defaultValue: '遵循不可变原则：回滚将作为一张新部署单生成并审计，不会改写 Git 历史。',
              })}
            </p>
          </div>
        </div>

        <div className="mt-4 rounded-lg bg-neutral-50 p-3 text-xs dark:bg-zinc-800/60">
          <div className="flex items-center justify-between gap-2">
            <span className="text-neutral-500 dark:text-zinc-400">{t('projectDeploy.rollbackTarget', { defaultValue: '回滚目标' })}</span>
            <span className="font-mono font-semibold text-neutral-800 dark:text-zinc-200">
              {target.branch}@{shortSha(target.sha)}
            </span>
          </div>
          <div className="mt-1.5 flex items-center justify-between gap-2">
            <span className="text-neutral-500 dark:text-zinc-400">{t('projectDeploy.rollbackSource', { defaultValue: '来源部署单' })}</span>
            <span className="font-mono text-neutral-700 dark:text-zinc-300">{target.id}</span>
          </div>
        </div>

        {error && (
          <p className="mt-3 rounded-lg bg-red-50 px-3 py-2 text-xs text-red-600 dark:bg-red-950/50 dark:text-red-400">{error}</p>
        )}

        <div className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            disabled={busy}
            className="rounded-lg border border-neutral-200 bg-white px-4 py-2 text-sm font-medium text-neutral-700 hover:bg-neutral-50 disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200"
          >
            {t('projectDeploy.cancelConfirm', { defaultValue: '取消' })}
          </button>
          <button
            type="button"
            onClick={() => void confirm()}
            disabled={busy}
            className="inline-flex items-center gap-2 rounded-lg bg-amber-600 px-4 py-2 text-sm font-medium text-white hover:bg-amber-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            <History className="size-4" />
            {busy ? t('projectDeploy.rollbackWorking', { defaultValue: '执行中…' }) : t('projectDeploy.rollbackConfirm', { defaultValue: '创建回滚部署单' })}
          </button>
        </div>
      </div>
    </div>
  )
}
