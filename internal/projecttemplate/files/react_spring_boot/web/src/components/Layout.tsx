import React from 'react'

interface LayoutProps {
  children: React.ReactNode
  healthStatus: 'ok' | 'error' | 'pending'
}

export const Layout: React.FC<LayoutProps> = ({ children, healthStatus }) => {
  return (
    <div className="shell">
      <header className="header">
        <div className="brand">
          <span>⚡</span>
          <span>Spring Boot + React Starter</span>
        </div>
        <div className={`status ${healthStatus}`}>
          <span className="dot" />
          <span>
            {healthStatus === 'ok' && 'Backend Connected (Java 21)'}
            {healthStatus === 'error' && 'Backend Offline'}
            {healthStatus === 'pending' && 'Connecting...'}
          </span>
        </div>
      </header>
      <main className="container">{children}</main>
    </div>
  )
}
