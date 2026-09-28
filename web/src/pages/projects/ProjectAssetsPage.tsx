import { useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Download, Upload } from 'lucide-react'
import { confirmDialog } from '../../components/ui/ConfirmDialog'
import { showToast } from '../../components/ui/Toast'
import { apiDelete, apiPostForm } from '../../lib/api'
import { useApiJson } from '../../lib/use-api'

type AssetRow = {
  id: string
  displayName: string
  currentSha: string
  createdBy: string
  createdAt: string
  archivedAt?: string
  usageCount: number
  size?: number
  mime?: string
}

function formatBytes(n?: number): string {
  if (n == null) return '—'
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)}MB`
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))}KB`
  return `${n}B`
}

const buttonCls =
  'inline-flex items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 py-1.5 text-sm font-medium text-neutral-700 shadow-sm hover:bg-neutral-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-200 dark:hover:bg-zinc-700'

export default function ProjectAssetsPage() {
  const { projectId = '' } = useParams()
  const { t } = useTranslation()
  const [reloadKey, setReloadKey] = useState(0)
  const [showArchived, setShowArchived] = useState(false)
  const [uploading, setUploading] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const listPath = projectId
    ? `/api/v1/projects/${encodeURIComponent(projectId)}/assets${showArchived ? '?includeArchived=1' : ''}`
    : null
  const state = useApiJson<AssetRow[]>(listPath, reloadKey)
  const rows = state.status === 'ok' ? state.data : []

  const reload = () => setReloadKey((k) => k + 1)

  async function onFilesSelected(files: FileList | null) {
    if (!files || files.length === 0 || !projectId) return
    setUploading(true)
    try {
      const form = new FormData()
      for (const f of Array.from(files)) form.append('file', f)
      await apiPostForm(`/api/v1/projects/${encodeURIComponent(projectId)}/assets`, form)
      showToast(t('projectAssets.uploadDone'), 'success')
      reload()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setUploading(false)
      if (fileInputRef.current) fileInputRef.current.value = ''
    }
  }

  async function archive(row: AssetRow) {
    const ok = await confirmDialog({
      title: t('projectAssets.archiveConfirmTitle', { name: row.displayName }),
      description: t('projectAssets.archiveConfirmDescription'),
      confirmLabel: t('projectAssets.archive'),
      cancelLabel: t('common.cancel', { defaultValue: 'Cancel' }),
    })
    if (!ok) return
    try {
      await apiDelete(`/api/v1/projects/${encodeURIComponent(projectId)}/assets/${encodeURIComponent(row.id)}`)
      showToast(t('projectAssets.archiveDone'), 'success')
      reload()
    } catch (e) {
      showToast(e instanceof Error ? e.message : String(e), 'error')
    }
  }

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="shrink-0 px-6 pt-5 pb-3">
        <div className="flex items-center justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold text-neutral-900 dark:text-zinc-100">{t('projectNav.assets')}</h1>
            <p className="mt-0.5 text-sm text-neutral-500 dark:text-zinc-500">{t('projectAssets.subtitle')}</p>
          </div>
          <div className="flex items-center gap-3">
            <label className="flex cursor-pointer select-none items-center gap-1.5 text-sm text-neutral-600 dark:text-zinc-400">
              <input
                type="checkbox"
                checked={showArchived}
                onChange={(e) => {
                  setShowArchived(e.target.checked)
                  setReloadKey((k) => k + 1)
                }}
                className="h-3.5 w-3.5"
              />
              {t('projectAssets.showArchived')}
            </label>
            <button
              type="button"
              className={buttonCls}
              disabled={uploading}
              onClick={() => fileInputRef.current?.click()}
            >
              <Upload className="h-4 w-4" />
              {uploading ? t('projectAssets.uploading') : t('projectAssets.upload')}
            </button>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              onChange={(e) => void onFilesSelected(e.target.files)}
            />
          </div>
        </div>
      </div>

      <div className="flex-1 overflow-y-auto px-6 py-3">
        {state.status === 'ok' && rows.length === 0 && (
          <div className="rounded-xl border border-dashed border-neutral-200 p-10 text-center text-sm text-neutral-500 dark:border-zinc-700 dark:text-zinc-500">
            {t('projectAssets.empty')}
          </div>
        )}
        {rows.length > 0 && (
          <table className="w-full text-left text-sm">
            <thead>
              <tr className="border-b border-neutral-200 text-xs uppercase tracking-wide text-neutral-500 dark:border-zinc-700 dark:text-zinc-500">
                <th className="py-2 pr-3 font-medium">{t('projectAssets.name')}</th>
                <th className="py-2 pr-3 font-medium">{t('projectAssets.size')}</th>
                <th className="py-2 pr-3 font-medium">{t('projectAssets.sha')}</th>
                <th className="py-2 pr-3 font-medium">{t('projectAssets.usage')}</th>
                <th className="py-2 pr-3 font-medium">{t('projectAssets.uploader')}</th>
                <th className="py-2 pr-3 font-medium">{t('projectAssets.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.id} className="border-b border-neutral-100 last:border-0 dark:border-zinc-800">
                  <td className="py-2.5 pr-3">
                    <span className="font-medium text-neutral-800 dark:text-zinc-200">{row.displayName}</span>
                    {row.archivedAt && (
                      <span className="ml-2 rounded bg-neutral-100 px-1.5 py-0.5 text-xs text-neutral-500 dark:bg-zinc-800 dark:text-zinc-500">
                        {t('projectAssets.archived')}
                      </span>
                    )}
                  </td>
                  <td className="py-2.5 pr-3 text-neutral-600 dark:text-zinc-400">{formatBytes(row.size)}</td>
                  <td className="py-2.5 pr-3 font-mono text-xs text-neutral-500 dark:text-zinc-500">
                    {row.currentSha.slice(0, 10)}…
                  </td>
                  <td className="py-2.5 pr-3 text-neutral-600 dark:text-zinc-400">{row.usageCount}</td>
                  <td className="py-2.5 pr-3 text-neutral-600 dark:text-zinc-400">{row.createdBy}</td>
                  <td className="py-2.5 pr-3">
                    <div className="flex items-center gap-2">
                      <a
                        href={`/api/v1/projects/${encodeURIComponent(projectId)}/assets/${encodeURIComponent(row.id)}/download`}
                        className="inline-flex items-center gap-1 text-neutral-600 hover:text-neutral-900 dark:text-zinc-400 dark:hover:text-zinc-100"
                        title={t('projectAssets.download')}
                      >
                        <Download className="h-4 w-4" />
                      </a>
                      {!row.archivedAt && (
                        <button
                          type="button"
                          onClick={() => void archive(row)}
                          className="text-sm text-red-600 hover:underline dark:text-red-400"
                        >
                          {t('projectAssets.archive')}
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
