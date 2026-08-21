// Formatting helpers shared across views.

/** ¥ rounded to cents (2 decimals). */
export function yuan(v: number | undefined): string {
  if (v === undefined || Number.isNaN(v)) return '¥0.00'
  return '¥' + v.toFixed(2)
}

/** "12.3k" / "1.2M" style token counts. */
export function compact(n: number | undefined): string {
  if (n === undefined || Number.isNaN(n)) return '-'
  if (Math.abs(n) >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M'
  if (Math.abs(n) >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
}

/** Full number with thousands separators. */
export function num(n: number | undefined): string {
  if (n === undefined || Number.isNaN(n)) return '-'
  return n.toLocaleString('en-US')
}

/** Credit rounded to 2 decimals (display only; underlying values keep full precision). */
export function credit(v: number | undefined): string {
  if (v === undefined || Number.isNaN(v)) return '0.00'
  return v.toFixed(2)
}

/** "08-17 21:06" from an ISO timestamp. */
export function shortTime(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (x: number) => String(x).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** "2026-08-17 21:06" fuller form. */
export function fullTime(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** Human-friendly duration from ms. */
export function durMs(ms: number | undefined): string {
  if (ms === undefined || Number.isNaN(ms)) return ''
  if (ms < 1000) return `${ms}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${Math.round(s % 60)}s`
}

let prevNow = Date.now()
/** Ensure a strictly increasing second resolution for stable React keys. */
export function nextKey(): number {
  const now = Date.now()
  if (now <= prevNow) prevNow += 1
  else prevNow = now
  return prevNow
}