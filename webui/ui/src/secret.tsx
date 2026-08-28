// A key's secret, shown the one time it exists. The manager answers a created
// key with its secret once and keeps only a hash, so every screen that creates
// a key shows it in these same words.
export function SecretReveal({ name, secret, copied, onCopy, onDone }: {
  name: string
  secret: string
  copied: boolean
  onCopy: () => void
  onDone: () => void
}) {
  return (
    <div className="secret-reveal" role="alert">
      <strong>“{name}” created. Copy the key now.</strong>
      <span className="muted">It will not be shown again.</span>
      <code>{secret}</code>
      <div style={{ display: 'flex', gap: 8 }}>
        <button className="primary" onClick={onCopy}>{copied ? 'Copied' : 'Copy key'}</button>
        <button className="ghost" onClick={onDone}>Done</button>
      </div>
    </div>
  )
}
