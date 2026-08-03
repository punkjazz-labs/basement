import { useEffect, useState } from 'react'
import { api, idempotency, copyText, type APIKey } from '../api'
import { confirmBox, noticeBox } from '../confirm'

const SNIPPETS = ['curl', 'Python', 'JavaScript', 'LiteLLM'] as const
type Snippet = (typeof SNIPPETS)[number]

function snippetFor(kind: Snippet, base: string, model: string): string {
  switch (kind) {
    case 'curl':
      return `curl ${base}/chat/completions \\
  -H "Authorization: Bearer $BASEMENT_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${model}",
    "messages": [{"role": "user", "content": "Hello from my Spark"}]
  }'`
    case 'Python':
      return `import os
from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key=os.environ["BASEMENT_API_KEY"],
)

response = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "Hello from my Spark"}],
)
print(response.choices[0].message.content)`
    case 'JavaScript':
      return `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${base}",
  apiKey: process.env.BASEMENT_API_KEY,
});

const response = await client.chat.completions.create({
  model: "${model}",
  messages: [{ role: "user", content: "Hello from my Spark" }],
});
console.log(response.choices[0].message.content);`
    case 'LiteLLM':
      return `model_list:
  - model_name: spark
    litellm_params:
      model: openai/${model}
      api_base: ${base}
      api_key: os.environ/BASEMENT_API_KEY`
  }
}

export default function Connect({ activeModelID }: { activeModelID?: string }) {
  const [keys, setKeys] = useState<APIKey[]>([])
  const [newName, setNewName] = useState('')
  const [freshSecret, setFreshSecret] = useState<{ name: string; secret: string } | null>(null)
  const [snippet, setSnippet] = useState<Snippet>('curl')
  const [copied, setCopied] = useState('')

  const base = `${window.location.origin}/v1`
  const model = activeModelID ?? '<model id, shown when a model is active>'

  const load = () => api<APIKey[]>('/api/v1/keys').then(setKeys).catch(() => {})
  useEffect(() => {
    load()
  }, [])

  const create = async (event: React.FormEvent) => {
    event.preventDefault()
    try {
      const result = await api<{ key: APIKey; secret: string }>('/api/v1/keys', {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ name: newName }),
      })
      setFreshSecret({ name: result.key.name, secret: result.secret })
      setNewName('')
      load()
    } catch (problem) {
      noticeBox('Could not create the key', problem instanceof Error ? problem.message : undefined)
    }
  }

  const revoke = async (key: APIKey) => {
    const { ok } = await confirmBox({
      title: `Revoke “${key.name}”?`,
      body: 'Clients using this key stop working immediately.',
      confirmLabel: 'Revoke key',
      danger: true,
    })
    if (!ok) return
    try {
      await api(`/api/v1/keys/${encodeURIComponent(key.id)}`, { method: 'DELETE', body: '{}' })
      load()
    } catch (problem) {
      noticeBox('Could not revoke the key', problem instanceof Error ? problem.message : undefined)
    }
  }

  const copy = async (label: string, value: string) => {
    await copyText(value)
    setCopied(label)
    setTimeout(() => setCopied(''), 1600)
  }

  return (
    <div className="stack">
      <section className="card">
        <p className="kicker">Your endpoint</p>
        <div className="endpoint-line">
          <code>{base}</code>
          <button className="ghost" onClick={() => copy('endpoint', base)}>
            {copied === 'endpoint' ? 'Copied' : 'Copy'}
          </button>
        </div>
        <p className="muted" style={{ marginBottom: 0 }}>
          One address, always. Switching models never changes it, so clients keep working.
          {activeModelID && (
            <>
              {' '}Current model ID:{' '}
              <code>{activeModelID}</code>{' '}
              <button className="quiet" onClick={() => copy('model', activeModelID)}>
                {copied === 'model' ? 'Copied' : 'Copy'}
              </button>
            </>
          )}
        </p>
      </section>

      <section className="card">
        <div className="section-head" style={{ marginBottom: 6 }}>
          <h2 style={{ fontSize: 16 }}>API keys</h2>
          <span className="muted">Required for any client that is not this console</span>
        </div>
        {freshSecret && (
          <div className="secret-reveal" role="alert">
            <strong>“{freshSecret.name}” created. Copy the key now.</strong>
            <span className="muted">For security it is never shown again.</span>
            <code>{freshSecret.secret}</code>
            <div style={{ display: 'flex', gap: 8 }}>
              <button className="primary" onClick={() => copy('secret', freshSecret.secret)}>
                {copied === 'secret' ? 'Copied' : 'Copy key'}
              </button>
              <button className="ghost" onClick={() => setFreshSecret(null)}>Done</button>
            </div>
          </div>
        )}
        {keys.map(key => (
          <div className="key-row" key={key.id}>
            <div className="grow">
              <strong>{key.name}</strong>
              <div className="faint" style={{ fontSize: 12 }}>
                Created {new Date(key.created_at).toLocaleDateString()}
                {key.last_used_at ? ` · last used ${new Date(key.last_used_at).toLocaleString()}` : ' · never used'}
              </div>
            </div>
            <button className="danger" onClick={() => revoke(key)}>Revoke</button>
          </div>
        ))}
        {keys.length === 0 && !freshSecret && (
          <p className="muted">No keys yet. Create one to connect Cursor, scripts, or anything OpenAI-compatible.</p>
        )}
        <form onSubmit={create} style={{ display: 'flex', gap: 8, marginTop: 12 }}>
          <input
            value={newName}
            onChange={event => setNewName(event.target.value)}
            placeholder="Key name, e.g. laptop"
            aria-label="New key name"
            required
            maxLength={64}
            style={{ flex: 1, background: 'var(--surface-2)', color: 'var(--ink)', border: '1px solid var(--line-strong)', borderRadius: 8, padding: '8px 12px' }}
          />
          <button className="primary">Create key</button>
        </form>
      </section>

      <section className="card">
        <div className="section-head" style={{ marginBottom: 10 }}>
          <h2 style={{ fontSize: 16 }}>Use it anywhere</h2>
        </div>
        <div className="snippet-tabs" role="tablist" aria-label="Integration examples">
          {SNIPPETS.map(name => (
            <button key={name} role="tab" aria-selected={snippet === name} onClick={() => setSnippet(name)}>
              {name}
            </button>
          ))}
        </div>
        <div className="snippet" style={{ marginTop: 10 }}>
          <button className="ghost copy" onClick={() => copy('snippet', snippetFor(snippet, base, model))}>
            {copied === 'snippet' ? 'Copied' : 'Copy'}
          </button>
          <pre><code>{snippetFor(snippet, base, model)}</code></pre>
        </div>
        <p className="faint" style={{ fontSize: 12.5, marginBottom: 0 }}>
          Set <code>BASEMENT_API_KEY</code> to a key from above. Works with the OpenAI SDKs, Cursor, Continue, LiteLLM, Open WebUI, and anything else OpenAI-compatible.
        </p>
      </section>
    </div>
  )
}
