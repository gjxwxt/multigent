import { request } from '../http'
import { HealthResponse } from '../../types'

export async function getHealth(): Promise<HealthResponse> {
  return request<HealthResponse>('/api/health')
}
