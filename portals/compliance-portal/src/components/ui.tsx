import React from 'react'
import Chip, { chipStatusFor } from './Chip'
import Empty from './Empty'

export { Empty }
export { default as Chip } from './Chip'
export { default as Field } from './Field'
export { default as MoneyInput } from './MoneyInput'
export { SkeletonRows } from './Empty'

export function PageTitle({ title, sub }: { title: string; sub?: string }) {
  return (
    <div className="mb-5">
      <h1 className="text-xl font-semibold tracking-tight text-stone-900">{title}</h1>
      {sub && <p className="text-sm text-stone-600 mt-1">{sub}</p>}
    </div>
  )
}

export function Card({ title, children, actions }: {
  title?: string; children: React.ReactNode; actions?: React.ReactNode
}) {
  return (
    <div className="card p-5">
      {(title || actions) && (
        <div className="flex items-center justify-between mb-4">
          {title && <h2 className="text-sm font-semibold text-stone-800">{title}</h2>}
          {actions}
        </div>
      )}
      {children}
    </div>
  )
}

/** Status is always a chip (spec §5), never coloured text alone. */
export function Status({ value }: { value: string }) {
  const v = value || 'unknown'
  const s = v.toLowerCase()
  const status =
    /ok|pass|ingested|transmitted|completed|active|open(?! source)|met|published|settled|verified|checking/i.test(s)
      ? chipStatusFor(s.includes('checking') ? 'pending' : 'verified')
      : /fail|missed|refused|error|escalated|offline|closed/i.test(s)
        ? chipStatusFor('failed')
        : chipStatusFor(s)
  return <Chip status={status}>{v}</Chip>
}

/** Page/inline fetch failures — role="alert" so screen readers announce them. */
export function ErrorNote({ error }: { error: any }) {
  if (!error) return null
  return (
    <div role="alert"
      className="rounded-lg border border-danger-strong/40 bg-danger px-3 py-2 text-sm text-danger-on flex items-start gap-2">
      <svg aria-hidden="true" viewBox="0 0 24 24" className="h-4 w-4 mt-0.5 shrink-0" fill="none" stroke="currentColor" strokeWidth="2">
        <circle cx="12" cy="12" r="10" />
        <path d="M12 8v4M12 16h.01" />
      </svg>
      <span>{error.friendlyMessage || String(error)}</span>
    </div>
  )
}
