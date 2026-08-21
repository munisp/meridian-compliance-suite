import axios, { AxiosInstance } from 'axios'
import { OIDC_ENABLED, currentAccessToken } from './oidc'

// Env-configurable API base URLs (SPEC: planes consume core contracts).
const env = (k: string, def: string) =>
  (import.meta as any).env?.[k] || def

export const BASES = {
  einvoicing: env('VITE_EINVOICING_URL', 'http://localhost:8110'),
  wht: env('VITE_WHT_URL', 'http://localhost:8130'),
  etr: env('VITE_ETR_URL', 'http://localhost:8109'),
  vasp: env('VITE_VASP_CARF_URL', 'http://localhost:8116'),
  pos: env('VITE_POS_VAT_URL', 'http://localhost:8106'),
  cases: env('VITE_CASE_MGMT_URL', 'http://localhost:8113'),
} as const

export type ServiceKey = keyof typeof BASES

const clients: Partial<Record<ServiceKey, AxiosInstance>> = {}

export function api(key: ServiceKey): AxiosInstance {
  if (!clients[key]) {
    clients[key] = axios.create({ baseURL: BASES[key], timeout: 15000 })
    clients[key]!.interceptors.request.use(async (cfg) => {
      if (OIDC_ENABLED) {
        // Prod: Bearer token from the in-memory OIDC session (silent renew).
        const token = await currentAccessToken()
        if (token) cfg.headers.Authorization = `Bearer ${token}`
        return cfg
      }
      const token = localStorage.getItem('meridian.token')
      const role = localStorage.getItem('meridian.role') || 'operator'
      if (token) cfg.headers.Authorization = `Bearer ${token}`
      cfg.headers['X-Dev-Role'] = role // dev-mode fallback per SPEC §1.3
      return cfg
    })
    clients[key]!.interceptors.response.use(
      (r) => r,
      (err) => {
        const p = err.response?.data
        err.friendlyMessage = p?.detail || p?.title || err.message
        return Promise.reject(err)
      },
    )
  }
  return clients[key]!
}

// errMsg extracts a human-readable message from a failed api call
// (RFC7807 detail -> axios message -> fallback), mirroring the core admin
// F-13 pattern: load failures must surface, not masquerade as empty states.
export function errMsg(e: any): string {
  return e?.friendlyMessage || e?.message || String(e)
}

// Money is kobo integers end-to-end; formatNGN (lib/format) is the single
// sanctioned formatter (Meridian One §9). `kobo` stays as a null-safe alias.
import { formatNGN } from './lib/format'
export { formatNGN, formatNGNCompact, parseNGNToKobo, formatDateNG } from './lib/format'

export const kobo = (k: number | undefined | null) =>
  k == null ? '—' : formatNGN(k)

export const bps = (b: number | undefined | null) =>
  b == null ? '—' : `${(b / 100).toFixed(2)}%`
