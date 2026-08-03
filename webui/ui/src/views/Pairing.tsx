import { useState } from 'react'
import { api, setCSRF } from '../api'
import { FORM_IGNORED_BY_MANAGERS, IGNORED_BY_MANAGERS } from '../fields'

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
        <p className="kicker">basement</p>
        <h1>Pair this Spark</h1>
      </div>
      <p className="muted">
        Enter the pairing token shown when the manager was installed.
      </p>
      {/* The token is masked because it is on screen next to nothing else,
          but it is a one-time bootstrap credential: after pairing, the
          browser holds a session cookie and this screen never comes back on
          this machine. A manager that saved it would keep an entry it can
          never fill again, so the field opts out. */}
      <form onSubmit={pair} {...FORM_IGNORED_BY_MANAGERS}>
        <input
          type="password"
          name="basement-pairing-token"
          id="basement-pairing-token"
          value={token}
          onChange={event => setToken(event.target.value)}
          placeholder="Pairing token"
          aria-label="Pairing token"
          required
          {...IGNORED_BY_MANAGERS}
        />
        <button className="primary" disabled={busy}>Pair</button>
      </form>
      {error && <p className="error-text" role="alert">{error}</p>}
      <p className="hint">
        Lost it? On the Spark, run <code>basement pairing-url</code> to print it again.
      </p>
    </main>
  )
}
