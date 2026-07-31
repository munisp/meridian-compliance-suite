import React, { useState } from 'react'
import { api } from '../api'
import { Card, Empty, ErrorNote, Field, MoneyInput, PageTitle, Status } from '../components/ui'

export default function Einvoicing() {
  const [form, setForm] = useState({ supplier_tin: '', buyer_tin: '', currency: 'NGN' })
  const [amountKobo, setAmountKobo] = useState<number | null>(10_000_000)
  const [submitting, setSubmitting] = useState(false)
  const [invoice, setInvoice] = useState<any>(null)
  const [error, setError] = useState<any>(null)
  const [lookupId, setLookupId] = useState('')
  const [lookup, setLookup] = useState<any>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSubmitting(true); setError(null)
    try {
      // Canonical REST payload — must match CanonicalInvoice JSON exactly
      // (contract test: services/einvoicing TestPortalPayloadContract).
      if (amountKobo == null) throw new Error('Enter a valid amount')
      const today = new Date().toISOString().slice(0, 10)
      const res = await api('einvoicing').post('/v1/invoices', {
        invoice_number: `WEB-${Date.now()}`,
        invoice_type: 'B2B',
        issue_date: today,
        currency: form.currency,
        supplier: { tin: form.supplier_tin, name: `Supplier ${form.supplier_tin}`, country: 'NG' },
        customer: { tin: form.buyer_tin, name: `Buyer ${form.buyer_tin}`, country: 'NG' },
        lines: [{
          id: '1', description: 'Consulting services',
          quantity_milli: 1000, unit_price_kobo: amountKobo,
          vat_category: 'S', vat_rate_bps: 750,
        }],
      })
      setInvoice(res.data)
      // request pre-clearance (IRN + crypto stamp)
      const id = res.data?.id || res.data?.invoice?.id
      if (id) {
        try {
          const pc = await api('einvoicing').post(`/v1/invoices/${id}/preclear`, {})
          setInvoice((inv: any) => ({ ...inv, preclear: pc.data }))
          try {
            const qr = await api('einvoicing').get(`/v1/invoices/${id}/qr`)
            setInvoice((inv: any) => ({ ...inv, qr: qr.data }))
          } catch { /* QR optional until cleared */ }
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
            <Field label="Supplier TIN" required>
              {(id, describedBy) => (
                <input id={id} className="input font-mono" value={form.supplier_tin} required
                  onChange={(e) => setForm({ ...form, supplier_tin: e.target.value })} placeholder="12345678-0001"
                  aria-describedby={describedBy} aria-required="true" />)}
            </Field>
            <Field label="Buyer TIN" required>
              {(id, describedBy) => (
                <input id={id} className="input font-mono" value={form.buyer_tin} required
                  onChange={(e) => setForm({ ...form, buyer_tin: e.target.value })} placeholder="87654321-0001"
                  aria-describedby={describedBy} aria-required="true" />)}
            </Field>
            <Field label="Amount (NGN)" required>
              {(id, describedBy) => (
                <MoneyInput id={id} valueKobo={amountKobo} onChangeKobo={setAmountKobo}
                  aria-describedby={describedBy} aria-required />)}
            </Field>
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
              {invoice.qr?.qr_svg && (
                <div className="pt-2">
                  <span className="text-stone-600 text-xs">Verification QR (IRN + HMAC)</span>
                  <div className="mt-1 w-36" dangerouslySetInnerHTML={{ __html: invoice.qr.qr_svg }} />
                  <div className="font-mono text-[10px] break-all text-stone-600 mt-1">{invoice.qr.payload}</div>
                </div>
              )}
            </div>
          ) : <Empty>No invoice submitted yet — IRN and crypto stamp appear here after pre-clearance.</Empty>}
        </Card>
      </div>
      <Card title="Lookup invoice">
        <form onSubmit={doLookup} className="flex gap-2">
          <input className="input" value={lookupId} onChange={(e) => setLookupId(e.target.value)} placeholder="invoice id" />
          <button className="btn-ghost">Fetch</button>
        </form>
        {lookup && <pre className="mt-3 text-xs bg-neutral-50 rounded-lg p-3 overflow-auto">{JSON.stringify(lookup, null, 2)}</pre>}
      </Card>
    </div>
  )
}

function Row({ k, v, mono }: { k: string; v: any; mono?: boolean }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-stone-600">{k}</span>
      <span className={mono ? 'font-mono text-xs text-right break-all' : 'text-right'}>{v}</span>
    </div>
  )
}
