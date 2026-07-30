// OIDC (Keycloak) login for the compliance portal — HARDENING.md H2.
// Active only when VITE_AUTH_MODE=keycloak; dev-token login stays the
// default. Authorization Code + PKCE via oidc-client-ts; tokens are held in
// memory (InMemoryWebStorage — never localStorage) and silently renewed.
import { InMemoryWebStorage, User, UserManager, WebStorageStateStore } from 'oidc-client-ts'

const env = (k: string, def = ''): string => (import.meta as any).env?.[k] ?? def

export const OIDC_ENABLED = env('VITE_AUTH_MODE') === 'keycloak'

let mgr: UserManager | null = null

export function oidcManager(): UserManager {
  if (!mgr) {
    mgr = new UserManager({
      authority: env('VITE_KEYCLOAK_ISSUER', 'https://keycloak:8443/realms/meridian'),
      client_id: env('VITE_KEYCLOAK_CLIENT_ID', 'compliance-portal'),
      redirect_uri: `${window.location.origin}/auth/callback`,
      post_logout_redirect_uri: `${window.location.origin}/`,
      response_type: 'code', // authorization code + PKCE (S256 default)
      scope: env('VITE_KEYCLOAK_SCOPE', 'openid profile email'),
      userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
      automaticSilentRenew: true,
      accessTokenExpiringNotificationTimeInSeconds: 60,
    })
  }
  return mgr
}

// currentAccessToken returns a fresh (silent-renewed) access token, or null.
export async function currentAccessToken(): Promise<string | null> {
  const u: User | null = await oidcManager().getUser()
  if (!u || u.expired) return null
  return u.access_token
}

// completeSignin handles the /auth/callback redirect from Keycloak.
export async function completeSignin(): Promise<User | null> {
  if (!window.location.search.includes('code=')) return null
  const u = await oidcManager().signinRedirectCallback(window.location.href)
  window.history.replaceState({}, document.title, '/')
  return u
}

export function oidcRoles(u: User | null): string[] {
  const p = (u?.profile ?? {}) as any
  const realm: string[] = p.realm_access?.roles ?? []
  return realm
}

export async function oidcSignin(): Promise<void> {
  await oidcManager().signinRedirect()
}

export async function oidcSignout(): Promise<void> {
  await oidcManager().removeUser()
}
