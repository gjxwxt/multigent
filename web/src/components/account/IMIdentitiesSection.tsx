import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Check, CheckCircle2, Copy, Link2, Loader2, MessageSquare, ShieldAlert, Unlink, X } from 'lucide-react'
import { apiDelete, apiFetch, apiPost } from '../../lib/api'
import { copyTextToClipboard } from '../../lib/clipboard'
import { useFormatDateTime } from '../../lib/format-datetime'
import { confirmDialog } from '../ui/ConfirmDialog'
import { overlayDismissProps } from '../ui/overlay'

type UserIMAgentRoute = {
  bindingId: string
  agentId: string
  projectId?: string
  agentWorkerId?: string
  displayName: string
}

type UserIMConnection = {
  id: string
  name: string
  provider: string
  providerLabel: string
  status: string
  baseUrl?: string
  instanceId?: string
  instanceName?: string
  instanceTrust?: string
  hasAccess: boolean
  bound: boolean
  sharedBinding?: boolean
  sharedFrom?: string
  externalUserId?: string
  externalUsername?: string
  boundAt?: string
  routes?: UserIMAgentRoute[]
}

type UserIMIdentitiesListResp = {
  connections: UserIMConnection[]
}

type BindCodeResp = {
  code: string
  command: string
  expiresAt: string
  provider: string
  connectionId: string
  connectionName: string
}

function getGroupKey(conn: UserIMConnection): string {
	if (conn.instanceId) {
		return `${conn.provider}:instance:${conn.instanceId}`
	}
  if (!conn.baseUrl) {
    return `conn:${conn.id}`
  }
  try {
    const url = new URL(conn.baseUrl)
    if (!url.host) {
      return `conn:${conn.id}`
    }
    return `${conn.provider}:${url.origin.toLowerCase()}`
  } catch {
    return `conn:${conn.id}`
  }
}

function getGroupDisplay(groupConns: UserIMConnection[]) {
  const first = groupConns[0]
  if (!first) return { title: 'IM', subtitle: '', providerLabel: 'IM' }

  if (first.instanceId && first.instanceName) {
    return {
      title: `${first.providerLabel} · ${first.instanceName}`,
      subtitle: first.baseUrl || '',
      providerLabel: first.providerLabel,
      trust: first.instanceTrust,
    }
  }

  let host = ''
  if (first.baseUrl) {
    try {
      const u = new URL(first.baseUrl)
      host = u.host
    } catch {
      host = first.baseUrl
    }
  }

  const isLocal = host.includes('127.0.0.1') || host.includes('localhost')
  const envHint = isLocal ? `本地协作环境 (${host})` : host
  const title = host ? `${first.providerLabel} · ${envHint}` : first.name
  const subtitle = first.baseUrl || ''

  return { title, subtitle, providerLabel: first.providerLabel, trust: '' }
}

function SectionHeader({ title, description }: { title: string; description: string }) {
  return (
    <div className="border-b border-neutral-100 px-5 py-4 dark:border-zinc-800">
      <div className="text-sm font-semibold text-neutral-900 dark:text-zinc-100">{title}</div>
      <p className="mt-0.5 max-w-2xl text-xs leading-5 text-neutral-500 dark:text-zinc-500">{description}</p>
    </div>
  )
}

