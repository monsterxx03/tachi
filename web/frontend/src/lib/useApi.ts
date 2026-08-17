import { useCallback, useEffect, useState } from 'react'
import type { AuthReporter } from '../App'

interface UseApiResult<T> {
  data: T | null
  loading: boolean
  error: string | null
  reload: () => void
}

/**
 * Loads an API resource with built-in auth-error reporting and reload.
 * Swallows AuthError (the app gate handles it) and surfaces other errors
 * as a dismissible message.
 */
export function useApi<T>(
  fetcher: () => Promise<T>,
  deps: unknown[],
  onAuthError?: AuthReporter,
): UseApiResult<T> {
  const [data, setData] = useState<T | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [nonce, setNonce] = useState(0)

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)
    fetcher()
      .then((d) => {
        if (cancelled) return
        setData(d)
        setLoading(false)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        if (err instanceof Error && err.name === 'AuthError') {
          onAuthError?.(err)
          // keep loading state; the gate will unmount this view
          return
        }
        setError(err instanceof Error ? err.message : String(err))
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  return { data, loading, error, reload }
}