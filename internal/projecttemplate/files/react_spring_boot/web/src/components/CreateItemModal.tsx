import React, { useState } from 'react'
import { CreateItemRequest } from '../types'

interface CreateItemModalProps {
  isOpen: boolean
  onClose: () => void
  onSubmit: (req: CreateItemRequest) => Promise<void>
}

export const CreateItemModal: React.FC<CreateItemModalProps> = ({ isOpen, onClose, onSubmit }) => {
  const [title, setTitle] = useState('')
  const [description, setDescription] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (!isOpen) return null

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!title.trim()) {
      setError('Title cannot be empty')
      return
    }
    try {
      setLoading(true)
      setError(null)
      await onSubmit({ title: title.trim(), description: description.trim() })
      setTitle('')
      setDescription('')
      onClose()
    } catch (err: any) {
      setError(err?.message || 'Failed to create item')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content" onClick={(e) => e.stopPropagation()}>
        <h2>Create New Item</h2>
        {error && <div className="status error" style={{ marginBottom: '12px' }}>{error}</div>}
        <form onSubmit={handleSubmit}>
          <div style={{ marginBottom: '16px' }}>
            <label style={{ fontSize: '13px', fontWeight: 600, color: '#334155' }}>
              Title *
            </label>
            <input
              type="text"
              className="input"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="e.g. Implement User Authentication"
              autoFocus
            />
          </div>
          <div style={{ marginBottom: '16px' }}>
            <label style={{ fontSize: '13px', fontWeight: 600, color: '#334155' }}>
              Description
            </label>
            <textarea
              className="input"
              rows={3}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Optional notes or details..."
            />
          </div>
          <div className="modal-actions">
            <button type="button" className="btn btn-secondary" onClick={onClose} disabled={loading}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={loading}>
              {loading ? 'Creating...' : 'Create'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
