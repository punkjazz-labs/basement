import { useState } from 'react'
import { api, setCSRF } from '../api'

export default function Pairing({ onPaired }: { onPaired: () => void }) {
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const pair = async (event: React.FormEvent) => {
    event.preventDefault()
    setError('')
    setBusy(true)
    try {
      const result = await api<{ csrf_token: string }>('/api/v1/auth/pair', {
        method: 'POST',
        body: JSON.stringify({ token }),
      })
      setCSRF(result.csrf_token)
      onPaired()
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : 'Pairing failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="pairing">
      <div>
        <p className="kicker">RunOnSpark manager</p>
        <h1>Pair this Spark</h1>
      </div>
      <p className="muted">
        Enter the pairing token shown when the manager was installed.
      </p>
      <form onSubmit={pair}>
        <input
          type="password"
          value={token}
          onChange={event => setToken(event.target.value)}
          placeholder="Pairing token"
          aria-label="Pairing token"
          autoComplete="off"
          required
        />
        <button className="primary" disabled={busy}>Pair</button>
      </form>
      {error && <p className="error-text" role="alert">{error}</p>}
      <p className="hint">
        Lost it? On the Spark, run <code>runonspark-manager pairing-url</code> to print it again.
      </p>
    </main>
  )
}
