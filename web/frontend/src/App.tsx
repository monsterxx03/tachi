import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { BrowserRouter, Route, Routes } from 'react-router-dom'

import { AuthError, getApiKey } from './api/client'
import { ApiKeyGate } from './auth/ApiKeyGate'
import { TopBar } from './layout/TopBar'
import { Overview } from './views/Overview'
import { Sessions } from './views/Sessions'
import { Usage } from './views/Usage'
import { OneOffs } from './views/OneOffs'
import { Inspector } from './views/inspector/Inspector'

/** Global context: any view can report a 401 via this callback. */
export type AuthReporter = (err: unknown) => void

/** App shell: holds auth state, renders the gate when unauthorized. */
export default function App() {
  const [authFailed, setAuthFailed] = useState(false)
  const [ready, setReady] = useState(false)

  // Probe the API once on startup: no key configured → everything just works.
  useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        await fetch('/api/usage', {
          headers: getApiKey() ? { 'X-Api-Key': getApiKey() } : {},
        })
        if (!cancelled) setAuthFailed(false)
      } catch {
        if (!cancelled) setAuthFailed(true)
      } finally {
        if (!cancelled) setReady(true)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const reportAuth: AuthReporter = useCallback((err) => {
    if (err instanceof AuthError) setAuthFailed(true)
  }, [])

  const onGated = useCallback(() => {
    setAuthFailed(false)
    // Re-mount routes so data refetches with the new key.
    setReady(false)
    setTimeout(() => setReady(true), 0)
  }, [])

  if (!ready) {
    return (
      <div className="flex items-center justify-center h-screen bg-paper text-sm text-inkdim">
        正在连接 Tachi 后端…
      </div>
    )
  }

  if (authFailed) {
    return <ApiKeyGate onSuccess={onGated} />
  }

  // scrollView wraps the scrollable page views (not the Inspector, which
// manages its own internal scrolling as an h-full split view).
function scrollView(node: ReactNode): ReactNode {
  return <div className="h-full overflow-y-auto">{node}</div>
}

return (
    <BrowserRouter>
      <div className="flex flex-col h-screen">
        <TopBar />
        <div className="flex-1 overflow-hidden">
          <div className="h-full">
            <Routes>
              <Route
                path="/"
                element={scrollView(<Overview onAuthError={reportAuth} />)}
              />
              <Route
                path="/sessions"
                element={scrollView(<Sessions onAuthError={reportAuth} />)}
              />
              <Route
                path="/usage"
                element={scrollView(<Usage onAuthError={reportAuth} />)}
              />
              <Route
                path="/oneoffs"
                element={scrollView(<OneOffs onAuthError={reportAuth} />)}
              />
              <Route
                path="/sessions/:id"
                element={<Inspector onAuthError={reportAuth} />}
              />
            </Routes>
          </div>
        </div>
      </div>
    </BrowserRouter>
  )
}