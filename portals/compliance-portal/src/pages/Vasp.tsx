import React, { useEffect, useState } from 'react'
import { api, errMsg, kobo } from '../api'
import { Card, Empty, ErrorNote, Field, MoneyInput, PageTitle, Status } from '../components/ui'

export default function Vasp() {
  const [gates, setGates] = useState<Record<string, any>>({})
  const [form, setForm] = useState({ user_hash: 'user-001', asset: 'BTC', side: 'buy', qty: '1' })
  const [priceKobo, setPriceKobo] = useState<number | null>(6_000_000_000)
  const [method, setMethod] = useState('fifo')
  const [msg, setMsg] = useState<any>(null)
  const [basis, setBasis] = useState<any>(null)
  const [carf, setCarf] = useState<any>(null)
  const [carfList, setCarfList] = useState<any[]>([])
  const [error, setError] = useState<any>(null)
  const [loadErr, setLoadErr] = useState('')
  const [tenant] = useState('demo-vasp')

  const loadGates = () => {
    setLoadErr('')
    const errs: string[] = []
    api('vasp').get('/v1/gates').then((r) => setGates(r.data.gates || {}))
      .catch((e) => { errs.push(errMsg(e)); setLoadErr(errs.join(' · ')) })
    api('vasp').get('/v1/carf/messages').then((r) => setCarfList(r.data.messages || []))
      .catch((e) => { errs.push(errMsg(e)); setLoadErr(errs.join(' · ')) })
  }
  useEffect(loadGates, [])

  const transmitClosed = !(gates['carf.transmit_enabled']?.open && gates['carf.gate.changed']?.open)

  const ingest = async (e: React.FormEvent) => {
    e.preventDefault(); setError(null); setMsg(null)
    try {
      const res = await api('vasp').post(`/v1/trades?method=${method}`, {
        tenant_id: tenant, user_hash: form.user_hash, asset: form.asset, side: form.side,
        qty_milli: Math.round(parseFloat(form.qty) * 1000),
        price_kobo: priceKobo ?? 0,
      })
      setMsg(res.data)
    } catch (err) { setError(err) }
  }

  const loadBasis = async () => {
    setError(null)
    try {
      const res = await api('vasp').get(`/v1/costbasis/${form.asset}?tenant_id=${tenant}&user_hash=${form.user_hash}&method=${method}`)
      setBasis(res.data)
    } catch (err) { setError(err) }
  }

  const buildCarf = async () => {
    setError(null)
    try {
      const res = await api('vasp').post('/v1/carf/build', { tenant_id: tenant, period: '' })
      setCarf(res.data); loadGates()
    } catch (err) { setError(err) }
  }

  const transmit = async (id: string) => {
    setError(null)
    try {
      const res = await api('vasp').post('/v1/carf/transmit', { message_id: id })
      setCarf(res.data); loadGates()
    } catch (err) { setError(err) }
  }

  return (
    <div className="space-y-5">
      <PageTitle title="VASP / CARF Console" sub="Trade ingest · FIFO/WAC cost basis · ring-fence · OECD CARF builder with gate enforcement (T10)" />

      {loadErr && <ErrorNote error={loadErr} />}

      <div className={`rounded-xl border px-4 py-3 text-sm flex items-center justify-between ${
        transmitClosed ? 'border-warning-strong/40 bg-warning text-warning-on' : 'border-success-strong/40 bg-success text-success-on'}`}>
        <div>
          <span className="font-medium">CARF transmission gate: {transmitClosed ? 'CLOSED' : 'OPEN'}</span>
          <span className="ml-2 text-xs">
            carf.transmit_enabled={String(gates['carf.transmit_enabled']?.open ?? false)} ·
            carf.gate.changed={String(gates['carf.gate.changed']?.open ?? false)} ·
            source: {gates['carf.transmit_enabled']?.source || 'default (fail-safe)'}
          </span>
        </div>
        {transmitClosed && <span className="text-xs">transmission refuses while closed</span>}
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-5">
        <Card title="Ingest trade">
          <form onSubmit={ingest} className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <Field label="User (pseudonym)" id="vasp-user">
                <input id="vasp-user" className="input" value={form.user_hash} onChange={(e) => setForm({ ...form, user_hash: e.target.value })} />
              </Field>
              <Field label="Asset" id="vasp-asset">
                <select id="vasp-asset" className="input" value={form.asset} onChange={(e) => setForm({ ...form, asset: e.target.value })}>
                  {['BTC', 'ETH', 'USDT', 'SOL'].map((a) => <option key={a}>{a}</option>)}
                </select>
              </Field>
              <Field label="Side" id="vasp-side">
                <select id="vasp-side" className="input" value={form.side} onChange={(e) => setForm({ ...form, side: e.target.value })}>
                  <option>buy</option><option>sell</option>
                </select>
              </Field>
              <Field label="Method" id="vasp-method">
                <select id="vasp-method" className="input" value={method} onChange={(e) => setMethod(e.target.value)}>
                  <option value="fifo">FIFO</option><option value="wac">weighted average</option>
                </select>
              </Field>
              <Field label="Qty (asset units)" id="vasp-qty">
                <input id="vasp-qty" className="input" type="number" step="0.001" min="0.001" value={form.qty}
                  onChange={(e) => setForm({ ...form, qty: e.target.value })} />
              </Field>
              <Field label="Price (NGN / asset)" required>
                {(id, describedBy) => (
                  <MoneyInput id={id} valueKobo={priceKobo} onChangeKobo={setPriceKobo}
                    aria-describedby={describedBy} aria-required />)}
              </Field>
            </div>
            <button className="btn">Ingest</button>
          </form>
          {msg && (
            <div className="mt-3 text-sm space-y-1">
              <div>Trade <span className="font-mono text-xs">{msg.trade_id}</span> ingested.</div>
              {msg.gain_loss && (
                <div className="rounded-lg bg-neutral-50 p-2 text-xs">
                  gain/loss: <b>{kobo(msg.gain_loss.gain_loss_kobo)}</b> ({msg.gain_loss.method}, proceeds {kobo(msg.gain_loss.proceeds_kobo)} / basis {kobo(msg.gain_loss.basis_kobo)})
                </div>
              )}
            </div>
          )}
          {error && <div className="mt-3"><ErrorNote error={error} /></div>}
        </Card>

        <Card title="Cost basis" actions={<button className="btn-ghost" onClick={loadBasis}>Check {method.toUpperCase()}</button>}>
          {basis ? (
            <div className="text-sm space-y-2">
              <Row k="Asset" v={basis.asset} />
              <Row k="Qty held (milli)" v={basis.qty_milli} />
              <Row k="Remaining basis" v={kobo(basis.basis_kobo)} />
              {basis.avg_cost_kobo_per_asset && <Row k="Avg cost" v={kobo(basis.avg_cost_kobo_per_asset)} />}
              {basis.open_lots != null && <Row k="Open lots" v={basis.open_lots} />}
            </div>
          ) : <Empty>Check the current position basis for the selected user/asset.</Empty>}
        </Card>
      </div>

      <Card title="CARF messages" actions={<button className="btn" onClick={buildCarf}>Build CARF XML</button>}>
        {carfList.length === 0 ? <Empty>No CARF messages built yet.</Empty> : (
          <table className="w-full">
            <thead><tr><th scope="col" className="th">Message</th><th scope="col" className="th">Type</th><th scope="col" className="th">Users</th><th scope="col" className="th">Txns</th><th scope="col" className="th">Validation</th><th scope="col" className="th">Status</th><th scope="col" className="th"></th></tr></thead>
            <tbody>
              {carfList.map((m: any) => (
                <tr key={m.id}>
                  <td className="td font-mono text-xs">{m.message_ref_id?.slice(0, 18)}…</td>
                  <td className="td">{m.doc_type_indic}{m.corr_of && ' (correction)'}</td>
                  <td className="td">{m.users}</td>
                  <td className="td">{m.transactions}</td>
                  <td className="td">{m.validation?.length ? <span className="text-danger-strong text-xs">{m.validation.join('; ')}</span> : <span className="text-success-strong text-xs">valid</span>}</td>
                  <td className="td"><Status value={m.status} /></td>
                  <td className="td">
                    <div className="flex gap-1">
                      <button className="btn-ghost !py-1 !px-2 text-xs" onClick={() => transmit(m.id)} disabled={transmitClosed}>Transmit</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {carf && carf.xml && (
          <details className="mt-3">
            <summary className="text-sm text-neutral-600 cursor-pointer">Latest message XML</summary>
            <pre className="text-xs bg-neutral-50 rounded-lg p-3 mt-2 overflow-auto max-h-72">{carf.xml}</pre>
          </details>
        )}
      </Card>
    </div>
  )
}

function Row({ k, v }: { k: string; v: any }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-stone-600">{k}</span><span className="text-right">{v}</span>
    </div>
  )
}
