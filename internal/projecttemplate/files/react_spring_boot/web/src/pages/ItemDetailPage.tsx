import { Link, useParams } from 'react-router-dom'
import { useEffect, useState } from 'react'
import { api } from '../services/api'
import { Item } from '../types'

/*
 * Second sample page demonstrating the cross-page pattern:
 * useParams to read the route param -> api call -> render; a Link back to the
 * list page. Copy this shape for detail/edit pages instead of inventing one.
 */
export function ItemDetailPage() {
  const { itemId } = useParams<{ itemId: string }>()
  const [item, setItem] = useState<Item | null>(null)
  const [status, setStatus] = useState<'loading' | 'ok' | 'missing'>('loading')

  useEffect(() => {
    let cancelled = false
    if (!itemId) {
      setStatus('missing')
      return
    }
    api
      .getItem(itemId)
      .then((data) => {
        if (!cancelled) {
          setItem(data)
          setStatus('ok')
        }
      })
      .catch(() => {
        if (!cancelled) setStatus('missing')
      })
    return () => {
      cancelled = true
    }
  }, [itemId])

  if (status === 'loading') {
    return <div className="card">Loading…</div>
  }
  if (status === 'missing' || !item) {
    return (
      <div className="card">
        <h2>Item not found</h2>
        <p style={{ color: '#64748b' }}>No item exists with id <code>{itemId}</code>.</p>
        <Link className="btn btn-secondary" to="/">
          ← Back to list
        </Link>
      </div>
    )
  }

  return (
    <div className="card">
      <p className="eyebrow">Item Detail</p>
      <h2>{item.title}</h2>
      <span className="status ok" style={{ fontSize: '12px' }}>
        {item.status}
      </span>
      {item.description && <p className="intro">{item.description}</p>}
      <p style={{ fontSize: 13, color: '#64748b' }}>
        Created at {new Date(item.createdAt).toLocaleString()}
      </p>
      <Link className="btn btn-secondary" to="/">
        ← Back to list
      </Link>
    </div>
  )
}
