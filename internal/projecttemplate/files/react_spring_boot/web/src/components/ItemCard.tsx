import React from 'react'
import { Item } from '../types'

interface ItemCardProps {
  item: Item
  onDelete: (id: string) => void
}

export const ItemCard: React.FC<ItemCardProps> = ({ item, onDelete }) => {
  return (
    <div className="item-row" data-testid={`item-row-${item.id}`}>
      <div>
        <div className="item-title">{item.title}</div>
        {item.description && <div className="item-desc">{item.description}</div>}
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
        <span className="status ok" style={{ fontSize: '11px', padding: '4px 8px' }}>
          {item.status}
        </span>
        <button
          className="btn btn-danger"
          style={{ padding: '4px 8px', fontSize: '12px' }}
          onClick={() => onDelete(item.id)}
          aria-label={`Delete ${item.title}`}
        >
          Delete
        </button>
      </div>
    </div>
  )
}
