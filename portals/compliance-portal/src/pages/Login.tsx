import React, { useState } from 'react'
import { useAuth, OIDC_ENABLED } from '../auth'

export default function Login() {
  const { login } = useAuth()
  const [user, setUser] = useState('')
  const [role, setRole] = useState('operator')
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!user.trim()) return
    setBusy(true)
    try {
      await login(user.trim(), role)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-sand-100">
      <div className="card w-full max-w-sm p-8">
        <div className="mb-6">
          <div className="text-2xl font-semibold tracking-tight text-sand-900">Meridian</div>
          <div className="text-sm text-sand-500 mt-1">
            Compliance Portal · {OIDC_ENABLED ? 'Keycloak SSO' : 'dev sign-in'}
          </div>
        </div>
        {OIDC_ENABLED ? (
          <div className="space-y-4">
            <button className="btn w-full justify-center" disabled={busy}
              onClick={async () => { setBusy(true); try { await login('', '') } finally { setBusy(false) } }}>
              {busy ? 'Redirecting…' : 'Sign in with Keycloak'}
            </button>
            <p className="text-xs text-sand-400 mt-5">
              Prod mode (VITE_AUTH_MODE=keycloak): authorization code + PKCE; tokens
              are kept in memory and silently renewed.
            </p>
          </div>
        ) : (
        <form onSubmit={submit} className="space-y-4">
          <div>
            <label className="label">User</label>
            <input className="input" value={user} onChange={(e) => setUser(e.target.value)}
              placeholder="e.g. amina.bello" autoFocus />
          </div>
          <div>
            <label className="label">Role</label>
            <select className="input" value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="operator">operator</option>
              <option value="admin">admin</option>
              <option value="auditor">auditor</option>
            </select>
          </div>
          <button className="btn w-full justify-center" disabled={busy}>
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </form>
        )}
        {!OIDC_ENABLED && (
        <p className="text-xs text-sand-400 mt-5">
          Dev mode: a short-lived HS256 JWT is minted locally (MERIDIAN_DEV_JWT_SECRET);
          services also accept the X-Dev-Role header when AUTH_MODE=dev.
        </p>
        )}
      </div>
    </div>
  )
}
