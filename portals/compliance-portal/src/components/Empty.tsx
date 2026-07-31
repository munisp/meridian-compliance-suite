import React from 'react'
import { Inbox, LucideIcon } from 'lucide-react'

interface EmptyProps {
  children?: React.ReactNode
  title?: string
  body?: string
  action?: React.ReactNode
  icon?: LucideIcon
}

/** Meridian One §5 — illustration-free empty state (bytes matter).
 *  Accepts either <Empty>guidance text</Empty> or title/body/action props. */
export default function Empty({ children, title, body, action, icon: Icon = Inbox }: EmptyProps) {
  return (
    <div className="flex flex-col items-center text-center py-6 px-4">
      <span className="rounded-full bg-neutral-100 p-3 mb-3">
        <Icon aria-hidden="true" className="h-6 w-6 text-neutral-500" />
      </span>
      {title ? <p className="text-base font-semibold text-stone-800">{title}</p> : null}
      <p className="text-sm text-stone-600 mt-1 max-w-sm">{body ?? children}</p>
      {action && <div className="mt-4">{action}</div>}
    </div>
  )
}

/** Skeleton rows matching table layout while data loads (spec §5 loading). */
export function SkeletonRows({ rows = 3, cols = 3 }: { rows?: number; cols?: number }) {
  return (
    <div className="space-y-2" aria-busy="true" aria-label="Loading">
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex gap-3 animate-pulse">
          {Array.from({ length: cols }).map((_, c) => (
            <div key={c} className="h-8 flex-1 rounded-md bg-neutral-100" />
          ))}
        </div>
      ))}
    </div>
  )
}
