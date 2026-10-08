import React from 'react'

interface ErrorBoundaryState {
  error: Error | null
}

/**
 * Root-level crash guard. Any uncaught render error swaps the whole tree for a
 * recoverable fallback instead of a blank page. Keep it at the App root — it
 * cannot catch errors in event handlers or async callbacks (those stay with
 * the caller), only errors thrown during rendering.
 */
export class ErrorBoundary extends React.Component<{ children: React.ReactNode }, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error }
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error('Unhandled render error:', error, info.componentStack)
  }

  render() {
    if (this.state.error) {
      return (
        <div className="card" role="alert">
          <h2>Something went wrong</h2>
          <p style={{ color: '#64748b' }}>
            The UI crashed while rendering. The error details below may help diagnose it.
          </p>
          <pre style={{ background: '#f8fafc', border: '1px solid #e2e8f0', borderRadius: 8, padding: 12, overflow: 'auto', fontSize: 12 }}>
            {this.state.error.message}
          </pre>
          <button className="btn btn-primary" onClick={() => window.location.reload()}>
            Refresh page
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
