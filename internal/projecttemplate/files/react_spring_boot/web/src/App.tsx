import { Route, Routes } from 'react-router-dom'
import { Layout } from './components/Layout'
import { ErrorBoundary } from './components/ErrorBoundary'
import { HomePage } from './pages/HomePage'
import { LazyItemDetailPage, SuspenseFallback } from './pages/lazy'
import { NotFoundPage } from './pages/NotFoundPage'
import { useAppData } from './hooks/useAppData'

/*
 * Route table = the page inventory. Adding a page:
 *   1. Create src/pages/<Name>Page.tsx
 *   2. Register a <Route> here. Once the page count grows, switch it to
 *      React.lazy(() => import(...)) + <Suspense> so each page becomes its
 *      own chunk (ItemDetailPage below is the working example).
 * The <BrowserRouter> itself lives in main.tsx so tests can inject a
 * MemoryRouter around <App /> instead of nesting two routers.
 */
export function App() {
  const { healthStatus } = useAppData()

  return (
    <ErrorBoundary>
      <Layout healthStatus={healthStatus}>
        <Routes>
          <Route path="/" element={<HomePage />} />
          <Route
            path="/items/:itemId"
            element={
              <SuspenseFallback>
                <LazyItemDetailPage />
              </SuspenseFallback>
            }
          />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </Layout>
    </ErrorBoundary>
  )
}
