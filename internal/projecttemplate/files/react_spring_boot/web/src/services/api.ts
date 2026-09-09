import { ApiError, CreateItemRequest, HealthResponse, Item } from '../types'

async function handleResponse<T>(res: Response): Promise<T> {
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

export const api = {
  async getHealth(): Promise<HealthResponse> {
    const res = await fetch('/api/health')
    return handleResponse<HealthResponse>(res)
  },

  async getItems(): Promise<Item[]> {
    const res = await fetch('/api/v1/items')
    return handleResponse<Item[]>(res)
  },

  async createItem(req: CreateItemRequest): Promise<Item> {
    const res = await fetch('/api/v1/items', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    })
    return handleResponse<Item>(res)
  },

  async deleteItem(id: string): Promise<void> {
    const res = await fetch(`/api/v1/items/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    })
    await handleResponse<void>(res)
  },
}
