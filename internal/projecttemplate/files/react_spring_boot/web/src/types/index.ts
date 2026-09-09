export interface HealthResponse {
  status: string
  timestamp: string
  service: string
}

export interface Item {
  id: string
  title: string
  description: string
  status: string
  createdAt: string
}

export interface CreateItemRequest {
  title: string
  description?: string
}

export interface ApiError {
  code: string
  message: string
  details: string[]
  timestamp: string
}
