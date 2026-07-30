import React, { useState } from 'react'
import { api, kobo } from '../api'
import { Card, Empty, ErrorNote, PageTitle, Status } from '../components/ui'

export default function Wht() {
  const [form, setForm] = useState({ payment_type: 'dividend', beneficiary: 'company', amount: '5000000', vendor_tin: '' })
  const [result, setResult] = useState<any>(null)
  const [error, setError] = useState<any>(null)
  const [busy, setBusy] = useState(false)
  const [credits, setCredits] = useState<any>(null)
  const [remit, setRemit] = useState<any>(null)

  const evaluate = async (e: React.FormEvent) => {
    e.preventDefault(); setBusy(true); setError(null)
    try {
      const res = await api('wht').post('/v1/wht/evaluate', {
        payment_type: form.payment_type, beneficiary: form.beneficiary,
        amount_kobo: Math.round(parseFloat(form.amount) * 100), vendor_tin: form.vendor_tin || undefined,
      })
      setResult(res.data)
    } catch (err) { setError(err) } finally { setBusy(false) }
  }

  const loadCredits = async () => {
    setError(null)
    try {
      const res = await api('wht').get(`/v1/wht/credits/${encodeURIComponent(form.vendor_tin)}`)
      setCredits(res.data)
    } catch (err) { setError(err) }
  }

  const buildRemit = async () => {
    setError(null)
    try {
      const res = await api('wht').post('/v1/wht/remit-file', { period: new Date().toISOString().slice(0, 7) })
      setRemit(res.data)
    } catch (err) { setError(err) }
  }

  return (
    <div className="space-y-5">
      <PageTitle title="WHT Dashboard" sub="WHT 2024 rules evaluation via rp-wht-2024 · credit ledger · remittance files (T7)" />
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <Card title="Evaluate withholding">
          <form onSubmit={evaluate} className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <div><label className="label">Payment type</label>
                <select className="input" value={form.payment_type} onChange={(e) => setForm({ ...form, payment_type: e.target.value })}>
                  {['dividend', 'interest', 'royalty', 'rent', 'service_fee', 'contract', 'commission', 'director_fee'].map((p) => <option key={p}>{p}</option>)}
                </select></div>
              <div><label className="label">Beneficiary</label>
                <select className="input" value={form.beneficiary} onChange={(e) => setForm({ ...form, beneficiary: e.target.value })}>
                  <option value="company">company</option><option value="individual">individual</option>
                </select></div>
            </div>
            <div><label className="label">Amount (NGN)</label>
              <input className="input" type="number" min="1" value={form.amount}
                onChange={(e) => setForm({ ...form, amount: e.target.value })} /></div>
            <div><label className="label">Vendor TIN (optional — no-TIN doubles rate)</label>
              <input className="input" value={form.vendor_tin} onChange={(e) => setForm({ ...form, vendor_tin: e.target.value })} /></div>
            <button className="btn" disabled={busy}>{busy ? 'Evaluating…' : 'Evaluate'}</button>
          </form>
          {error && <div className="mt-3"><ErrorNote error={error} /></div>}
        </Card>
        <Card title="Evaluation result">
          {result ? (
            <div className="space-y-2 text-sm">
              <Row k="Decision" v={result.decision || result.rule_id} />
              <Row k="Rate" v={result.rate_bps != null ? `${result.rate_bps / 100}%` : '—'} />
              <Row k="WHT amount" v={kobo(result.wht_kobo ?? result.amount_kobo)} />
              <Row k="Narration" v={result.narrate} />
              <Row k="Trace" v={<pre className="text-xs whitespace-pre-wrap">{JSON.stringify(result.trace ?? result.rules_applied ?? [], null, 1)}</pre>} />
            </div>
          ) : <Empty>Run an evaluation to see the decision + rule trace.</Empty>}
        </Card>
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <Card title="Vendor WHT credits" actions={<button className="btn-ghost" onClick={loadCredits} disabled={!form.vendor_tin}>Load</button>}>
          {credits ? <pre className="text-xs bg-sand-50 rounded-lg p-3 overflow-auto">{JSON.stringify(credits, null, 2)}</pre>
            : <Empty>Enter a vendor TIN and load their credit ledger.</Empty>}
        </Card>
        <Card title="Remittance file" actions={<button className="btn-ghost" onClick={buildRemit}>Generate</button>}>
          {remit ? (
            <div className="text-sm space-y-2">
              <Row k="File ID" v={remit.file_id || remit.id} mono />
              <Row k="Period" v={remit.period} />
              <Row k="Entries" v={remit.entries ?? remit.count} />
              <Row k="Total" v={kobo(remit.total_kobo)} />
            </div>
          ) : <Empty>Generate the monthly remittance file (CSV + XML).</Empty>}
        </Card>
      </div>
    </div>
  )
}

function Row({ k, v, mono }: { k: string; v: any; mono?: boolean }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-sand-500 shrink-0">{k}</span>
      <span className={mono ? "text-right break-all font-mono text-xs" : "text-right break-all"}>{v ?? '—'}</span>
    </div>
  )
}
