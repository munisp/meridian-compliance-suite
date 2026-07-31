import React, { useEffect, useState } from 'react'
import { api } from '../api'
import { Card, Empty, ErrorNote, Field, PageTitle, Status } from '../components/ui'

export default function Cases() {
  const [matters, setMatters] = useState<any[]>([])
  const [selected, setSelected] = useState<any>(null)
  const [docs, setDocs] = useState<any[]>([])
  const [deadlines, setDeadlines] = useState<any[]>([])
  const [form, setForm] = useState({ title: '', client_id: '', practice_area: 'tax-appeal' })
  const [dlForm, setDlForm] = useState({ title: '', due_at: '', severity: 'info' })
  const [error, setError] = useState<any>(null)
  const [evidence, setEvidence] = useState<any>(null)

  const load = () => {
    api('cases').get('/v1/matters').then((r) => setMatters(r.data.matters || [])).catch((e) => setError(e))
  }
  useEffect(load, [])

  const open = async (m: any) => {
    setSelected(m); setEvidence(null)
    try {
      const d = await api('cases').get(`/v1/matters/${m.id}/documents`)
      setDocs(d.data.documents || [])
      const dl = await api('cases').get(`/v1/deadlines?matter_id=${m.id}`)
      setDeadlines(dl.data.deadlines || [])
    } catch (e) { setError(e) }
  }

  const create = async (e: React.FormEvent) => {
    e.preventDefault(); setError(null)
    try {
      await api('cases').post('/v1/matters', { ...form, tenant_id: 'chambers-1' })
      setForm({ title: '', client_id: '', practice_area: 'tax-appeal' })
      load()
    } catch (err) { setError(err) }
  }

  const addDeadline = async (e: React.FormEvent) => {
    e.preventDefault(); setError(null)
    try {
      await api('cases').post(`/v1/matters/${selected.id}/deadlines`, {
        title: dlForm.title, due_at: new Date(dlForm.due_at).toISOString(), severity: dlForm.severity,
      })
      setDlForm({ title: '', due_at: '', severity: 'info' })
      open(selected)
    } catch (err) { setError(err) }
  }

  const buildEvidence = async () => {
    setError(null)
    try {
      const res = await api('cases').post(`/v1/matters/${selected.id}/evidence-pack`, {})
      setEvidence(res.data)
    } catch (err) { setError(err) }
  }

  return (
    <div className="space-y-5">
      <PageTitle title="Practitioner Workspace" sub="Matters · privileged documents · deadlines + escalation · evidence packs (T13-practitioner)" />
      {error && <ErrorNote error={error} />}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-5">
        <div className="space-y-5">
          <Card title="New matter">
            <form onSubmit={create} className="space-y-3">
              <Field label="Title" required id="matter-title">
                <input id="matter-title" className="input" required aria-required="true" value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} />
              </Field>
              <Field label="Client ID" required id="matter-client">
                <input id="matter-client" className="input" required aria-required="true" value={form.client_id} onChange={(e) => setForm({ ...form, client_id: e.target.value })} />
              </Field>
              <Field label="Practice area" id="matter-area">
                <select id="matter-area" className="input" value={form.practice_area} onChange={(e) => setForm({ ...form, practice_area: e.target.value })}>
                  <option value="tax-appeal">tax-appeal</option><option value="audit-defence">audit-defence</option>
                  <option value="advisory">advisory</option><option value="ombud-referral">ombud-referral</option>
                </select>
              </Field>
              <button className="btn">Open matter</button>
            </form>
          </Card>
          <Card title={`Matters (${matters.length})`}>
            {matters.length === 0 ? <Empty>No matters yet.</Empty> : (
              <ul className="divide-y divide-neutral-100">
                {matters.map((m: any) => (
                  <li key={m.id}>
                    <button onClick={() => open(m)}
                      className={`w-full text-left px-2 py-2 rounded-lg hover:bg-neutral-50 ${selected?.id === m.id ? 'bg-neutral-100' : ''}`}>
                      <div className="flex items-center justify-between">
                        <span className="text-sm font-medium text-neutral-800">{m.title}</span>
                        <Status value={m.status} />
                      </div>
                      <div className="text-xs text-stone-600">{m.ref} · {m.client_name || m.client_id}</div>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>

        {selected ? (
          <div className="lg:col-span-2 space-y-5">
            <Card title={`${selected.title} — documents (${docs.length})`}
              actions={<button className="btn-ghost" onClick={buildEvidence}>Build evidence pack</button>}>
              {docs.length === 0 ? <Empty>No documents uploaded.</Empty> : (
                <table className="w-full">
                  <thead><tr><th scope="col" className="th">Name</th><th scope="col" className="th">SHA-256</th><th scope="col" className="th">Privilege</th><th scope="col" className="th">Uploaded</th></tr></thead>
                  <tbody>
                    {docs.map((d: any) => (
                      <tr key={d.id}>
                        <td className="td">{d.name}</td>
                        <td className="td font-mono text-xs">{d.sha256?.slice(0, 12)}…</td>
                        <td className="td">{d.privileged ? <span className="chip-warning">privileged</span> : <span className="text-xs text-stone-600">—</span>}</td>
                        <td className="td text-xs">{d.uploaded_at?.slice(0, 10)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {evidence && (
                <div className="mt-3 rounded-lg bg-success border border-success-strong/40 p-3 text-xs">
                  Evidence pack sealed → WORM <span className="font-mono">{evidence.worm_receipt?.worm_uri}</span>
                  <br />sha256: <span className="font-mono">{evidence.worm_receipt?.sha256?.slice(0, 24)}…</span> ({evidence.worm_receipt?.source})
                </div>
              )}
            </Card>
            <Card title="Deadlines">
              {deadlines.length === 0 ? <Empty>No deadlines set.</Empty> : (
                <table className="w-full mb-4">
                  <thead><tr><th scope="col" className="th">Title</th><th scope="col" className="th">Due</th><th scope="col" className="th">Severity</th><th scope="col" className="th">Status</th></tr></thead>
                  <tbody>
                    {deadlines.map((d: any) => (
                      <tr key={d.id}>
                        <td className="td">{d.title}</td>
                        <td className="td text-xs">{d.due_at?.replace('T', ' ').slice(0, 16)}</td>
                        <td className="td"><Status value={d.severity} /></td>
                        <td className="td"><Status value={d.status} /></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              <form onSubmit={addDeadline} className="flex flex-wrap items-end gap-2">
                <Field label="Title" required id="dl-title">
                  <input id="dl-title" className="input w-48" required aria-required="true" value={dlForm.title} onChange={(e) => setDlForm({ ...dlForm, title: e.target.value })} />
                </Field>
                <Field label="Due" required id="dl-due">
                  <input id="dl-due" className="input" type="datetime-local" required aria-required="true" value={dlForm.due_at} onChange={(e) => setDlForm({ ...dlForm, due_at: e.target.value })} />
                </Field>
                <Field label="Severity" id="dl-severity">
                  <select id="dl-severity" className="input" value={dlForm.severity} onChange={(e) => setDlForm({ ...dlForm, severity: e.target.value })}>
                    <option>info</option><option>warning</option><option>critical</option>
                  </select>
                </Field>
                <button className="btn">Add</button>
              </form>
            </Card>
          </div>
        ) : (
          <div className="lg:col-span-2"><Card><Empty>Select a matter to view documents and deadlines.</Empty></Card></div>
        )}
      </div>
    </div>
  )
}
