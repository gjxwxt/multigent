import { request } from '../http'
import { CreateItemRequest, Item } from '../../types'

export async function getItems(): Promise<Item[]> {
  return request<Item[]>('/api/v1/items')
}

export async function getItem(id: string): Promise<Item> {
  return request<Item>(`/api/v1/items/${encodeURIComponent(id)}`)
}

export async function createItem(req: CreateItemRequest): Promise<Item> {
  return request<Item>('/api/v1/items', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

export async function deleteItem(id: string): Promise<void> {
  await request<void>(`/api/v1/items/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  })
}
