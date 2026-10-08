import { ApiError } from '../types'

/*
 * HTTP plumbing shared by every api/ domain module.
 *
 * Directory growth rule (keep this structure as the API surface grows):
 *   1. New backend DTO first  -> types/index.ts (or types/<domain>.ts)
 *   2. New endpoint methods   -> api/<domain>.ts (one file per backend domain)
 *   3. Pages import from      -> services/api (the aggregate barrel), never fetch directly
 *
 * Keep this file generic: no endpoint knowledge, no business defaults.
 */
export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json', ...init?.headers },
    ...init,
  })
  return handleResponse<T>(res)
}

export async function handleResponse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let errorData: ApiError
    try {
      errorData = await res.json()
    } catch {
      errorData = {
        code: 'HTTP_' + res.status,
        message: res.statusText || 'Request failed',
        details: [],
        timestamp: new Date().toISOString(),
      }
    }
    throw errorData
  }
  if (res.status === 204) {
    return {} as T
  }
  return res.json()
}
