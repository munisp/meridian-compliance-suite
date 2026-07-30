import React from 'react'
import { NavLink, Outlet } from 'react-router-dom'
import { useAuth } from '../auth'

const NAV = [
  { to: '/', label: 'Overview', end: true },
  { to: '/einvoicing', label: 'E-Invoicing' },
  { to: '/wht', label: 'WHT' },
  { to: '/etr', label: 'ETR (Pillar Two)' },
  { to: '/vasp', label: 'VASP / CARF' },
  { to: '/pos', label: 'Retailer POS' },
  { to: '/cases', label: 'Practitioner' },
]

export default function Layout() {
  const { user, role, logout } = useAuth()
  return (
    <div className="min-h-screen flex">
      <aside className="w-60 shrink-0 bg-sand-900 text-sand-100 flex flex-col">
        <div className="px-5 py-5 border-b border-sand-800">
          <div className="text-lg font-semibold tracking-tight">Meridian</div>
          <div className="text-xs text-sand-300 mt-0.5">Compliance Portal</div>
        </div>
        <nav className="flex-1 px-3 py-4 space-y-1">
          {NAV.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end as any}
              className={({ isActive }) =>
                `block rounded-lg px-3 py-2 text-sm transition-colors ${
                  isActive ? 'bg-clay-600 text-white' : 'text-sand-200 hover:bg-sand-800'
                }`
              }
            >
              {n.label}
            </NavLink>
          ))}
        </nav>
        <div className="px-5 py-4 border-t border-sand-800 text-xs text-sand-400">
          Market Zone · NRS Unified Platform
        </div>
      </aside>
      <div className="flex-1 flex flex-col min-w-0">
        <header className="h-14 bg-white border-b border-sand-200 flex items-center justify-between px-6">
          <div className="text-sm text-sand-600">
            Nigeria Revenue Service · TaxTech compliance plane
          </div>
          <div className="flex items-center gap-3 text-sm">
            <span className="badge bg-moss-500/15 text-moss-700">{role}</span>
            <span className="text-sand-700">{user}</span>
            <button onClick={logout} className="btn-ghost !py-1 !px-2 text-xs">Sign out</button>
          </div>
        </header>
        <main className="flex-1 p-6 overflow-auto">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
