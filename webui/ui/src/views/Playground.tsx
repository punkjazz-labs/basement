import { useEffect, useRef, useState } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { logoFor } from '../catalog'
import {
  COUNCIL_STAGES, councilByline, councilHistory, councilOffered, runCouncil, seedFrom, stageState,
  type ChatRequest, type CouncilDelta, type CouncilModel, type CouncilRecord, type CouncilStage,
} from '../council'

interface Message {
  role: 'user' | 'assistant'
  content: string
  thinking?: string
  // Present only on an answer a council turn produced: who wrote it, from
  // which answers, and how the models ranked them.
  council?: CouncilRecord
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
export default function Playground({ ready, modelID, modelName, recipeID, chatModels }: {
  ready: boolean
  modelID?: string
  modelName?: string
  recipeID?: string
  // Every text model serving on this Spark right now. Two or more of them is
  // the whole condition for the council being offered at all.
  chatModels: CouncilModel[]
}) {
  const [messages, setMessages] = useState<Message[]>([])
  const [draft, setDraft] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [stats, setStats] = useState('')
  const [showThinking, setShowThinking] = useState(false)
  // Which model the composer talks to, and whether the council was asked for.
  // The wish outlives a model going away: what it produces is decided at send
  // time against the models actually serving then.
  const [chosenID, setChosenID] = useState('')
  const [councilWanted, setCouncilWanted] = useState(false)
  const [councilStage, setCouncilStage] = useState<CouncilStage | null>(null)
  const [openWork, setOpenWork] = useState<number[]>([])
  const abortRef = useRef<AbortController | null>(null)
  const chatRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    chatRef.current?.scrollTo({ top: chatRef.current.scrollHeight })
  }, [messages])
  useEffect(() => () => abortRef.current?.abort(), [])

  const offered = councilOffered(chatModels)
  const council = offered && councilWanted
  const selected =
    chatModels.find(model => model.id === chosenID) ??
    chatModels.find(model => model.id === modelID) ??
    chatModels[0]
  const targetID = selected?.id ?? modelID
  const targetName = council ? 'Council' : selected?.name ?? modelName

  const applyDelta = (delta: CouncilDelta) => {
    setMessages(previous => {
      const next = [...previous]
      const current = next[next.length - 1]
      const thought = delta.thinking ?? ''
      next[next.length - 1] = {
        ...current,
        role: 'assistant',
        content: delta.replace ? (delta.text ?? '') : current.content + (delta.text ?? ''),
        thinking: delta.replace
          ? (thought || undefined)
          : thought ? (current.thinking ?? '') + thought : current.thinking,
      }
      return next
    })
  }

  // One complete answer, nothing shown while it is produced: the council's
  // first two stages are working material, not something to watch.
  const completeReply = async (request: ChatRequest, signal: AbortSignal): Promise<string> => {
    const response = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...request, stream: false }),
      signal,
    })
    if (!response.ok) {
      const body = await response.json().catch(() => null)
      throw new Error(body?.error?.message ?? `The model returned ${response.status}`)
    }
    const body = await response.json()
    return visibleText(body?.choices?.[0]?.message?.content ?? '')
  }

  // The reply, token by token, and the speed it arrived at. Both the plain
  // turn and the council's final answer come through here, so there is one
  // stream parser and one set of numbers.
  const streamReply = async (
    request: ChatRequest,
    signal: AbortSignal,
    onDelta: (delta: CouncilDelta) => void,
  ): Promise<string> => {
    const started = performance.now()
    let firstToken = 0
    let chunks = 0
    let completionTokens = 0
    let answer = ''
    // No token cap: thinking models spend freely before the visible answer,
    // and a cap cuts replies mid-sentence. The stop button is the limit.
    // The usage frame makes the meter count what the engine generated,
    // thinking included — the same arithmetic as the model benchmark.
    const response = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...request, stream: true, stream_options: { include_usage: true } }),
      signal,
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
            answer += text
            onDelta({ text, thinking: thought })
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
    return visibleText(answer)
  }

  const send = async () => {
    const content = draft.trim()
    if (!content || streaming || !ready) return
    const history = councilHistory(messages.map(message => ({
      role: message.role,
      content: message.role === 'assistant' ? visibleText(message.content) : message.content,
      council: message.council,
    })))
    setMessages(previous => [...previous, { role: 'user', content }, { role: 'assistant', content: '' }])
    setDraft('')
    setStreaming(true)
    setStats('')
    const controller = new AbortController()
    abortRef.current = controller
    try {
      if (council) {
        const outcome = await runCouncil(
          { question: content, history, models: chatModels, seed: seedFrom(content, messages.length) },
          {
            answer: request => completeReply(request, controller.signal),
            stream: (request, onDelta) => streamReply(request, controller.signal, onDelta),
          },
          { onStage: setCouncilStage, onDelta: applyDelta },
        )
        if (outcome.council) {
          const record = outcome.council
          setMessages(previous => {
            const next = [...previous]
            next[next.length - 1] = { ...next[next.length - 1], council: record }
            return next
          })
        }
      } else {
        await streamReply({ model: targetID ?? '', messages: [...history, { role: 'user', content }] },
          controller.signal, applyDelta)
      }
    } catch (problem) {
      if (!controller.signal.aborted && (problem as Error).name !== 'AbortError') {
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
      setCouncilStage(null)
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

  const work = (record: CouncilRecord) => (
    <div className="work">
      <div className="work-head">Answers <span className="anon">reviewed without names</span></div>
      <div className="drafts">
        {record.answers.map((answer, index) => (
          <div className="draft" key={index}>
            <div className="who">
              {answer.model}
              {/* The green first place is a reading of the rankings, so it
                  appears only when a ranking was actually readable. */}
              {record.rankings.length > 0 && answer.model === record.winner && <span className="win">ranked 1st</span>}
            </div>
            <p>{answer.text}</p>
          </div>
        ))}
      </div>
      {record.rankings.length > 0 && (
        <div className="ranks">
          {record.rankings.map((ranking, index) => (
            <div className="rank" key={index}>
              <span className="who">{ranking.reviewer}</span>
              <span className="order">
                {ranking.order.map((name, place) => <span key={place}>{place + 1} {name}</span>)}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  )

  return (
    <div className="playground">
      <div className="section-head play-head">
        <img src={logoFor(recipeID ? [recipeID] : [])} alt="" width="30" height="30" />
        <div>
          <strong>{targetName}</strong>
          <div className="faint">Serving on this Spark, through your own endpoint</div>
        </div>
        {/* The picker exists only where there is something to pick: two or
            more text models serving is what puts the council on offer. */}
        {offered && (
          <div className="modelsel" role="group" aria-label="Which model answers">
            {chatModels.map(model => (
              <button
                key={model.id}
                className={!council && selected?.id === model.id ? 'on' : ''}
                onClick={() => { setCouncilWanted(false); setChosenID(model.id) }}
              >
                {model.name}
              </button>
            ))}
            <button className={council ? 'on' : ''} onClick={() => setCouncilWanted(true)}>
              Council<span className="n">{chatModels.length}</span>
            </button>
          </div>
        )}
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
          const record = message.council
          const byline = record ? councilByline(record) : null
          return (
            <div key={index} className={`msg assistant ${streaming && last ? 'streaming' : ''} ${waiting ? 'waiting' : ''}`}>
              {thought && <div className="think-stream">{thought}</div>}
              {waiting && councilStage ? (
                <div className="progress">
                  <span className="spin" aria-hidden="true" />
                  <span className="stage">
                    {COUNCIL_STAGES.map(stage => (
                      <span key={stage} className={`s ${stageState(stage, councilStage)}`}>{stage}</span>
                    ))}
                  </span>
                </div>
              ) : waiting ? (
                <span className="thinking">Thinking</span>
              ) : (
                <div className="md" dangerouslySetInnerHTML={{ __html: renderMarkdown(text) }} />
              )}
              {byline && (
                <>
                  <div className="byline">
                    <span className="mono">{byline.model}</span>{byline.text}
                    <button
                      onClick={() => setOpenWork(previous =>
                        previous.includes(index) ? previous.filter(item => item !== index) : [...previous, index])}
                    >
                      Show the work
                    </button>
                  </div>
                  {openWork.includes(index) && record && work(record)}
                </>
              )}
            </div>
          )
        })}
      </div>
      {messages.length > 0 && (
        <div className="chat-foot">
          {stats && <p className="chat-meta">{stats}</p>}
          <span className="spacer" />
          {!streaming && (
            <button className="quiet" onClick={() => { setMessages([]); setStats(''); setOpenWork([]) }}>
              Clear conversation
            </button>
          )}
        </div>
      )}
      <div className="composer">
        <textarea
          value={draft}
          rows={1}
          placeholder={council ? 'Ask the council' : `Message ${targetName ?? 'your model'}`}
          aria-label={council ? 'Ask the council' : `Message ${targetName ?? 'your model'}`}
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
