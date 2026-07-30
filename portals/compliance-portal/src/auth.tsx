import React, { createContext, useContext, useState } from 'react'
import { api } from './api'

interface AuthState {
  user: string | null
  role: string
  login: (user: string, role: string) => Promise<void>
  logout: () => void
}

const AuthCtx = createContext<AuthState>(null as any)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [user, setUser] = useState<string | null>(localStorage.getItem('meridian.user'))
  const [role, setRole] = useState<string>(localStorage.getItem('meridian.role') || 'operator')

  const login = async (u: string, r: string) => {
    // Mint a real dev HS256 JWT via the ETR service dev-token endpoint;
    // fall back to X-Dev-Role header auth when services are offline.
    try {
      const res = await api('etr').post('/v1/dev-token', { sub: u, roles: [r] })
      localStorage.setItem('meridian.token', res.data.token)
    } catch {
      localStorage.removeItem('meridian.token')
    }
    localStorage.setItem('meridian.user', u)
    localStorage.setItem('meridian.role', r)
    setUser(u)
    setRole(r)
  }

  const logout = () => {
    localStorage.removeItem('meridian.token')
    localStorage.removeItem('meridian.user')
    setUser(null)
  }

  return <AuthCtx.Provider value={{ user, role, login, logout }}>{children}</AuthCtx.Provider>
}

export const useAuth = () => useContext(AuthCtx)
