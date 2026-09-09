import React, { useState } from 'react'
import { CreateItemModal } from '../components/CreateItemModal'
import { ItemCard } from '../components/ItemCard'
import { api } from '../services/api'
import { CreateItemRequest, HealthResponse, Item } from '../types'

interface HomePageProps {
  health: HealthResponse | null
  items: Item[]
  onRefresh: () => void
}

export const HomePage: React.FC<HomePageProps> = ({ health, items, onRefresh }) => {
  const [modalOpen, setModalOpen] = useState(false)
  const [deletingId, setDeletingId] = useState<string | null>(null)

  const handleCreate = async (req: CreateItemRequest) => {
    await api.createItem(req)
    onRefresh()
  }

  const handleDelete = async (id: string) => {
    try {
      setDeletingId(id)
      await api.deleteItem(id)
      onRefresh()
    } finally {
      setDeletingId(null)
    }
  }

  return (
    <>
      <div className="card">
        <p className="eyebrow">Enterprise Stack</p>
        <h1>Spring Boot 3.3 + React 18</h1>
        <p className="intro">
          This starter is initialized with a robust layered backend (Java 21, Gradle, JUnit 5 MockMvc tests)
          and a modular React frontend (Vite, TypeScript, Vitest).
        </p>

        {health && (
          <div style={{ fontSize: '13px', color: '#64748b' }}>
            System Probe: <code>/api/health</code> reported status <strong>{health.status}</strong> at{' '}
            {new Date(health.timestamp).toLocaleTimeString()}
          </div>
        )}
      </div>

      <div className="card">
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '16px' }}>
          <div>
            <h2 style={{ margin: 0 }}>Item Management ({items.length})</h2>
            <p style={{ margin: '4px 0 0', fontSize: '13px', color: '#64748b' }}>
              Full CRUD example backed by thread-safe Spring service and repository
            </p>
          </div>
          <button className="btn btn-primary" onClick={() => setModalOpen(true)}>
            + Add Item
          </button>
        </div>

        {items.length === 0 ? (
          <div style={{ padding: '32px', textAlign: 'center', color: '#94a3b8' }}>
            No items found. Click "+ Add Item" to create one.
          </div>
        ) : (
          <div>
            {items.map((item) => (
              <ItemCard key={item.id} item={item} onDelete={handleDelete} />
            ))}
          </div>
        )}
      </div>

      <CreateItemModal
        isOpen={modalOpen}
        onClose={() => setModalOpen(false)}
        onSubmit={handleCreate}
      />
    </>
  )
}
