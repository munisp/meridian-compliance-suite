import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: { port: 5173 },
  build: {
    rollupOptions: {
      output: {
        // PERF: the portal shipped as a single 401 kB chunk — split heavy
        // vendors into cache-friendly chunks so a code change does not
        // invalidate React/router/i18n for every returning visitor.
        manualChunks: {
          react: ['react', 'react-dom', 'react-router-dom'],
          oidc: ['oidc-client-ts'],
          i18n: ['i18next', 'react-i18next'],
          ui: ['lucide-react', 'axios'],
        },
      },
    },
  },
})
