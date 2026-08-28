import { useEffect, useState } from 'react'

type Health = { status: string; service: string }

export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/health')
      .then((response) => {
        if (!response.ok) throw new Error(`API returned ${response.status}`)
        return response.json() as Promise<Health>
      })
      .then(setHealth)
      .catch((reason: unknown) => setError(reason instanceof Error ? reason.message : String(reason)))
  }, [])

  return (
    <main className="shell">
      <section className="card">
        <p className="eyebrow">MULTIGENT STARTER</p>
        <h1>React + Go Fullstack</h1>
        <p className="intro">一个可构建、可预览、可由智能助手继续迭代的前后端一体项目。</p>
        <div className={health ? 'status ok' : error ? 'status error' : 'status pending'}>
          <span className="dot" />
          {health ? `Go API 已连接 · ${health.status}` : error ? `Go API 不可达 · ${error}` : '正在检查 Go API…'}
        </div>
        <p className="hint">修改 <code>web/src/App.tsx</code> 后刷新即可看到前端更新。</p>
      </section>
    </main>
  )
}
