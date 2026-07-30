import React, { useState } from 'react'
import { api, bps, kobo } from '../api'
import { Card, Empty, ErrorNote, PageTitle, Status } from '../components/ui'

export default function Etr() {
  const [groupId, setGroupId] = useState('demo-mne')
  const [year, setYear] = useState(2025)
  const [basis, setBasis] = useState('dual')
  const [qdmtt, setQdmtt] = useState(false)
  const [comp, setComp] = useState<any>(null)
  const [trace, setTrace] = useState<any[]>([])
  const [error, setError] = useState<any>(null)
  const [busy, setBusy] = useState(false)
  const [seeded, setSeeded] = useState(false)

  const seedDemo = async () => {
    setError(null)
    try {
      await api('etr').post('/v1/etr/groups', {
        id: groupId, name: 'Demo MNE Group', consolidated_revenue_kobo: 20_000_000_000_000_000,
      })
      await api('etr').post('/v1/etr/entities', [
        { id: 'upe-ng', group_id: groupId, name: 'HoldCo Nigeria', jurisdiction: 'NG', is_upe: true,
          net_income_kobo: 500_000_000_00, covered_taxes_kobo: 150_000_000_00,
          payroll_kobo: 100_000_000_00, tangible_assets_kobo: 200_000_000_00 },
        { id: 'sub-mu', group_id: groupId, name: 'Mauritius Sub', jurisdiction: 'MU', parent_id: 'upe-ng',
          net_income_kobo: 1_000_000_000_00, covered_taxes_kobo: 100_000_000_00,
          payroll_kobo: 50_000_000_00, tangible_assets_kobo: 50_000_000_00 },
        { id: 'cfc-gg', group_id: groupId, name: 'Guernsey CFC', jurisdiction: 'GG', parent_id: 'upe-ng',
          is_cfc: true, cfc_taxes_kobo: 30_000_000_00, net_income_kobo: 300_000_000_00 },
      ])
      setSeeded(true)
    } catch (err) { setError(err) }
  }

  const run = async () => {
    setBusy(true); setError(null); setComp(null); setTrace([])
    try {
      const res = await api('etr').post('/v1/etr/compute', {
        group_id: groupId, fiscal_year: year, basis, qdmtt_upgrade: qdmtt,
      })
      setComp(res.data)
      const tr = await api('etr').get(`/v1/etr/computations/${res.data.id}/trace`)
      setTrace(tr.data.steps || [])
    } catch (err) { setError(err) } finally { setBusy(false) }
  }

  const girUrl = comp ? `${(api('etr').defaults.baseURL)}/v1/etr/computations/${comp.id}/gir.xml` : '#'

  return (
    <div className="space-y-5">
      <PageTitle title="ETR Dashboard" sub="Pillar Two / GloBE effective tax rate computation · audit-defensible step trace · GIR (T9)" />
      <Card title="Run computation">
        <div className="flex flex-wrap items-end gap-3">
          <div><label className="label">Group</label>
            <input className="input w-44" value={groupId} onChange={(e) => setGroupId(e.target.value)} /></div>
          <div><label className="label">Fiscal year</label>
            <select className="input" value={year} onChange={(e) => setYear(+e.target.value)}>
              {[2024, 2025, 2026, 2027, 2028].map((y) => <option key={y}>{y}</option>)}
            </select></div>
          <div><label className="label">Basis</label>
            <select className="input" value={basis} onChange={(e) => setBasis(e.target.value)}>
              <option value="dual">dual (NTA + OECD)</option><option value="nta">NTA</option><option value="globe">OECD GloBE</option>
            </select></div>
          <label className="flex items-center gap-2 text-sm text-sand-700 pb-2">
            <input type="checkbox" checked={qdmtt} onChange={(e) => setQdmtt(e.target.checked)} />
            QDMTT upgrade armed
          </label>
          <button className="btn-ghost" onClick={seedDemo}>Seed demo group</button>
          <button className="btn" onClick={run} disabled={busy}>{busy ? 'Computing…' : 'Run ETR'}</button>
        </div>
        {seeded && <div className="text-xs text-moss-700 mt-2">Demo group seeded (NG UPE + low-tax MU sub + GG CFC).</div>}
        {error && <div className="mt-3"><ErrorNote error={error} /></div>}
      </Card>

      {comp && (
        <>
          <div className="grid grid-cols-1 lg:grid-cols-3 gap-5">
            <Card title="Result">
              <div className="space-y-2 text-sm">
                <Row k="In scope" v={comp.in_scope ? 'yes' : 'no'} />
                <Row k="Total top-up" v={kobo(comp.total_topup_kobo)} />
                <Row k="CFC pool" v={kobo(comp.cfc_pool_kobo)} />
                <Row k="Digest" v={<span className="font-mono text-xs">{comp.digest?.slice(0, 16)}…</span>} />
                <div className="pt-2 flex gap-2">
                  <a className="btn-ghost text-xs" href={girUrl} target="_blank" rel="noreferrer">Download GIR XML</a>
                </div>
              </div>
            </Card>
            <Card title="Jurisdictions" >
              <table className="w-full">
                <thead><tr><th className="th">Jur.</th><th className="th">ETR</th><th className="th">Top-up %</th><th className="th">Top-up</th></tr></thead>
                <tbody>
                  {(comp.jurisdictions || []).map((j: any) => (
                    <tr key={j.jurisdiction + j.basis}>
                      <td className="td font-medium">{j.jurisdiction} <span className="text-xs text-sand-400">{j.basis}</span></td>
                      <td className="td">{bps(j.etr_bps)}</td>
                      <td className="td">{bps(j.topup_pct_bps)}</td>
                      <td className="td">{kobo(j.topup_kobo)}{j.qdmtt_applied && <span className="badge bg-moss-500/15 text-moss-700 ml-1">QDMTT</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
            <Card title="IIR allocations">
              {(comp.iir_allocations || []).length === 0 ? <Empty>No residual top-up to allocate.</Empty> : (
                <table className="w-full">
                  <thead><tr><th className="th">From</th><th className="th">To</th><th className="th">Via</th><th className="th">Amount</th></tr></thead>
                  <tbody>
                    {comp.iir_allocations.map((a: any, i: number) => (
                      <tr key={i}>
                        <td className="td">{a.from_jurisdiction}</td>
                        <td className="td">{a.to_entity_name}</td>
                        <td className="td"><Status value={a.mechanism} /></td>
                        <td className="td">{kobo(a.amount_kobo)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Card>
          </div>
          <Card title="Audit step trace">
            <ol className="space-y-3">
              {trace.map((s: any) => (
                <li key={s.step_no} className="border-l-2 border-clay-500/50 pl-4">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-mono text-sand-400">#{s.step_no}</span>
                    <span className="text-sm font-medium text-sand-800">{s.name}</span>
                    <span className="badge bg-sand-200 text-sand-600">{s.pack}</span>
                  </div>
                  <div className="text-xs text-sand-500 mt-0.5">{s.narrate}</div>
                  <div className="text-xs font-mono text-sand-400 mt-0.5">rule: {s.rule_ref}</div>
                </li>
              ))}
            </ol>
          </Card>
        </>
      )}
    </div>
  )
}

function Row({ k, v }: { k: string; v: any }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-sand-500">{k}</span><span className="text-right">{v}</span>
    </div>
  )
}
