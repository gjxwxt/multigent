import React from 'react'
import { Link } from 'react-router-dom'
import { Item } from '../types'

interface ItemCardProps {
  item: Item
  onDelete: (id: string) => void
  deleting?: boolean
}

export const ItemCard: React.FC<ItemCardProps> = ({ item, onDelete, deleting }) => {
  return (
    <div className="item-row" data-testid={`item-row-${item.id}`}>
      <div>
        {/* Title links to the detail route: the sample of list -> detail navigation */}
        <Link className="item-title" to={`/items/${encodeURIComponent(item.id)}`} style={{ textDecoration: 'none' }}>
          {item.title}
        </Link>
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
          disabled={deleting}
          aria-label={`Delete ${item.title}`}
        >
          Delete
        </button>
      </div>
    </div>
  )
}
