import { useEffect, useState } from 'react'

type Health = { status: string; service: string; db?: string }
type Customer = { id: number; name: string; email: string; tier: string }
type Order = {
  id: number
  customer_id: number
  title: string
  amount_cents: number
  status: string
}

const statusLabel: Record<string, string> = {
  draft: '草稿',
  placed: '已下单',
  paid: '已支付',
  shipped: '已发货',
  delivered: '已交付',
  cancelled: '已取消',
}

export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [orders, setOrders] = useState<Order[]>([])
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/health')
      .then((response) => {
        if (!response.ok) throw new Error(`API returned ${response.status}`)
        return response.json() as Promise<Health>
      })
      .then(setHealth)
      .catch((reason: unknown) => setError(reason instanceof Error ? reason.message : String(reason)))
    fetch('/api/orders?limit=10')
      .then((response) => (response.ok ? (response.json() as Promise<Order[]>) : []))
      .then(setOrders)
      .catch(() => setOrders([]))
  }, [])

  const yuan = (cents: number) => (cents / 100).toFixed(2)

  return (
    <main className="shell">
      <section className="card">
        <p className="eyebrow">MULTIGENT STARTER</p>
        <h1>React + Go Fullstack</h1>
        <p className="intro">一个可构建、可预览、可由智能助手继续迭代的前后端一体项目。</p>
        <div className={health ? 'status ok' : error ? 'status error' : 'status pending'}>
          <span className="dot" />
          {health
            ? `Go API 已连接 · ${health.status}${health.db ? ` · db ${health.db}` : ''}`
            : error
              ? `Go API 不可达 · ${error}`
              : '正在检查 Go API…'}
        </div>

        <h2>最近订单</h2>
        {orders.length === 0 ? (
          <p className="hint">暂无订单数据 — 运行 <code>make db:seed:baseline</code> 装载演示基线。</p>
        ) : (
          <table className="orders">
            <thead>
              <tr>
                <th>#</th>
                <th>标题</th>
                <th>金额</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              {orders.map((o) => (
                <tr key={o.id}>
                  <td>{o.id}</td>
                  <td>{o.title}</td>
                  <td>¥{yuan(o.amount_cents)}</td>
                  <td>{statusLabel[o.status] ?? o.status}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <p className="hint">修改 <code>web/src/App.tsx</code> 后刷新即可看到前端更新。</p>
      </section>
    </main>
  )
}
