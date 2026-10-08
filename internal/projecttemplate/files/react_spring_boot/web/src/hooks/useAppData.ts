import { useEffect, useState } from 'react'
import { api } from '../services/api'
import { HealthResponse, Item } from '../types'

export function useAppData() {
  const [health, setHealth] = useState<HealthResponse | null>(null)
  const [healthStatus, setHealthStatus] = useState<'ok' | 'error' | 'pending'>('pending')
  const [items, setItems] = useState<Item[]>([])

  const loadData = async () => {
    try {
      const healthData = await api.getHealth()
      setHealth(healthData)
      setHealthStatus('ok')
    } catch {
      setHealthStatus('error')
    }

    try {
      const itemsData = await api.getItems()
      setItems(itemsData)
    } catch {
      // items load error handled gracefully
    }
  }

  useEffect(() => {
    loadData()
  }, [])

  return { health, healthStatus, items, loadData }
}
