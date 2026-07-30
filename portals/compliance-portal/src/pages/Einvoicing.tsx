import React, { useState } from 'react'
import { api, kobo } from '../api'
import { Card, Empty, ErrorNote, PageTitle, Status } from '../components/ui'

export default function Einvoicing() {
  const [form, setForm] = useState({ supplier_tin: '', buyer_tin: '', amount: '100000', currency: 'NGN' })
  const [submitting, setSubmitting] = useState(false)
  const [invoice, setInvoice] = useState<any>(null)
  const [error, setError] = useState<any>(null)
  const [lookupId, setLookupId] = useState('')
  const [lookup, setLookup] = useState<any>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitting(true); setError(null)
    try {
      const res = await api('einvoicing').post('/v1/invoices', {
        supplier_tin: form.supplier_tin, buyer_tin: form.buyer_tin,
        lines: [{ description: 'Consulting services', qty: 1, unit_price_kobo: Math.round(parseFloat(form.amount) * 100) }],
        currency: form.currency,
      })
      setInvoice(res.data)
      // request pre-clearance (IRN + crypto stamp)
      const id = res.data?.id || res.data?.invoice?.id
      if (id) {
        try {
          const pc = await api('einvoicing').post(`/v1/invoices/${id}/preclear`, {})
          setInvoice((inv: any) => ({ ...inv, preclear: pc.data }))
        } catch { /* pre-clearance optional in dev */ }
      }
    } catch (err) { setError(err) } finally { setSubmitting(false) }
  }

  const doLookup = async (e: React.FormEvent) => {
    e.preventDefault(); setLookup(null); setError(null)
    try {
      const res = await api('einvoicing').get(`/v1/invoices/${lookupId}`)
      setLookup(res.data)
    } catch (err) { setError(err) }
  }

  return (
    <div className="space-y-5">
      <PageTitle title="E-Invoicing Console" sub="Submit invoices for MBS pre-clearance · IRN + crypto stamp status (T1/T2)" />
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <Card title="Submit invoice">
          <form onSubmit={submit} className="space-y-3">
            <div><label className="label">Supplier TIN</label>
              <input className="input" value={form.supplier_tin} required
                onChange={(e) => setForm({ ...form, supplier_tin: e.target.value })} placeholder="12345678-0001" /></div>
            <div><label className="label">Buyer TIN</label>
              <input className="input" value={form.buyer_tin} required
                onChange={(e) => setForm({ ...form, buyer_tin: e.target.value })} placeholder="87654321-0001" /></div>
            <div><label className="label">Amount (NGN)</label>
              <input className="input" type="number" min="1" value={form.amount} required
                onChange={(e) => setForm({ ...form, amount: e.target.value })} /></div>
            <button className="btn" disabled={submitting}>{submitting ? 'Submitting…' : 'Submit + pre-clear'}</button>
          </form>
          {error && <div className="mt-3"><ErrorNote error={error} /></div>}
        </Card>
        <Card title="IRN / stamp status">
          {invoice ? (
            <div className="space-y-2 text-sm">
              <Row k="Invoice ID" v={invoice.id || invoice.invoice?.id} />
              <Row k="Status" v={<Status value={invoice.status || invoice.invoice?.status || 'submitted'} />} />
              <Row k="IRN" v={invoice.preclear?.irn || invoice.irn || 'pending'} mono />
              <Row k="Crypto stamp" v={invoice.preclear?.crypto_stamp || invoice.crypto_stamp || 'pending'} mono />
              <Row k="Rule pack" v={invoice.rule_pack_version || invoice.invoice?.rule_pack_version} />
            </div>
          ) : <Empty>No invoice submitted yet — IRN and crypto stamp appear here after pre-clearance.</Empty>}
        </Card>
      </div>
      <Card title="Lookup invoice">
        <form onSubmit={doLookup} className="flex gap-2">
          <input className="input" value={lookupId} onChange={(e) => setLookupId(e.target.value)} placeholder="invoice id" />
          <button className="btn-ghost">Fetch</button>
        </form>
        {lookup && <pre className="mt-3 text-xs bg-sand-50 rounded-lg p-3 overflow-auto">{JSON.stringify(lookup, null, 2)}</pre>}
      </Card>
    </div>
  )
}

function Row({ k, v, mono }: { k: string; v: any; mono?: boolean }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-sand-500">{k}</span>
      <span className={mono ? 'font-mono text-xs text-right break-all' : 'text-right'}>{v}</span>
    </div>
  )
}
