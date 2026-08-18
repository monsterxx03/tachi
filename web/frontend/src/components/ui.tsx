import type { ReactNode } from 'react'

/** Card: white panel with border + light shadow (report.html style). */
export function Card({
  className = '',
  children,
}: {
  className?: string
  children: ReactNode
}) {
  return <div className={`card ${className}`}>{children}</div>
}

/** Card header row: title on the left, optional right slot. */
export function CardHead({
  title,
  right,
}: {
  title: ReactNode
  right?: ReactNode
}) {
  return (
    <div className="flex items-center justify-between mb-3">
      <h3 className="text-[13px] font-semibold text-ink">{title}</h3>
      {right}
    </div>
  )
}

/** Statistic card used on the overview page. */
export function StatCard({
  label,
  value,
  sub,
  valueClass = '',
}: {
  label: string
  value: ReactNode
  sub?: ReactNode
  valueClass?: string
}) {
  return (
    <Card className="px-4 py-4">
      <div className="text-xs text-inkdim mb-1">{label}</div>
      <div className={`text-3xl font-bold tracking-tight mb-0.5 ${valueClass}`}>
        {value}
      </div>
      {sub && <div className="text-[11px] text-muted">{sub}</div>}
    </Card>
  )
}

/** Small colored pill with mono text. */
export function Badge({
  color = 'gray',
  children,
}: {
  color?: 'green' | 'blue' | 'yellow' | 'purple' | 'gray' | 'gold'
  children: ReactNode
}) {
  const map = {
    green: 'badge text-accent bg-accent-soft border border-line',
    blue: 'badge text-linkblue bg-linksoft border border-line',
    yellow: 'badge text-warn bg-warnsoft border border-line',
    purple: 'badge text-violet bg-violetsft border border-line',
    gray: 'badge text-inkdim bg-paper2 border border-line',
    gold: 'badge text-gold bg-warnsoft border border-line',
  }
  return <span className={map[color]}>{children}</span>
}

/** Section label used inside payload/event blocks. */
export function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="text-[10px] uppercase tracking-wider text-muted mb-1.5">
      {children}
    </div>
  )
}

/** Page header (title + subtitle). */
export function PageHead({ title, sub }: { title: string; sub?: ReactNode }) {
  return (
    <div className="mb-4">
      <h1 className="text-xl font-semibold">{title}</h1>
      {sub && <div className="text-[13px] text-inkdim mt-0.5">{sub}</div>}
    </div>
  )
}

/** Full-area centered loading state (spinner + optional text). */
export function Loading({ text = '加载中…' }: { text?: string }) {
  return (
    <div className="h-full flex items-center justify-center gap-2.5 text-sm text-inkdim">
      <span className="w-4 h-4 rounded-full border-2 border-line border-t-accent animate-spin" />
      {text}
    </div>
  )
}

/** Empty state (centered hint). */
export function Empty({
  icon,
  title,
  hint,
}: {
  icon?: string
  title: string
  hint?: string
}) {
  return (
    <div className="flex flex-col items-center pt-24 text-muted gap-2 text-[13px]">
      {icon && <div className="text-3xl">{icon}</div>}
      <div>{title}</div>
      {hint && <div className="text-xs text-muted">{hint}</div>}
    </div>
  )
}