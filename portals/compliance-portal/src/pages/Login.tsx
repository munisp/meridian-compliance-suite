import React, { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useAuth, OIDC_ENABLED } from '../auth'
import Field from '../components/Field'
import LangSwitcher from '../components/LangSwitcher'

export default function Login() {
  const { login } = useAuth()
  const { t } = useTranslation('common')
  const [user, setUser] = useState('')
  const [role, setRole] = useState('operator')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!user.trim()) {
      setError('Enter a username')
      return
    }
    setError(null)
    setBusy(true)
    try {
      await login(user.trim(), role)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-neutral-100">
      <div className="card w-full max-w-sm p-8">
        <div className="mb-6 flex items-start justify-between gap-3">
          <div>
            <div className="text-2xl font-semibold tracking-tight text-brand-900">{t('app.title')}</div>
            <div className="text-sm text-stone-600 mt-1">
              {t('app.subtitle')} · {OIDC_ENABLED ? 'Keycloak SSO' : 'dev sign-in'}
            </div>
          </div>
          <LangSwitcher />
        </div>
        {OIDC_ENABLED ? (
          <div className="space-y-4">
            <button className="btn w-full justify-center min-h-[44px]" disabled={busy}
              onClick={async () => { setBusy(true); try { await login('', '') } finally { setBusy(false) } }}>
              {busy ? 'Redirecting…' : 'Sign in with Keycloak'}
            </button>
            <p className="text-xs text-stone-600 mt-5">
              Prod mode (VITE_AUTH_MODE=keycloak): authorization code + PKCE; tokens
              are kept in memory and silently renewed.
            </p>
          </div>
        ) : (
        <form onSubmit={submit} className="space-y-4">
          <Field label="User" required error={error}>
            {(id, describedBy, invalid) => (
              <input id={id} className="input" value={user} onChange={(e) => setUser(e.target.value)}
                placeholder="e.g. amina.bello" autoFocus autoComplete="username"
                aria-describedby={describedBy} aria-invalid={invalid || undefined} aria-required="true" />
            )}
          </Field>
          <Field label="Role">
            {(id) => (
              <select id={id} className="input" value={role} onChange={(e) => setRole(e.target.value)}>
                <option value="operator">operator</option>
                <option value="admin">admin</option>
                <option value="auditor">auditor</option>
              </select>
            )}
          </Field>
          <button className="btn w-full justify-center min-h-[44px]" disabled={busy}>
            {busy ? 'Signing in…' : t('auth.signIn')}
          </button>
        </form>
        )}
        {!OIDC_ENABLED && (
        <p className="text-xs text-stone-600 mt-5">
          Dev mode: a short-lived HS256 JWT is minted locally (MERIDIAN_DEV_JWT_SECRET);
          services also accept the X-Dev-Role header when AUTH_MODE=dev.
        </p>
        )}
      </div>
    </div>
  )
}
