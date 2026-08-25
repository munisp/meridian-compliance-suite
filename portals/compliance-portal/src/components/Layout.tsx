import React from 'react'
import { NavLink, Outlet } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import {
  LayoutDashboard, FileText, Percent, Globe2, Bitcoin, ReceiptText, Briefcase, KeyRound, LogOut, LucideIcon,
} from 'lucide-react'
import { useAuth } from '../auth'
import LangSwitcher from './LangSwitcher'

const NAV: { to: string; key: string; end?: boolean; icon: LucideIcon }[] = [
  { to: '/', key: 'overview', end: true, icon: LayoutDashboard },
  { to: '/einvoicing', key: 'einvoicing', icon: FileText },
  { to: '/wht', key: 'wht', icon: Percent },
  { to: '/etr', key: 'etr', icon: Globe2 },
  { to: '/vasp', key: 'vasp', icon: Bitcoin },
  { to: '/pos', key: 'pos', icon: ReceiptText },
  { to: '/cases', key: 'cases', icon: Briefcase },
  { to: '/merchant', key: 'merchant', icon: KeyRound },
]

export default function Layout() {
  const { user, role, logout } = useAuth()
  const { t } = useTranslation('common')
  return (
    <div className="min-h-screen flex">
      <a href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:bg-brand-700 focus:text-white focus:px-3 focus:py-2 focus:rounded-md">
        Skip to content
      </a>
      <aside className="w-60 shrink-0 bg-brand-800 text-brand-100 flex flex-col">
        <div className="px-5 py-5 border-b border-brand-700">
          <div className="text-lg font-semibold tracking-tight text-white">{t('app.title')}</div>
          <div className="text-xs text-brand-200 mt-0.5">{t('app.subtitle')}</div>
        </div>
        <nav className="flex-1 px-3 py-4 space-y-1" aria-label="Primary">
          {NAV.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end as any}
              className={({ isActive }) =>
                `flex items-center gap-2 rounded-lg px-3 py-2 min-h-[44px] text-sm transition-colors
                 focus-visible:ring-2 focus-visible:ring-brand-400 focus-visible:outline-none ${
                  isActive ? 'bg-brand-700 text-white font-semibold' : 'text-brand-100 hover:bg-brand-700/60'
                }`
              }
            >
              <n.icon aria-hidden="true" className="h-4 w-4 shrink-0" />
              {t(`nav.${n.key}`)}
            </NavLink>
          ))}
        </nav>
        <div className="px-5 py-4 border-t border-brand-700 text-xs text-brand-200">
          Market Zone · NRS Unified Platform
        </div>
      </aside>
      <div className="flex-1 flex flex-col min-w-0">
        <header className="h-14 bg-white border-b border-neutral-200 flex items-center justify-between px-6 shadow-sm">
          <div className="text-sm text-stone-600">{t('app.tagline')}</div>
          <div className="flex items-center gap-3 text-sm">
            <LangSwitcher />
            <span className="chip-info">{role}</span>
            <span className="text-stone-800">{user}</span>
            <button onClick={logout} className="btn-ghost !py-1 !px-2 text-xs min-h-[36px]">
              <LogOut aria-hidden="true" className="h-3.5 w-3.5" />
              {t('auth.signOut')}
            </button>
          </div>
        </header>
        <main id="main" className="flex-1 p-6 overflow-auto">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
