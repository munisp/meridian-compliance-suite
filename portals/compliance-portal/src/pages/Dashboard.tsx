import React, { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, BASES, ServiceKey } from '../api'
import { Card, PageTitle, Status } from '../components/ui'

const SECTIONS: { key: ServiceKey; name: string; desc: string; to: string }[] = [
  { key: 'einvoicing', name: 'E-Invoicing Console', desc: 'Invoice submit, IRN + crypto stamp status', to: '/einvoicing' },
  { key: 'wht', name: 'WHT Dashboard', desc: 'WHT 2024 evaluation + remittance files', to: '/wht' },
  { key: 'etr', name: 'ETR Dashboard', desc: 'Pillar Two ETR, step trace, GIR download', to: '/etr' },
  { key: 'vasp', name: 'VASP / CARF Console', desc: 'Cost basis, ring-fence, CARF + gates', to: '/vasp' },
  { key: 'pos', name: 'Retailer POS Dashboard', desc: 'Receipts, attribution, variance', to: '/pos' },
  { key: 'cases', name: 'Practitioner Workspace', desc: 'Matters, documents, deadlines', to: '/cases' },
]

export default function Dashboard() {
  const [health, setHealth] = useState<Record<string, string>>({})

  useEffect(() => {
    SECTIONS.forEach((s) => {
      api(s.key).get('/healthz')
        .then((r) => setHealth((h) => ({ ...h, [s.key]: r.data?.status || 'ok' })))
        .catch(() => setHealth((h) => ({ ...h, [s.key]: 'offline' })))
    })
  }, [])

  return (
    <div>
      <PageTitle title="Compliance Overview" sub="Market-zone services health and quick navigation" />
      <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
        {SECTIONS.map((s) => (
          <Link key={s.key} to={s.to} className="block group">
            <Card>
              <div className="flex items-start justify-between">
                <div>
                  <div className="font-medium text-sand-900 group-hover:text-clay-700">{s.name}</div>
                  <div className="text-sm text-sand-500 mt-1">{s.desc}</div>
                  <div className="text-xs text-sand-400 mt-3">{BASES[s.key]}</div>
                </div>
                <Status value={health[s.key] || 'checking'} />
              </div>
            </Card>
          </Link>
        ))}
      </div>
    </div>
  )
}
