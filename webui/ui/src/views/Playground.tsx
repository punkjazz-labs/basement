import { useEffect, useRef, useState } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { logoFor } from '../catalog'

interface Message {
  role: 'user' | 'assistant'
  content: string
}

// Thinking models sometimes leak reasoning markup into the text stream, for
// example a stray "</think>" before the answer. Only the answer is shown, and
// only the answer is sent back as conversation history.
const visibleText = (text: string) =>
  text
    .replace(/<think>[\s\S]*?(?:<\/think>|$)/g, '')
    .replace(/^\s*<\/think>/, '')
    .replace(/^\s+/, '')

// Models speak markdown; render it, sanitized, so replies read like answers
// rather than markup.
const renderMarkdown = (text: string) =>
  DOMPurify.sanitize(marked.parse(text, { async: false }) as string)

// Streams straight through the manager's own /v1 proxy using the console
// session — the same endpoint and behavior an API-key client gets.
export default function Playground({ ready, modelID, modelName, recipeID }: {
  ready: boolean
  modelID?: string
  modelName?: string
  recipeID?: string
}) {
  const [messages, setMessages] = useState<Message[]>([])
  const [draft, setDraft] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [stats, setStats] = useState('')
  const abortRef = useRef<AbortController | null>(null)
  const chatRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    chatRef.current?.scrollTo({ top: chatRef.current.scrollHeight })
  }, [messages])
  useEffect(() => () => abortRef.current?.abort(), [])

  const send = async () => {
    const content = draft.trim()
    if (!content || streaming || !ready) return
    const history = [
      ...messages.map(message =>
        message.role === 'assistant' ? { ...message, content: visibleText(message.content) } : message,
      ),
      { role: 'user' as const, content },
    ]
    setMessages([...history, { role: 'assistant', content: '' }])
    setDraft('')
    setStreaming(true)
    setStats('')
    const controller = new AbortController()
    abortRef.current = controller
    const started = performance.now()
    let firstToken = 0
    let chunks = 0
    try {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        // No token cap: thinking models spend freely before the visible answer,
        // and a cap cuts replies mid-sentence. The stop button is the limit.
        body: JSON.stringify({ model: modelID, messages: history, stream: true }),
        signal: controller.signal,
      })
      if (!response.ok || !response.body) {
        const body = await response.json().catch(() => null)
        throw new Error(body?.error?.message ?? `The model returned ${response.status}`)
      }
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split('\n')
        buffer = lines.pop() ?? ''
        for (const line of lines) {
          const data = line.trim()
          if (!data.startsWith('data:')) continue
          const payload = data.slice(5).trim()
          if (payload === '[DONE]') continue
          try {
            const chunk = JSON.parse(payload)
            const delta: string = chunk.choices?.[0]?.delta?.content ?? ''
            if (delta) {
              if (!firstToken) firstToken = performance.now()
              chunks += 1
              setMessages(previous => {
                const next = [...previous]
                next[next.length - 1] = {
                  role: 'assistant',
                  content: next[next.length - 1].content + delta,
                }
                return next
              })
            }
          } catch {
            /* partial chunk; the buffer catches it next round */
          }
        }
      }
      if (firstToken) {
        const generation = (performance.now() - firstToken) / 1000
        const rate = generation > 0 ? (chunks / generation).toFixed(1) : 'n/a'
        setStats(`${chunks} tokens · ${rate} tok/s · first token in ${Math.round(firstToken - started)} ms`)
      }
    } catch (problem) {
      if ((problem as Error).name !== 'AbortError') {
        setMessages(previous => {
          const next = [...previous]
          next[next.length - 1] = {
            role: 'assistant',
            content: problem instanceof Error ? problem.message : 'The request failed.',
          }
          return next
        })
      }
    } finally {
      setStreaming(false)
      abortRef.current = null
    }
  }

  if (!ready) {
    return (
      <div className="empty">
        <p><strong>No model is serving right now.</strong></p>
        <p className="muted">Start a model from the Models tab, then talk to it here.</p>
      </div>
    )
  }

  return (
    <div className="playground">
      <div className="section-head play-head">
        <img src={logoFor(recipeID ? [recipeID] : [])} alt="" width="30" height="30" />
        <div>
          <strong>{modelName}</strong>
          <div className="faint">Serving on this Spark, through your own endpoint</div>
        </div>
      </div>
      <div className="chat card" ref={chatRef} aria-live="polite">
        {messages.length === 0 && (
          <p className="chat-hint">Send a message. The reply streams from your Spark, token by token.</p>
        )}
        {messages.map((message, index) => {
          const last = index === messages.length - 1
          if (message.role === 'user') {
            return <div key={index} className="msg user">{message.content}</div>
          }
          const text = visibleText(message.content)
          const waiting = streaming && last && !text
          return (
            <div key={index} className={`msg assistant ${streaming && last ? 'streaming' : ''} ${waiting ? 'waiting' : ''}`}>
              {waiting
                ? <span className="thinking">Thinking</span>
                : <div className="md" dangerouslySetInnerHTML={{ __html: renderMarkdown(text) }} />}
            </div>
          )
        })}
      </div>
      {messages.length > 0 && (
        <div className="chat-foot">
          {stats && <p className="chat-meta">{stats}</p>}
          <span className="spacer" />
          {!streaming && (
            <button className="quiet" onClick={() => { setMessages([]); setStats('') }}>Clear conversation</button>
          )}
        </div>
      )}
      <div className="composer">
        <textarea
          value={draft}
          rows={1}
          placeholder={`Message ${modelName ?? 'your model'}`}
          aria-label={`Message ${modelName ?? 'your model'}`}
          onChange={event => setDraft(event.target.value)}
          onKeyDown={event => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              send()
            }
          }}
        />
        {streaming ? (
          <button className="send" aria-label="Stop generating" title="Stop generating" onClick={() => abortRef.current?.abort()}>
            <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><rect x="1" y="1" width="10" height="10" rx="2.5" fill="currentColor" /></svg>
          </button>
        ) : (
          <button className="send" aria-label="Send" title="Send" onClick={send} disabled={!draft.trim()}>
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 19V5M5 12l7-7 7 7" /></svg>
          </button>
        )}
      </div>
    </div>
  )
}
