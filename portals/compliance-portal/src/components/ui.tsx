import React from 'react'

export function PageTitle({ title, sub }: { title: string; sub?: string }) {
  return (
    <div className="mb-5">
      <h1 className="text-xl font-semibold tracking-tight text-sand-900">{title}</h1>
      {sub && <p className="text-sm text-sand-500 mt-1">{sub}</p>}
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
          {title && <h2 className="text-sm font-semibold text-sand-800">{title}</h2>}
          {actions}
        </div>
      )}
      {children}
    </div>
  )
}

export function Status({ value }: { value: string }) {
  const tone =
    /ok|pass|ingested|transmitted|completed|active|open|met|published|settled/i.test(value)
      ? 'bg-moss-500/15 text-moss-700'
      : /fail|missed|refused|error|escalated/i.test(value)
        ? 'bg-red-100 text-red-700'
        : 'bg-sand-200 text-sand-700'
  return <span className={`badge ${tone}`}>{value}</span>
}

export function ErrorNote({ error }: { error: any }) {
  if (!error) return null
  return (
    <div className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
      {error.friendlyMessage || String(error)}
    </div>
  )
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <div className="text-sm text-sand-400 py-6 text-center">{children}</div>
}
