import { useEffect, useRef, useState } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { logoFor } from '../catalog'

interface Message {
  role: 'user' | 'assistant'
  content: string
  thinking?: string
}

// Thinking models sometimes leak reasoning into the text stream in two
// shapes: tagged "<think>...</think>" blocks, and bare reasoning ending in
// "</think>" when the chat template already opened the block inside the
// prompt. Both are stripped; only the answer is shown, and only the answer
// is sent back as conversation history. An answer that legitimately contains
// a literal "</think>" would lose its lead-in — vanishingly rare next to
// reasoning leaking on every reply.
const visibleText = (text: string) =>
  text
    .replace(/<think>[\s\S]*?(?:<\/think>|$)/g, '')
    .replace(/^[\s\S]*?<\/think>\s*/, '')
    .replace(/^\s+/, '')

// The reasoning a reply cost, whichever way it arrived: the reasoning_content
// stream when the runtime parses it out, or the inline think block otherwise.
const thinkingText = (message: Message) => {
  const tagged = [...message.content.matchAll(/<think>([\s\S]*?)(?:<\/think>|$)/g)].map(m => m[1]).join('\n')
  const bare = tagged ? '' : (message.content.match(/^([\s\S]*?)<\/think>/)?.[1] ?? '')
  return [message.thinking, tagged || bare].filter(Boolean).join('\n').trim()
}

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
  const [showThinking, setShowThinking] = useState(false)
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
      ...messages.map(message => ({
        role: message.role,
        content: message.role === 'assistant' ? visibleText(message.content) : message.content,
      })),
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
    let completionTokens = 0
    try {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        // No token cap: thinking models spend freely before the visible answer,
        // and a cap cuts replies mid-sentence. The stop button is the limit.
        // The usage frame makes the meter count what the engine generated,
        // thinking included — the same arithmetic as the model benchmark.
        body: JSON.stringify({
          model: modelID, messages: history, stream: true,
          stream_options: { include_usage: true },
        }),
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
            if (chunk.usage?.completion_tokens > 0) completionTokens = chunk.usage.completion_tokens
            const delta = chunk.choices?.[0]?.delta
            const text: string = delta?.content ?? ''
            const thought: string = delta?.reasoning_content ?? delta?.reasoning ?? ''
            if (text || thought) {
              if (!firstToken) firstToken = performance.now()
              if (text) chunks += 1
              setMessages(previous => {
                const next = [...previous]
                const current = next[next.length - 1]
                next[next.length - 1] = {
                  role: 'assistant',
                  content: current.content + text,
                  thinking: thought ? (current.thinking ?? '') + thought : current.thinking,
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
        const total = completionTokens || chunks
        const rate = generation > 0 ? (total / generation).toFixed(1) : 'n/a'
        setStats(`${total} tokens · ${rate} tok/s · first token in ${Math.round(firstToken - started)} ms`)
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
        {/* Only a conversation that has actually produced reasoning offers to
            show it; on a model that never thinks, the control never appears. */}
        {messages.some(message => message.role === 'assistant' && thinkingText(message)) && (
          <label className="think-toggle">
            <input type="checkbox" checked={showThinking} onChange={event => setShowThinking(event.target.checked)} />
            Show thinking
          </label>
        )}
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
          const thought = showThinking ? thinkingText(message) : ''
          const waiting = streaming && last && !text
          return (
            <div key={index} className={`msg assistant ${streaming && last ? 'streaming' : ''} ${waiting ? 'waiting' : ''}`}>
              {thought && <div className="think-stream">{thought}</div>}
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
