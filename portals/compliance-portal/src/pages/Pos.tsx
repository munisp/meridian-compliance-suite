import React, { useEffect, useState } from 'react'
import { api, kobo } from '../api'
import { Card, Empty, ErrorNote, PageTitle, Status } from '../components/ui'

export default function Pos() {
  const [receipts, setReceipts] = useState<any[]>([])
  const [variance, setVariance] = useState<any>(null)
  const [mode, setMode] = useState<any>(null)
  const [form, setForm] = useState({ merchant_tin: '12345678-0001', terminal: 'POS-01', lat: '6.5244', lon: '3.3792', amount: '25000', category: 'electronics', store_forward: false })
  const [error, setError] = useState<any>(null)
  const [tenant] = useState('demo-retailer')
  const [recon, setRecon] = useState<any>(null)

  const load = () => {
    api('pos').get(`/v1/receipts?tenant_id=${tenant}&limit=50`).then((r) => setReceipts(r.data.receipts || [])).catch(() => {})
    api('pos').get(`/v1/variance?tenant_id=${tenant}`).then((r) => setVariance(r.data)).catch(() => {})
    api('pos').get('/v1/attribution/mode').then((r) => setMode(r.data)).catch(() => {})
  }
  useEffect(load, [])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault(); setError(null)
    try {
      await api('pos').post('/v1/receipts', {
        tenant_id: tenant, merchant_tin: form.merchant_tin, terminal_id: form.terminal,
        receipt_no: `R-${Date.now()}`, lat: parseFloat(form.lat), lon: parseFloat(form.lon),
        store_and_forward: form.store_forward,
        lines: [{ sku: 'SKU-1', qty_milli: 1000, unit_price_kobo: Math.round(parseFloat(form.amount) * 100), category: form.category }],
      })
      load()
    } catch (err) { setError(err) }
  }

  const runRecon = async () => {
    setError(null)
    try {
      const period = new Date().toISOString().slice(0, 7)
      const res = await api('pos').post('/v1/settlement/recon', { tenant_id: tenant, period })
      setRecon(res.data)
    } catch (err) { setError(err) }
  }

  return (
    <div className="space-y-5">
      <PageTitle title="Retailer POS Dashboard" sub="Receipt ingest · capture-time state/LGA attribution · variance detection (T6)" />

      {mode && (
        <div className="rounded-xl border border-sand-200 bg-white px-4 py-3 text-sm text-sand-700">
          Attribution mode: <b>{mode.mode}</b> · federal {mode.federal_share_bps / 100}% / state {mode.state_share_bps / 100}% / LGA {mode.lga_share_bps / 100}%
          <span className="text-xs text-sand-400 ml-2">({mode.source} {mode.rule_pack_version?.split(',')[3] || ''})</span>
        </div>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-5">
        <Card title="Submit receipt">
          <form onSubmit={submit} className="space-y-3">
            <div><label className="label">Merchant TIN</label>
              <input className="input" value={form.merchant_tin} onChange={(e) => setForm({ ...form, merchant_tin: e.target.value })} /></div>
            <div className="grid grid-cols-2 gap-3">
              <div><label className="label">Lat</label>
                <input className="input" value={form.lat} onChange={(e) => setForm({ ...form, lat: e.target.value })} /></div>
              <div><label className="label">Lon</label>
                <input className="input" value={form.lon} onChange={(e) => setForm({ ...form, lon: e.target.value })} /></div>
            </div>
            <div><label className="label">Amount (NGN)</label>
              <input className="input" type="number" min="1" value={form.amount} onChange={(e) => setForm({ ...form, amount: e.target.value })} /></div>
            <div><label className="label">Category (basket)</label>
              <select className="input" value={form.category} onChange={(e) => setForm({ ...form, category: e.target.value })}>
                <option value="electronics">electronics (standard 7.5%)</option>
                <option value="basic_food">basic_food (zero-rated)</option>
                <option value="pharmacy">pharmacy (zero-rated)</option>
                <option value="financial_services">financial_services (exempt)</option>
              </select></div>
            <label className="flex items-center gap-2 text-sm text-sand-700">
              <input type="checkbox" checked={form.store_forward} onChange={(e) => setForm({ ...form, store_forward: e.target.checked })} />
              store-and-forward (offline spool)
            </label>
            <button className="btn">Submit</button>
          </form>
          {error && <div className="mt-3"><ErrorNote error={error} /></div>}
        </Card>

        <Card title={`Receipts (${receipts.length})`}>
          {receipts.length === 0 ? <Empty>No receipts yet.</Empty> : (
            <div className="max-h-96 overflow-auto">
              <table className="w-full">
                <thead><tr><th className="th">Receipt</th><th className="th">Total</th><th className="th">VAT</th><th className="th">State/LGA</th></tr></thead>
                <tbody>
                  {receipts.map((r: any) => (
                    <tr key={r.id}>
                      <td className="td font-mono text-xs">{r.receipt_no}</td>
                      <td className="td">{kobo(r.total_kobo)}</td>
                      <td className="td">{kobo(r.vat_kobo)}</td>
                      <td className="td text-xs">{r.state}<br /><span className="text-sand-400">{r.lga}</span></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <div className="space-y-5">
          <Card title="Variance detection">
            {variance ? (
              <div className="text-sm">
                <div className="mb-2">{variance.variance_count} variance(s) detected</div>
                {(variance.variances || []).slice(0, 5).map((v: any, i: number) => (
                  <div key={i} className="text-xs rounded-lg bg-amber-50 border border-amber-200 p-2 mb-1">
                    <b>{v.kind}</b> · {v.explanation} (Δ {v.delta_kobo} kobo)
                  </div>
                ))}
              </div>
            ) : <Empty>Loading…</Empty>}
          </Card>
          <Card title="Settlement recon" actions={<button className="btn-ghost" onClick={runRecon}>Run</button>}>
            {recon ? (
              <div className="text-sm space-y-2">
                <Row k="Period" v={recon.period} />
                <Row k="Receipts" v={recon.receipts} />
                <Row k="VAT" v={kobo(recon.vat_kobo)} />
                <Row k="Federal / state" v={`${kobo(recon.federal_kobo)} / ${kobo(recon.state_kobo)}`} />
                <Row k="Ledger" v={`${recon.ledger_mode} · ${recon.ledger_transfer_id}`} />
              </div>
            ) : <Empty>Post period VAT to the remittance ledger (300).</Empty>}
          </Card>
        </div>
      </div>
    </div>
  )
}

function Row({ k, v }: { k: string; v: any }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-sand-500">{k}</span><span className="text-right text-xs">{v}</span>
    </div>
  )
}
