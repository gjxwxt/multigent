import { lazy, Suspense, ReactNode } from 'react'

const LazyItemDetailPage = lazy(() =>
  import('./ItemDetailPage').then((m) => ({ default: m.ItemDetailPage })),
)

function SuspenseFallback({ children }: { children: ReactNode }) {
  return <Suspense fallback={<div className="card">Loading…</div>}>{children}</Suspense>
}

export { LazyItemDetailPage, SuspenseFallback }
