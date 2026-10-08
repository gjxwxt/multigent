import { Link } from 'react-router-dom'

export function NotFoundPage() {
  return (
    <div className="card">
      <h2>Page not found</h2>
      <p style={{ color: '#64748b' }}>The route you requested does not exist.</p>
      <Link className="btn btn-primary" to="/">
        ← Back to home
      </Link>
    </div>
  )
}
