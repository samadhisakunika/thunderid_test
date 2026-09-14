import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { ThunderIDProvider } from '@thunderid/react'
import './index.css'
import App from './App.jsx'

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <ThunderIDProvider
      clientId="DEMO_PARTNER_APP"
      applicationId="2e8a7989-709b-47d3-a815-6dc1d929793c"
      baseUrl="https://localhost:8091"
    >
      <App />
    </ThunderIDProvider>
  </StrictMode>,
)
