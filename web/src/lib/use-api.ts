import { useEffect, useRef, useState } from 'react'
import { apiFetch } from './api'

export type ApiState<T> =
  | { status: 'loading' }
  | { status: 'error'; error: Error }
  | { status: 'ok'; data: T }

type UseApiJsonOptions = {
  silentStatuses?: number[]
  /**
   * Reload semantics for the SAME path. When false (default), a reloadKey
   * bump keeps the previous data rendered while refetching in the background
   * (stale-while-revalidate): polling callers no longer flash the whole page
   * through `loading` (deploy page 30s jitter, fixed 2026-10-02). Only a
   * PATH change clears to loading — a different resource must not show the
   * previous resource's data. Pass true to restore the legacy flash-on-reload
   * behavior if a caller genuinely wants it.
   */
  keepPreviousDataOnReload?: boolean
  /** Deprecated no-op kept for callers passing `{ silent: true }`; ignored. */
  silent?: boolean
}

export function useApiJson<T>(path: string | null, reloadKey = 0, options?: UseApiJsonOptions): ApiState<T> {
  const [state, setState] = useState<ApiState<T>>({ status: 'loading' })
  const prevPath = useRef(path)
  const prevReloadKey = useRef(reloadKey)
  // Callers often pass options inline (`{ silentStatuses: [403] }`), so the array
  // identity changes on every render. Depend on a serialized key instead and read
  // the live options through a ref — otherwise the effect re-runs (refetches) on
  // every render, hammering the endpoint in a loop. (Upstream 96580ca fixed the
  // same bug independently; the effect body below reads through the ref.)
  const optionsRef = useRef(options)
  optionsRef.current = options
  const silentStatusesKey = options?.silentStatuses ? options.silentStatuses.join(',') : ''

  useEffect(() => {
    if (path == null) {
      return
    }
    const silentStatuses = optionsRef.current?.silentStatuses
    let cancelled = false
    const pathChanged = prevPath.current !== path
    // reloadChanged intentionally read-and-dropped: a reloadKey bump does NOT
    // clear to loading anymore (stale-while-revalidate), the ref bump below is
    // all the effect needs. Kept as an assignment so the ref write is explicit
    // about which transition each branch covers.
    void (prevReloadKey.current !== reloadKey)
    prevPath.current = path
    prevReloadKey.current = reloadKey
    if (pathChanged) {
      setState({ status: 'loading' })
    }
    const url = reloadKey ? `${path}${path.includes('?') ? '&' : '?'}_=${reloadKey}` : path
    apiFetch<T>(url, silentStatuses ? { silentStatuses } : undefined)
      .then((data) => {
        if (!cancelled) {
          setState({ status: 'ok', data })
        }
      })
      .catch((e) => {
        if (!cancelled) {
          setState({
            status: 'error',
            error: e instanceof Error ? e : new Error(String(e)),
          })
        }
      })
    return () => {
      cancelled = true
    }
  }, [path, reloadKey, silentStatusesKey])

  if (path == null) {
    return { status: 'error', error: new Error('no path') }
  }
  return state
}
