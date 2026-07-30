import React, { createContext, useContext, useEffect, useState } from 'react'
import { api } from './api'
import { OIDC_ENABLED, completeSignin, currentAccessToken, oidcManager, oidcRoles, oidcSignin, oidcSignout } from './oidc'

interface AuthState {
  user: string | null
  role: string
  login: (user: string, role: string) => Promise<void>
  logout: () => void
}

const AuthCtx = createContext<AuthState>(null as any)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [user, setUser] = useState<string | null>(
    OIDC_ENABLED ? null : localStorage.getItem('meridian.user'))
  const [role, setRole] = useState<string>(
    OIDC_ENABLED ? 'operator' : localStorage.getItem('meridian.role') || 'operator')

  // Prod mode (VITE_AUTH_MODE=keycloak): finish the PKCE redirect if we are
  // returning from Keycloak, otherwise pick up an in-memory session.
  useEffect(() => {
    if (!OIDC_ENABLED) return
    let cancelled = false
    ;(async () => {
      try {
        const u = (await completeSignin()) ?? (await oidcManager().getUser())
        if (!cancelled && u && !u.expired) {
          setUser(u.profile.preferred_username || u.profile.sub || 'oidc-user')
          setRole(oidcRoles(u)[0] || 'operator')
        }
      } catch {
        /* unauthenticated — stay on the login page */
      }
    })()
    return () => { cancelled = true }
  }, [])

  const login = async (u: string, r: string) => {
    if (OIDC_ENABLED) {
      await oidcSignin() // redirects to Keycloak (authorization code + PKCE)
      return
    }
    // Dev mode: mint a real dev HS256 JWT via the ETR service dev-token
    // endpoint; fall back to X-Dev-Role header auth when services are offline.
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
    if (OIDC_ENABLED) {
      void oidcSignout()
      setUser(null)
      return
    }
    localStorage.removeItem('meridian.token')
    localStorage.removeItem('meridian.user')
    setUser(null)
  }

  return <AuthCtx.Provider value={{ user, role, login, logout }}>{children}</AuthCtx.Provider>
}

export const useAuth = () => useContext(AuthCtx)
export { OIDC_ENABLED, currentAccessToken }
