import { useCallback, useEffect, useRef, useState } from 'react'
import type { RefObject } from 'react'

import { api } from '../api/client'
import type { AuthReporter } from '../App'
import type { SessionSummaryItem } from '../types/api'

export interface UseSessionListResult {
  sessions: SessionSummaryItem[]
  /** Total number of sessions on disk (not just loaded pages). */
  total: number
  /** First page loading (the whole list is replaced by a loader). */
  loading: boolean
  /** A follow-up page is being fetched. */
  loadingMore: boolean
  /** First-page error (page shows retry UI). */
  error: string | null
  /** Follow-up page error (bottom of the list shows retry). */
  loadMoreError: string | null
  hasMore: boolean
  reload: () => void
  loadMore: () => void
  /** The scrollable container (the page's / sidebar's overflow-y-auto div). */
  containerRef: RefObject<HTMLDivElement>
  /** Sentinel at the end of the list; entering the container's viewport
   *  triggers loading the next page. Render it unconditionally. */
  sentinelRef: RefObject<HTMLDivElement>
}

/** Default page size for the session list; the backend caps at 200. */
export const SESSION_PAGE_SIZE = 50

// Module-level first-page cache shared by the list page and the Inspector
// sidebar: navigating from /sessions into /sessions/:id reuses the list the
// user just browsed instead of re-fetching it (and re-paying the backend's
// per-session usage ledger lookups). reload() invalidates it.
interface SessionListCache {
  sessions: SessionSummaryItem[]
  total: number
  nextCursor: string
  ts: number
}
let listCache: SessionListCache | null = null
const LIST_CACHE_TTL_MS = 60_000

/**
 * Keyset-paginated session list with infinite-scroll wiring.
 *
 * The backend sorts sessions by creation time (session IDs are time-prefixed,
 * so ID order IS chronological order), which makes the last returned ID a
 * stable cursor: each page is fetched with ?limit=N&cursor=<lastId>.
 *
 * Usage: put containerRef on the scroll container, render the sentinel
 * element at the end of the list, and call reload() to reset to page one.
 */
export function useSessionList(
  pageSize = SESSION_PAGE_SIZE,
  onAuthError?: AuthReporter,
): UseSessionListResult {
  const [sessions, setSessions] = useState<SessionSummaryItem[]>([])
  const [total, setTotal] = useState(0)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [loadMoreError, setLoadMoreError] = useState<string | null>(null)
  const [nonce, setNonce] = useState(0)

  // Generation guard: a reload invalidates any in-flight loadMore so stale
  // pages can never be appended onto the fresh first page.
  const genRef = useRef(0)
  const cursorRef = useRef('')
  const busyRef = useRef(false)
  const containerRef = useRef<HTMLDivElement>(null)
  const sentinelRef = useRef<HTMLDivElement>(null)

  const reload = useCallback(() => {
    listCache = null
    setNonce((n) => n + 1)
  }, [])

  // First page (and reload). Replaces the whole list. Serves from the shared
  // module cache when fresh (see SessionListCache above).
  useEffect(() => {
    const gen = ++genRef.current
    let cancelled = false
    busyRef.current = true
    setLoading(true)
    setError(null)
    setLoadMoreError(null)

    const cached = listCache
    if (cached && Date.now() - cached.ts < LIST_CACHE_TTL_MS) {
      setSessions(cached.sessions)
      setTotal(cached.total)
      setHasMore(!!cached.nextCursor)
      cursorRef.current = cached.nextCursor
      setLoading(false)
      busyRef.current = false
      return
    }

    api
      .listSessions({ limit: pageSize })
      .then((page) => {
        if (cancelled || gen !== genRef.current) return
        setSessions(page.sessions)
        setTotal(page.total)
        setHasMore(!!page.next_cursor)
        cursorRef.current = page.next_cursor ?? ''
        listCache = {
          sessions: page.sessions,
          total: page.total,
          nextCursor: page.next_cursor ?? '',
          ts: Date.now(),
        }
      })
      .catch((err: unknown) => {
        if (cancelled || gen !== genRef.current) return
        if (err instanceof Error && err.name === 'AuthError') {
          onAuthError?.(err)
          return
        }
        setError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false)
          busyRef.current = false
        }
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pageSize, nonce])

  const loadMore = useCallback(() => {
    if (busyRef.current || !cursorRef.current) return
    const gen = genRef.current
    const cursor = cursorRef.current
    busyRef.current = true
    setLoadingMore(true)
    setLoadMoreError(null)
    api
      .listSessions({ limit: pageSize, cursor })
      .then((page) => {
        // Stale responses (reload happened meanwhile) are dropped.
        if (gen !== genRef.current || cursor !== cursorRef.current) return
        setSessions((prev) => {
          // Keyset pagination is gap-free by construction; the dedupe is a
          // cheap belt-and-suspenders for the reload race above.
          const seen = new Set(prev.map((s) => s.id))
          return [...prev, ...page.sessions.filter((s) => !seen.has(s.id))]
        })
        setTotal(page.total)
        setHasMore(!!page.next_cursor)
        cursorRef.current = page.next_cursor ?? ''
      })
      .catch((err: unknown) => {
        if (gen !== genRef.current) return
        if (err instanceof Error && err.name === 'AuthError') {
          onAuthError?.(err)
          return
        }
        setLoadMoreError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        setLoadingMore(false)
        busyRef.current = false
      })
  }, [pageSize, onAuthError])

  // IntersectionObserver: when the sentinel enters the scroll container's
  // viewport (with a 300px look-ahead), fetch the next page. The sentinel is
  // rendered unconditionally, so this effect runs once the first page lands.
  useEffect(() => {
    const container = containerRef.current
    const sentinel = sentinelRef.current
    if (!container || !sentinel || loading || error) return
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) loadMore()
      },
      { root: container, rootMargin: '300px 0px' },
    )
    obs.observe(sentinel)
    return () => obs.disconnect()
  }, [loadMore, loading, error])

  return {
    sessions,
    total,
    loading,
    loadingMore,
    error,
    loadMoreError,
    hasMore,
    reload,
    loadMore,
    containerRef,
    sentinelRef,
  }
}
