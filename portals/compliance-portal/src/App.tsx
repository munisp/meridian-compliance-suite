import React from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { useAuth } from './auth'
import Layout from './components/Layout'
import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import Einvoicing from './pages/Einvoicing'
import Wht from './pages/Wht'
import Etr from './pages/Etr'
import Vasp from './pages/Vasp'
import Pos from './pages/Pos'
import Cases from './pages/Cases'

export default function App() {
  const { user } = useAuth()
  if (!user) return <Login />
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Dashboard />} />
        <Route path="/einvoicing" element={<Einvoicing />} />
        <Route path="/wht" element={<Wht />} />
        <Route path="/etr" element={<Etr />} />
        <Route path="/vasp" element={<Vasp />} />
        <Route path="/pos" element={<Pos />} />
        <Route path="/cases" element={<Cases />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}
