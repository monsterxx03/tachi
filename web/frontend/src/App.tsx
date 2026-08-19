import { useCallback, useState, type ReactNode } from 'react'
import { BrowserRouter, Route, Routes } from 'react-router-dom'

import { AuthError } from './api/client'
import { ApiKeyGate } from './auth/ApiKeyGate'
import { TopBar } from './layout/TopBar'
import { Overview } from './views/Overview'
import { Sessions } from './views/Sessions'
import { Usage } from './views/Usage'
import { Inspector } from './views/inspector/Inspector'

/** Global context: any view can report a 401 via this callback. */
export type AuthReporter = (err: unknown) => void

/** App shell: holds auth state, renders the gate when unauthorized.
 *
 * There is deliberately NO startup auth probe: every view already reports
 * 401s through onAuthError (useApi / useSessionList), so the first real
 * request decides. This keeps page loads to exactly the requests the page
 * needs — no extra /api/usage ping on every refresh. */
export default function App() {
  const [authFailed, setAuthFailed] = useState(false)
  // Bumped when the API key is entered, so the route tree re-mounts and
  // every view refetches with the new key.
  const [routeKey, setRouteKey] = useState(0)

  const reportAuth: AuthReporter = useCallback((err) => {
    if (err instanceof AuthError) setAuthFailed(true)
  }, [])

  const onGated = useCallback(() => {
    setAuthFailed(false)
    setRouteKey((k) => k + 1)
  }, [])

  if (authFailed) {
    return <ApiKeyGate onSuccess={onGated} />
  }

  // scrollView wraps the scrollable page views (not the Inspector or the
  // Sessions list, which manage their own scrolling: the Inspector is an
  // h-full split view, and Sessions needs its container ref for infinite
  // scroll).
  function scrollView(node: ReactNode): ReactNode {
    return <div className="h-full overflow-y-auto">{node}</div>
  }

  return (
    <BrowserRouter>
      <div className="flex flex-col h-screen">
        <TopBar />
        <div className="flex-1 overflow-hidden">
          <div className="h-full">
            <Routes key={routeKey}>
              <Route
                path="/"
                element={scrollView(<Overview onAuthError={reportAuth} />)}
              />
              <Route
                path="/sessions"
                element={<Sessions onAuthError={reportAuth} />}
              />
              <Route
                path="/usage"
                element={scrollView(<Usage onAuthError={reportAuth} />)}
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