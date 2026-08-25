import React, { useEffect, useState } from 'react'
import { api, errMsg, kobo } from '../api'
import { Card, Empty, ErrorNote, Field, PageTitle, Status } from '../components/ui'

// Merchant self-service (I5): API key lifecycle + VAT dashboard summary.
// Load failures surface via errMsg/ErrorNote — never a silent catch.
interface ApiKeyMeta {
  id: string; name: string; prefix: string; active: boolean
  created_at: string; rotated_at?: string | null; revoked_at?: string | null; last_used_at?: string | null
}
interface VatPeriod { period: string; invoice_count: number; tax_exclusive_kobo: number; tax_kobo: number; payable_kobo: number }
interface VatSummary {
  tenant_id: string; periods: VatPeriod[]; by_status: Record<string, number>
  total_invoices: number; total_tax_kobo: number; total_payable_kobo: number
}

export default function Merchant() {
  const [summary, setSummary] = useState<VatSummary | null>(null)
  const [keys, setKeys] = useState<ApiKeyMeta[]>([])
  const [loadErr, setLoadErr] = useState('')
  const [error, setError] = useState<any>(null)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  // Plaintext secret shown exactly once after create/rotate, never re-fetched.
  const [secretOnce, setSecretOnce] = useState<{ id: string; key: string } | null>(null)

  const load = () => {
    setLoadErr('')
    const errs: string[] = []
    api('einvoicing').get('/v1/vat/summary').then((r) => setSummary(r.data))
      .catch((e) => { errs.push(errMsg(e)); setLoadErr(errs.join(' · ')) })
    api('einvoicing').get('/v1/apikeys').then((r) => setKeys(r.data.keys || []))
      .catch((e) => { errs.push(errMsg(e)); setLoadErr(errs.join(' · ')) })
  }
  useEffect(load, [])

  const createKey = async (e: React.FormEvent) => {
    e.preventDefault(); setBusy(true); setError(null); setSecretOnce(null)
    try {
      const res = await api('einvoicing').post('/v1/apikeys', { name: name.trim() })
      setSecretOnce({ id: res.data.id, key: res.data.api_key })
      setName('')
      load()
    } catch (err) { setError(err) } finally { setBusy(false) }
  }

  const rotate = async (id: string) => {
    setError(null); setSecretOnce(null)
    try {
      const res = await api('einvoicing').post(`/v1/apikeys/${id}/rotate`)
      setSecretOnce({ id, key: res.data.api_key })
      load()
    } catch (err) { setError(err) }
  }

  const revoke = async (id: string) => {
    setError(null)
    try {
      await api('einvoicing').post(`/v1/apikeys/${id}/revoke`)
      load()
    } catch (err) { setError(err) }
  }

  return (
    <div className="space-y-5">
      <PageTitle title="Merchant Self-Service" sub="API key management (hashed, rotatable) · VAT summary from your e-invoices (I5)" />

      {loadErr && <ErrorNote error={loadErr} />}

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-5">
        <Card title="VAT collected (all periods)">
          {summary ? (
            <div className="space-y-2 text-sm">
              <Row k="Invoices" v={summary.total_invoices} />
              <Row k="VAT total" v={kobo(summary.total_tax_kobo)} />
              <Row k="Payable total" v={kobo(summary.total_payable_kobo)} />
            </div>
          ) : <Empty>Loading VAT summary…</Empty>}
        </Card>
        <Card title="Invoices by status">
          {summary && Object.keys(summary.by_status).length > 0 ? (
            <div className="flex flex-wrap gap-2">
              {Object.entries(summary.by_status).map(([st, n]) => (
                <span key={st} className="inline-flex items-center gap-1.5 text-sm">
                  <Status value={st} /> <span className="text-stone-600">×{n}</span>
                </span>
              ))}
            </div>
          ) : <Empty>No invoices yet.</Empty>}
        </Card>
        <Card title="VAT by period">
          {summary && summary.periods.length > 0 ? (
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-stone-600">
                  <th className="py-1">Period</th><th className="py-1 text-right">Invoices</th><th className="py-1 text-right">VAT</th>
                </tr>
              </thead>
              <tbody>
                {summary.periods.map((p) => (
                  <tr key={p.period} className="border-t border-neutral-100">
                    <td className="py-1 font-mono text-xs">{p.period}</td>
                    <td className="py-1 text-right">{p.invoice_count}</td>
                    <td className="py-1 text-right">{kobo(p.tax_kobo)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : <Empty>No invoiced periods yet.</Empty>}
        </Card>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <Card title="Create API key">
          <form onSubmit={createKey} className="space-y-3">
            <Field label="Key name" required hint="e.g. erp-sync — the secret is shown once, only the hash is stored">
              {(id, describedBy) => (
                <input id={id} className="input" value={name} required maxLength={120}
                  onChange={(e) => setName(e.target.value)} aria-describedby={describedBy} />)}
            </Field>
            <button className="btn" disabled={busy || !name.trim()}>{busy ? 'Creating…' : 'Create key'}</button>
          </form>
          {error && <div className="mt-3"><ErrorNote error={error} /></div>}
          {secretOnce && (
            <div role="status" className="mt-3 rounded-lg border border-amber-300 bg-amber-50 px-3 py-2 text-sm">
              <div className="font-semibold">Copy this key now — it will not be shown again.</div>
              <code className="block mt-1 break-all font-mono text-xs">{secretOnce.key}</code>
            </div>
          )}
        </Card>
        <Card title={`API keys (${keys.length})`}>
          {keys.length > 0 ? (
            <ul className="divide-y divide-neutral-100 text-sm">
              {keys.map((k) => (
                <li key={k.id} className="py-2 flex items-center justify-between gap-3">
                  <div className="min-w-0">
                    <div className="font-medium truncate">{k.name}</div>
                    <div className="text-xs text-stone-600 font-mono">{k.prefix}… · {new Date(k.created_at).toLocaleDateString()}</div>
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    <Status value={k.active ? 'active' : 'revoked'} />
                    {k.active && (
                      <>
                        <button className="btn-ghost" onClick={() => rotate(k.id)}>Rotate</button>
                        <button className="btn-ghost text-danger-on" onClick={() => revoke(k.id)}>Revoke</button>
                      </>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          ) : <Empty>No API keys yet — create one for machine integrations.</Empty>}
        </Card>
      </div>
    </div>
  )
}

function Row({ k, v }: { k: string; v: any }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-stone-600 shrink-0">{k}</span>
      <span className="text-right break-all">{v ?? '—'}</span>
    </div>
  )
}