export function IMIdentitiesSection() {
  const { t } = useTranslation()
  const formatDateTime = useFormatDateTime()
  const [connections, setConnections] = useState<UserIMConnection[]>([])
  const [loading, setLoading] = useState(true)
  const [unbindingGroupKey, setUnbindingGroupKey] = useState<string | null>(null)

  // Bind code modal state
  const [modalConn, setModalConn] = useState<UserIMConnection | null>(null)
  const [modalGroupTitle, setModalGroupTitle] = useState<string | null>(null)
  const [bindCode, setBindCode] = useState<BindCodeResp | null>(null)
  const [bindLoading, setBindLoading] = useState(false)
  const [bindError, setBindError] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const [bindSuccess, setBindSuccess] = useState(false)

  const pollTimerRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const load = useCallback(async () => {
    try {
      const data = await apiFetch<UserIMIdentitiesListResp>('/api/v1/user/im-identities')
      setConnections(data.connections ?? [])
      return data.connections ?? []
    } catch {
      return []
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // Stop polling when modal unmounts
  useEffect(() => {
    return () => {
      if (pollTimerRef.current) {
        clearInterval(pollTimerRef.current)
        pollTimerRef.current = null
      }
    }
  }, [])

  // Start/stop polling when modal is opened/closed
  const startPolling = useCallback((connId: string) => {
    if (pollTimerRef.current) clearInterval(pollTimerRef.current)
    pollTimerRef.current = setInterval(async () => {
      try {
        const data = await apiFetch<UserIMIdentitiesListResp>('/api/v1/user/im-identities')
        const updated = (data.connections ?? []).find(c => c.id === connId)
        if (updated?.bound) {
          setBindSuccess(true)
          if (pollTimerRef.current) {
            clearInterval(pollTimerRef.current)
            pollTimerRef.current = null
          }
          setConnections(data.connections ?? [])
          setTimeout(() => {
            closeModal()
          }, 1800)
        }
      } catch {
        // Ignore transient poll errors
      }
    }, 3000)
  }, [])

  function closeModal() {
    if (pollTimerRef.current) {
      clearInterval(pollTimerRef.current)
      pollTimerRef.current = null
    }
    setModalConn(null)
    setModalGroupTitle(null)
    setBindCode(null)
    setBindError(null)
    setBindSuccess(false)
    setCopied(false)
    void load()
  }

  async function openBindModal(conn: UserIMConnection, groupTitle?: string) {
    setModalConn(conn)
    setModalGroupTitle(groupTitle || null)
    setBindCode(null)
    setBindError(null)
    setBindSuccess(false)
    setCopied(false)
    setBindLoading(true)

    try {
      const res = await apiPost<BindCodeResp>(`/api/v1/user/im-identities/connections/${encodeURIComponent(conn.id)}/bind-code`, {})
      setBindCode(res)
      startPolling(conn.id)
    } catch (err) {
      setBindError(err instanceof Error ? err.message : String(err))
    } finally {
      setBindLoading(false)
    }
  }

  async function copyCommand(cmd: string) {
    if (!await copyTextToClipboard(cmd)) return
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1600)
  }

  async function unbindGroup(group: { key: string; title: string; connections: UserIMConnection[] }) {
    const boundConns = group.connections.filter((c) => c.bound)
    if (boundConns.length === 0) return

    const ok = await confirmDialog({
      title: t('account.imUnbindTitle', { defaultValue: '解除协同身份绑定' }),
      description: t('account.imUnbindInstanceConfirm', {
        defaultValue: `确定要解除与「${group.title}」的协同身份绑定吗？解除后，当前实例下的所有智能体将无法再向您投递通知或发起审批。`,
        name: group.title,
      }),
      confirmLabel: t('account.imUnbindButton', { defaultValue: '确认解绑' }),
      cancelLabel: t('common.cancel', { defaultValue: '取消' }),
      tone: 'danger',
    })
    if (!ok) return

    setUnbindingGroupKey(group.key)
    try {
      for (const c of boundConns) {
        await apiDelete(`/api/v1/user/im-identities/connections/${encodeURIComponent(c.id)}`)
      }
      await load()
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err))
    } finally {
      setUnbindingGroupKey(null)
    }
  }

  const groups = useMemo(() => {
    const map = new Map<string, UserIMConnection[]>()
    for (const conn of connections) {
      const key = getGroupKey(conn)
      const existing = map.get(key)
      if (existing) {
        existing.push(conn)
      } else {
        map.set(key, [conn])
      }
    }

    return Array.from(map.entries()).map(([key, conns]) => {
      const info = getGroupDisplay(conns)
      return {
        key,
        ...info,
        connections: conns,
      }
    })
  }, [connections])

  return (
    <section className="mt-4 overflow-hidden rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40">
      <SectionHeader
        title={t('account.imIdentitiesTitle', { defaultValue: '即时通讯与协同身份' })}
        description={t('account.imIdentitiesDescription', {
          defaultValue: '将您的 Mattermost 或团队协作平台账号与当前 Multigent 账号关联。绑定后可直接在聊天客户端接收工作流通知并使用 /mg 快捷指令。',
        })}
      />

      <div className="space-y-4 px-5 py-5">
        {loading ? (
          <div className="flex items-center justify-center py-8 text-sm text-neutral-400">
            <Loader2 className="mr-2 size-4 animate-spin" />
            {t('common.loading', { defaultValue: '加载中...' })}
          </div>
        ) : connections.length === 0 ? (
          <div className="rounded-lg border border-dashed border-neutral-200 p-6 text-center text-sm text-neutral-400 dark:border-zinc-700">
            <MessageSquare className="mx-auto mb-2 size-6 text-neutral-300 dark:text-zinc-600" />
            <p>{t('account.noIMConnections', { defaultValue: '当前工作区暂未接入任何即时通讯渠道（如 Mattermost、飞书等）。' })}</p>
          </div>
        ) : (
          <div className="space-y-4">
            {groups.map((group) => {
              const boundConn = group.connections.find((c) => c.bound)
              const sharedConn = group.connections.find((c) => c.sharedBinding)
              const isGroupBound = Boolean(boundConn || sharedConn)
              const identityConn = boundConn || sharedConn
              const externalUsername = identityConn?.externalUsername || identityConn?.externalUserId
              const boundAt = identityConn?.boundAt
              const bindableConn = group.connections.find((c) => c.hasAccess) || group.connections[0]
              const hasAnyAccess = group.connections.some((c) => c.hasAccess)

              return (
                <div
                  key={group.key}
                  className="overflow-hidden rounded-xl border border-neutral-200/80 bg-white shadow-sm dark:border-zinc-700/60 dark:bg-zinc-900/50"
                >
                  {/* Group Container Header */}
                  <div className="flex flex-wrap items-center justify-between gap-3 border-b border-neutral-200/60 bg-neutral-50/50 px-4 py-3 sm:px-5 sm:py-3.5 dark:border-zinc-700/60 dark:bg-zinc-800/30">
                    <div className="flex items-center gap-3 min-w-0">
                      <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-sky-50 text-sky-600 ring-1 ring-sky-100 dark:bg-sky-950/40 dark:text-sky-400 dark:ring-sky-900/50">
                        <MessageSquare className="size-4" />
                      </div>
                      <div className="min-w-0">
                        <div className="flex flex-wrap items-center gap-2">
                          <h4 className="truncate text-sm font-semibold text-neutral-900 dark:text-zinc-100">
                            {group.title}
                          </h4>
                          {group.trust === 'admin_attested' ? (
                            <span className="rounded-full bg-sky-50 px-2 py-0.5 text-[11px] font-medium text-sky-700 ring-1 ring-sky-200/60 dark:bg-sky-950/30 dark:text-sky-300 dark:ring-sky-800/40">
                              {t('account.imInstanceAdminAttested', { defaultValue: '管理员确认的实例' })}
                            </span>
                          ) : null}
                        </div>
                        {group.subtitle && (
                          <p className="truncate text-[11px] font-mono text-neutral-400 dark:text-zinc-500">
                            {group.subtitle}
                          </p>
                        )}
                      </div>
                    </div>

                    {/* Right: Instance-level Action Button */}
                    <div className="flex items-center gap-2 shrink-0">
                      {isGroupBound ? (
                        <button
                          type="button"
                          disabled={unbindingGroupKey === group.key}
                          onClick={() => void unbindGroup(group)}
                          className="inline-flex items-center gap-1.5 rounded-md border border-neutral-200 bg-white px-3 py-1.5 text-xs font-medium text-red-600 shadow-sm transition hover:border-red-200 hover:bg-red-50 disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-red-400 dark:hover:border-red-900/50 dark:hover:bg-red-950/20"
                        >
                          <Unlink className="size-3.5" />
                          {unbindingGroupKey === group.key
                            ? t('common.loading', { defaultValue: '处理中...' })
                            : t('account.imUnbindAction', { defaultValue: '解除绑定' })}
                        </button>
                      ) : hasAnyAccess && bindableConn ? (
                        <button
                          type="button"
                          onClick={() => void openBindModal(bindableConn, group.title)}
                          className="inline-flex items-center gap-1.5 rounded-md bg-sky-600 px-3.5 py-1.5 text-xs font-medium text-white shadow-sm transition hover:bg-sky-700 focus:outline-none focus:ring-2 focus:ring-sky-500/20"
                        >
                          <Link2 className="size-3.5" />
                          {t('account.imBindAction', { defaultValue: '绑定账号' })}
                        </button>
                      ) : null}
                    </div>
                  </div>

                  {/* Instance Identity Status Bar */}
                  {isGroupBound ? (
                    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-emerald-100/70 bg-emerald-50/40 px-4 py-2.5 sm:px-5 dark:border-emerald-950/40 dark:bg-emerald-950/10">
                      <div className="flex flex-wrap items-center gap-2 text-xs">
                        <span className="font-medium text-neutral-600 dark:text-zinc-400">
                          {t('account.imExternalAccount', { defaultValue: '协同账号' })}:
                        </span>
                        <span className="font-semibold text-sky-700 dark:text-sky-400">
                          @{externalUsername}
                        </span>
                        {boundAt && (
                          <span className="text-[11px] text-neutral-400 dark:text-zinc-500">
                            · {t('account.imBoundAt', { defaultValue: '绑定于' })} {formatDateTime(boundAt)}
                          </span>
                        )}
                      </div>
                      <div className="flex items-center gap-1.5 text-xs font-medium text-emerald-700 dark:text-emerald-400">
                        <CheckCircle2 className="size-3.5 text-emerald-600 dark:text-emerald-400" />
                        <span>{t('account.imInstanceBoundStatus', { defaultValue: '已连接 (全实例生效)' })}</span>
                      </div>
                    </div>
                  ) : (
                    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-neutral-100 bg-neutral-50/30 px-4 py-2.5 sm:px-5 text-xs text-neutral-500 dark:border-zinc-800 dark:bg-zinc-800/20 dark:text-zinc-400">
                      <p>
                        {t('account.imInstanceUnboundDesc', {
                          defaultValue: '当前实例未绑定协同账号。绑定一次，该实例下所有智能体将自动激活与您的通知和审批协同。',
                        })}
                      </p>
                      <span className="shrink-0 rounded-full bg-neutral-100 px-2.5 py-0.5 text-[11px] font-medium text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                        {t('account.imInstanceUnboundStatus', { defaultValue: '未连接' })}
                      </span>
                    </div>
                  )}

                  {/* Subheader: 可见 Bot 接入路由 & 接入点数量 */}
                  <div className="flex items-center justify-between border-b border-neutral-100 bg-neutral-50/20 px-4 py-2 sm:px-5 text-xs font-medium text-neutral-500 dark:border-zinc-800 dark:bg-zinc-800/10 dark:text-zinc-400">
                    <span>{t('account.visibleBotRoutesTitle', { defaultValue: '可见 Bot 接入路由' })}</span>
                    <span className="text-[11px] text-neutral-400 dark:text-zinc-500 font-normal">
                      {t('account.botEndpointsCount', {
                        defaultValue: '{{count}} 个 Bot 接入点',
                        count: group.connections.length,
                      })}
                    </span>
                  </div>

                  {/* Bot Endpoint Rows with Max-Height & Scrolling */}
                  <div className="max-h-60 sm:max-h-64 overflow-y-auto divide-y divide-neutral-100 dark:divide-zinc-800">
                    {group.connections.map((conn) => {
                      const hasRoutes = conn.routes && conn.routes.length > 0
                      const isReady = isGroupBound && (conn.bound || conn.sharedBinding)

                      return (
                        <div
                          key={conn.id}
                          className="flex items-center justify-between gap-3 px-4 py-3 sm:px-5 transition-colors hover:bg-neutral-50/30 dark:hover:bg-zinc-800/10"
                        >
                          {/* Left: Bot name & routes */}
                          <div className="min-w-0 flex-1">
                            <div className="flex flex-wrap items-center gap-2">
                              <span className="text-xs sm:text-sm font-semibold text-neutral-900 dark:text-zinc-100">
                                {hasRoutes
                                  ? conn.routes!.map((r) => r.displayName).join('、')
                                  : conn.name}
                              </span>
                              <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] sm:text-[11px] font-mono text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                                {conn.name}
                              </span>
                            </div>

                            {!hasRoutes ? (
                              <p className="mt-0.5 text-[11px] text-amber-600 dark:text-amber-400">
                                {conn.bound
                                  ? t('account.boundNoRoutesHint', { defaultValue: '已绑定外部账号，但当前无可见的智能体项目或 Worker。' })
                                  : t('account.noRoutesHint', { defaultValue: '暂无可访问的智能体项目或 Worker。' })}
                              </p>
                            ) : null}
                          </div>

                          {/* Right: Clean Status Badge - NO buttons here! */}
                          <div className="flex shrink-0 items-center gap-2">
                            {isReady ? (
                              <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-emerald-50 px-2.5 py-0.5 text-xs font-medium text-emerald-700 ring-1 ring-emerald-200/60 dark:bg-emerald-950/30 dark:text-emerald-300 dark:ring-emerald-800/40">
                                <CheckCircle2 className="size-3 text-emerald-600 dark:text-emerald-400" />
                                {t('account.imRouteReady', { defaultValue: '已就绪' })}
                              </span>
                            ) : !conn.hasAccess ? (
                              <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-amber-50 px-2.5 py-0.5 text-xs font-medium text-amber-700 ring-1 ring-amber-200/60 dark:bg-amber-950/30 dark:text-amber-300 dark:ring-amber-800/40">
                                <ShieldAlert className="size-3" />
                                {t('account.imNoAccess', { defaultValue: '无可用权限' })}
                              </span>
                            ) : (
                              <span className="inline-flex shrink-0 items-center gap-1.5 rounded-full bg-neutral-100 px-2.5 py-0.5 text-xs font-medium text-neutral-500 dark:bg-zinc-800 dark:text-zinc-400">
                                <span className="size-1.5 rounded-full bg-neutral-400 dark:bg-zinc-500" />
                                {t('account.imRouteWaiting', { defaultValue: '待激活' })}
                              </span>
                            )}
                          </div>
                        </div>
                      )
                    })}
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>

      {/* Bind Code Modal */}
      {modalConn && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 p-4"
          {...overlayDismissProps(closeModal)}
        >
          <div
            className="w-full max-w-md rounded-xl border border-neutral-200 bg-white shadow-xl dark:border-zinc-700 dark:bg-zinc-900"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex items-center justify-between border-b border-neutral-100 px-5 py-4 dark:border-zinc-800">
              <div className="flex items-center gap-2.5 min-w-0">
                <div className="flex size-7 shrink-0 items-center justify-center rounded-md bg-sky-50 text-sky-600 dark:bg-sky-950/40 dark:text-sky-400">
                  <MessageSquare className="size-4" />
                </div>
                <div className="min-w-0">
                  <h3 className="truncate text-sm font-semibold text-neutral-900 dark:text-zinc-100">
                    {t('account.bindModalTitle', { defaultValue: '绑定协作账号' })} · {modalGroupTitle || modalConn.name}
                  </h3>
                  <p className="truncate text-xs text-neutral-400 dark:text-zinc-500">
                    {modalConn.providerLabel} · {t('account.bindModalInstanceTip', { defaultValue: '一次绑定，当前实例下所有智能体自动共享协同。' })}
                  </p>
                </div>
              </div>
              <button
                type="button"
                onClick={closeModal}
                className="rounded-md p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200 shrink-0"
              >
                <X className="size-4" />
              </button>
            </div>

            <div className="px-5 py-5">
              {bindLoading ? (
                <div className="flex flex-col items-center justify-center py-6 text-sm text-neutral-500 dark:text-zinc-400">
                  <Loader2 className="mb-2 size-6 animate-spin text-sky-600" />
                  <p>{t('account.generatingBindCode', { defaultValue: '正在生成绑定码...' })}</p>
                </div>
              ) : bindError ? (
                <div className="rounded-lg bg-red-50 p-3 text-xs text-red-600 dark:bg-red-950/30 dark:text-red-300">
                  {bindError}
                </div>
              ) : bindSuccess ? (
                <div className="flex flex-col items-center justify-center py-6 text-center">
                  <div className="mb-2 flex size-12 items-center justify-center rounded-full bg-emerald-100 text-emerald-600 dark:bg-emerald-950/60 dark:text-emerald-400">
                    <Check className="size-6 stroke-[2.5]" />
                  </div>
                  <h4 className="text-base font-semibold text-neutral-900 dark:text-zinc-100">
                    {t('account.bindSuccessTitle', { defaultValue: '绑定成功！' })}
                  </h4>
                  <p className="mt-1 text-xs text-neutral-500 dark:text-zinc-400">
                    {t('account.bindSuccessDesc', { defaultValue: '您的聊天账号已与当前 Multigent 用户关联，即将自动返回。' })}
                  </p>
                </div>
              ) : bindCode ? (
                <div className="space-y-4">
                  <p className="text-xs leading-5 text-neutral-600 dark:text-zinc-300">
                    {t('account.bindCommandHint', {
                      defaultValue: '请打开您的聊天客户端，在任意频道或与 Bot 的对话窗口中发送以下指令以完成持有认证：',
                    })}
                  </p>

                  <div className="flex items-center gap-2 rounded-lg border border-sky-200 bg-sky-50/50 px-3.5 py-3 dark:border-sky-900/50 dark:bg-sky-950/20">
                    <code className="min-w-0 flex-1 select-all font-mono text-sm font-semibold text-sky-900 dark:text-sky-200">
                      {bindCode.command}
                    </code>
                    <button
                      type="button"
                      onClick={() => void copyCommand(bindCode.command)}
                      className="inline-flex shrink-0 items-center gap-1 rounded-md bg-white px-2.5 py-1 text-xs font-medium text-sky-700 shadow-sm ring-1 ring-neutral-200 transition hover:bg-neutral-50 dark:bg-zinc-800 dark:text-sky-300 dark:ring-zinc-700"
                    >
                      {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
                      {copied ? t('common.copied', { defaultValue: '已复制' }) : t('common.copy', { defaultValue: '复制' })}
                    </button>
                  </div>

                  <div className="flex items-center justify-between text-xs text-neutral-400 dark:text-zinc-500">
                    <span className="inline-flex items-center gap-1.5">
                      <Loader2 className="size-3 animate-spin text-sky-500" />
                      {t('account.waitingVerification', { defaultValue: '等待发送认证命令...' })}
                    </span>
                    <span>{t('account.bindCodeValidDuration', { defaultValue: '10分钟内有效' })}</span>
                  </div>

                  <div className="rounded-lg bg-neutral-50 p-3 text-xs text-neutral-500 dark:bg-zinc-800/40 dark:text-zinc-400">
                    <span className="font-semibold text-neutral-700 dark:text-zinc-300">💡 {t('common.tip', { defaultValue: '提示' })}: </span>
                    {t('account.bindStatusTip', { defaultValue: '绑定完成后在聊天窗口敲入 /mg status 可随时核对权限与协同状态。' })}
                  </div>
                </div>
              ) : null}
            </div>

            <div className="flex justify-end gap-2 border-t border-neutral-100 px-5 py-3 dark:border-zinc-800">
              <button
                type="button"
                onClick={closeModal}
                className="h-8 rounded-md border border-neutral-200 bg-white px-3 text-sm font-medium text-neutral-700 hover:bg-neutral-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800"
              >
                {t('common.close', { defaultValue: '关闭' })}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  )
}
