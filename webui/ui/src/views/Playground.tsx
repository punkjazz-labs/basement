import { memo, useEffect, useMemo, useRef, useState, type MouseEvent } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { copyText } from '../api'
import { logoFor } from '../catalog'
import { confirmBox } from '../confirm'
import {
  answerMeter, clearQuestion, composerHeight, hasDelta, jumpInFlight, mergeDelta, pinnedToBottom,
  shouldReleasePin, splitStreamTail, stoppedMeter, tokenMeter, waitMeter, withCaret, NO_DELTA,
  type PendingDelta,
} from '../chat'
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
  // What this answer cost and how fast it came. The numbers belong to the
  // turn that produced them, so an earlier turn keeps its own receipt.
  meter?: string
  // A stream that failed keeps the text that had already arrived. The reason
  // it stopped goes here, under that text.
  error?: string
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

// Every code block carries the Copy that Connect gives a snippet. The wrap
// runs after the sanitize step, and marked escapes every angle bracket inside
// a block, so no answer can write these tags itself.
const withCodeCopy = (html: string) =>
  html
    .replaceAll('<pre>', '<div class="codeblock"><button class="copy" type="button">Copy</button><pre>')
    .replaceAll('</pre>', '</pre></div>')

// Models speak markdown; render it, sanitized, so replies read like answers
// rather than markup.
const renderMarkdown = (text: string, tools = true) => {
  const html = DOMPurify.sanitize(marked.parse(text, { async: false }) as string)
  return tools ? withCodeCopy(html) : html
}

// The Copy buttons live inside markup the parser wrote, so one handler on the
// answer serves all of them. It takes the code the reader sees, which is the
// source the model wrote: the parser escaped it and the text node holds it
// back in its first form.
const COPY_LABEL = 'Copy'
const copyCodeBlock = (event: MouseEvent<HTMLDivElement>) => {
  const button = (event.target as HTMLElement).closest('.codeblock .copy')
  if (!button) return
  const code = button.parentElement?.querySelector('pre')?.textContent ?? ''
  if (!code) return
  void copyText(code)
  button.textContent = 'Copied'
  window.setTimeout(() => { button.textContent = COPY_LABEL }, 1600)
}

// A finished answer parses once. React leaves an html string it has already
// written alone, so the nodes hold still while a later turn streams under
// them and a selection inside this one survives.
const Answer = memo(function Answer({ text }: { text: string }) {
  const html = useMemo(() => renderMarkdown(text), [text])
  return <div className="md" onClick={copyCodeBlock} dangerouslySetInnerHTML={{ __html: html }} />
})

// The answer that is arriving. Everything above the last blank line is closed
// and parses only when a new blank line closes more of it; the tail after it
// is the only part that parses again on the next token. The caret sits at the
// end of that tail, where the next word appears.
const LiveAnswer = memo(function LiveAnswer({ text }: { text: string }) {
  const { closed, tail } = useMemo(() => splitStreamTail(text), [text])
  const closedHTML = useMemo(() => renderMarkdown(closed, false), [closed])
  const tailHTML = useMemo(() => withCaret(renderMarkdown(tail, false)), [tail])
  return (
    <div className="md">
      {closed !== '' && <div className="closed" dangerouslySetInnerHTML={{ __html: closedHTML }} />}
      <div className="tail" dangerouslySetInnerHTML={{ __html: tailHTML }} />
    </div>
  )
})

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
  // Enter during a stream has no effect. It says so, once, until the answer
  // ends.
  const [busyNote, setBusyNote] = useState(false)
  // The state of the answer, in one word, for a screen reader. The transcript
  // itself says nothing: it changes tens of times a second.
  const [status, setStatus] = useState('')
  // Which answer the owner has just copied, so its button can say so.
  const [copied, setCopied] = useState(-1)
  const abortRef = useRef<AbortController | null>(null)
  const chatRef = useRef<HTMLDivElement>(null)
  const draftRef = useRef<HTMLTextAreaElement>(null)
  // Whether the transcript follows the answer. The ref decides that on every
  // token, without a render; the state only drives the jump control, which
  // changes when the reader scrolls, not when a token arrives.
  const pinnedRef = useRef(true)
  const [pinned, setPinned] = useState(true)
  // A smooth jump takes a moment, and the answer grows while it travels.
  // Scroll positions on the way there do not release the pin.
  const jumpUntilRef = useRef(0)
  // What the meter in the turn counts while the answer is on its way: the
  // wait before the first token, then the tokens themselves.
  const meterRef = useRef({ started: 0, firstToken: 0, tokens: 0 })
  const tickRef = useRef<number | null>(null)
  // The receipt the finished stream measured, held until the turn can take it.
  const receiptRef = useRef('')
  // The tokens that arrived since the last repaint, and the frame that will
  // apply them.
  const pendingRef = useRef<PendingDelta>(NO_DELTA)
  const frameRef = useRef<number | null>(null)

  // The playground stays mounted behind the other tabs, so every reach for
  // the field or for the Escape key asks first whether it is on screen.
  const onScreen = () => draftRef.current?.offsetParent != null
  // preventScroll where the caret returns on its own, at the end of an
  // answer: taking the field must not move the page the owner is reading.
  const focusDraft = (preventScroll = false) => {
    if (onScreen()) draftRef.current?.focus({ preventScroll })
  }

  // The console honours the reduced-motion setting everywhere else, so a jump
  // under that setting arrives at once instead of gliding.
  const reducedMotion = () =>
    window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false

  const toLatest = (smooth: boolean) => {
    const chat = chatRef.current
    if (!chat) return
    // 'auto' for a token: a smooth scroll on every token fights itself.
    chat.scrollTo({ top: chat.scrollHeight, behavior: smooth && !reducedMotion() ? 'smooth' : 'auto' })
  }

  const pinToLatest = () => {
    pinnedRef.current = true
    setPinned(true)
  }

  const onChatScroll = () => {
    const chat = chatRef.current
    if (!chat) return
    const atEnd = pinnedToBottom(chat)
    // A jump still on its way reports the positions it passes through. They
    // are the jump's, not the reader's, so they change nothing.
    if (!atEnd && !shouldReleasePin(atEnd, performance.now(), jumpUntilRef.current)) return
    jumpUntilRef.current = 0
    pinnedRef.current = atEnd
    setPinned(atEnd)
  }

  // The wheel, a finger and the keys are the reader, and the reader outranks
  // a jump in flight. The trip loses its claim the moment one of them
  // arrives, so the next position is read as the reader's own.
  const endJump = () => { jumpUntilRef.current = 0 }

  const jumpToLatest = () => {
    pinToLatest()
    jumpUntilRef.current = reducedMotion() ? 0 : performance.now() + 900
    toLatest(true)
    focusDraft()
  }

  // The box grows with the text it holds, so a long question is never written
  // blind. An empty draft returns it to one line.
  const resizeDraft = () => {
    const field = draftRef.current
    if (!field) return
    field.style.height = 'auto'
    field.style.height = `${composerHeight(field.scrollHeight)}px`
  }

  // The answer moves the transcript only while the reader is already at the
  // end of it. Scroll up during a stream and the view stays where it was put.
  // A jump that is still travelling is given the new end instead: the token
  // and the trip then go to the same place.
  useEffect(() => {
    if (pinnedRef.current) toLatest(jumpInFlight(performance.now(), jumpUntilRef.current))
  }, [messages])
  // A draft that changes without a keystroke, a send that empties the box.
  useEffect(resizeDraft, [draft])
  // Escape stops the answer wherever the caret is, for as long as one is
  // arriving.
  useEffect(() => {
    if (!streaming) return
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && onScreen()) abortRef.current?.abort()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [streaming])
  useEffect(() => { if (!streaming) setBusyNote(false) }, [streaming])
  useEffect(() => () => {
    abortRef.current?.abort()
    if (tickRef.current !== null) window.clearInterval(tickRef.current)
    if (frameRef.current !== null) window.cancelAnimationFrame(frameRef.current)
  }, [])

  const offered = councilOffered(chatModels)
  const council = offered && councilWanted
  const selected =
    chatModels.find(model => model.id === chosenID) ??
    chatModels.find(model => model.id === modelID) ??
    chatModels[0]
  const targetID = selected?.id ?? modelID
  const targetName = council ? 'Council' : selected?.name ?? modelName

  // Everything that arrived since the last frame, applied as one change.
  const flushDeltas = () => {
    frameRef.current = null
    const pending = pendingRef.current
    if (!hasDelta(pending)) return
    pendingRef.current = NO_DELTA
    setMessages(previous => {
      const next = [...previous]
      const current = next[next.length - 1]
      if (!current) return previous
      next[next.length - 1] = {
        ...current,
        role: 'assistant',
        content: pending.replace ? pending.text : current.content + pending.text,
        thinking: pending.replace
          ? (pending.thinking || undefined)
          : pending.thinking ? (current.thinking ?? '') + pending.thinking : current.thinking,
      }
      return next
    })
  }

  // A chunk of the stream is much smaller than one frame, and parsing the
  // answer again for each of them is what makes a long answer stutter. The
  // chunks are added up here and the screen is written about sixty times a
  // second, whatever the token rate.
  const queueDelta = (delta: CouncilDelta) => {
    pendingRef.current = mergeDelta(pendingRef.current, delta)
    if (frameRef.current === null) frameRef.current = window.requestAnimationFrame(flushDeltas)
  }

  // The last tokens of an answer must land even when no frame follows them,
  // and a hidden window gives no frames at all.
  const flushNow = () => {
    if (frameRef.current !== null) {
      window.cancelAnimationFrame(frameRef.current)
      frameRef.current = null
    }
    flushDeltas()
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
            if (text) {
              chunks += 1
              // The live meter counts the answer, not the reasoning: while a
              // model only thinks, the clock is the honest number.
              if (!meterRef.current.firstToken) meterRef.current.firstToken = performance.now()
              meterRef.current.tokens = chunks
            }
            answer += text
            onDelta({ text, thinking: thought })
          }
        } catch {
          /* partial chunk; the buffer catches it next round */
        }
      }
    }
    if (firstToken) {
      // The receipt waits here for the turn that produced it.
      receiptRef.current =
        answerMeter(completionTokens || chunks, performance.now() - firstToken, firstToken - started)
    }
    return visibleText(answer)
  }

  // Every 500ms while the answer is on its way: the wait in seconds until the
  // first token, the count and the rate after it.
  const tickMeter = () => {
    const meter = meterRef.current
    setStats(meter.firstToken
      ? tokenMeter(meter.tokens, performance.now() - meter.firstToken)
      : waitMeter(performance.now() - meter.started))
  }

  // One question, and the turns that come before it. A send keeps every turn
  // there is; a retry keeps the turns above the question it asks again.
  const ask = async (content: string, prior: Message[]) => {
    if (!content || streaming || !ready) return
    const history = councilHistory(prior.map(message => ({
      role: message.role,
      content: message.role === 'assistant' ? visibleText(message.content) : message.content,
      council: message.council,
    })))
    setMessages([...prior, { role: 'user', content }, { role: 'assistant', content: '' }])
    // A retry drops the turns under the question, so a work panel that was
    // open on one of them has nothing left to show.
    setOpenWork(previous => previous.filter(item => item < prior.length))
    setStreaming(true)
    setBusyNote(false)
    setStatus('Answering')
    receiptRef.current = ''
    pendingRef.current = NO_DELTA
    // A question the owner just sent always comes into view, and the
    // transcript follows the answer to it again.
    pinToLatest()
    // The click path leaves the caret on a button that is about to go away.
    focusDraft()
    meterRef.current = { started: performance.now(), firstToken: 0, tokens: 0 }
    setStats(waitMeter(0))
    tickRef.current = window.setInterval(tickMeter, 500)
    const controller = new AbortController()
    abortRef.current = controller
    let failed = false
    try {
      if (council) {
        const outcome = await runCouncil(
          { question: content, history, models: chatModels, seed: seedFrom(content, prior.length) },
          {
            answer: request => completeReply(request, controller.signal),
            stream: (request, onDelta) => streamReply(request, controller.signal, onDelta),
          },
          { onStage: setCouncilStage, onDelta: queueDelta },
        )
        if (outcome.council) {
          const record = outcome.council
          flushNow()
          setMessages(previous => {
            const next = [...previous]
            next[next.length - 1] = { ...next[next.length - 1], council: record }
            return next
          })
        }
      } else {
        await streamReply({ model: targetID ?? '', messages: [...history, { role: 'user', content }] },
          controller.signal, queueDelta)
      }
    } catch (problem) {
      if (!controller.signal.aborted && (problem as Error).name !== 'AbortError') {
        failed = true
        setStatus('The request failed')
        // The text that already arrived is the part the owner most wants when
        // a long answer breaks, so the turn keeps it and the reason goes
        // under it.
        flushNow()
        setMessages(previous => {
          const next = [...previous]
          next[next.length - 1] = {
            ...next[next.length - 1],
            role: 'assistant',
            error: problem instanceof Error ? problem.message : 'The request failed.',
          }
          return next
        })
      }
    } finally {
      if (tickRef.current !== null) {
        window.clearInterval(tickRef.current)
        tickRef.current = null
      }
      // The tokens of the last frame land even when no frame follows them.
      flushNow()
      const meter = meterRef.current
      const live = meter.firstToken ? tokenMeter(meter.tokens, performance.now() - meter.firstToken) : ''
      // A stopped answer is short because the owner stopped it. Nothing else
      // on the screen says so.
      const receipt = controller.signal.aborted
        ? stoppedMeter(receiptRef.current || live)
        : failed ? '' : receiptRef.current
      if (receipt) {
        setMessages(previous => {
          const next = [...previous]
          next[next.length - 1] = { ...next[next.length - 1], meter: receipt }
          return next
        })
      }
      // The turn carries its own numbers now, so the live line has nothing
      // left to count.
      setStats('')
      if (!failed) setStatus(controller.signal.aborted ? 'Stopped' : 'Answer ready')
      setStreaming(false)
      setCouncilStage(null)
      abortRef.current = null
      // The stop control and the send button trade places, so the caret has
      // nowhere to be until the field takes it back.
      focusDraft(true)
    }
  }

  const send = () => {
    const content = draft.trim()
    if (!content || streaming || !ready) return
    setDraft('')
    void ask(content, messages)
  }

  // The same question again, in place of the answer it produced. The turns
  // under that answer went with it, so the model reads the same history the
  // first attempt read.
  const retry = (index: number) => {
    if (streaming || !ready) return
    let at = index - 1
    while (at >= 0 && messages[at].role !== 'user') at -= 1
    if (at < 0) return
    void ask(messages[at].content, messages.slice(0, at))
  }

  const copyAnswer = async (index: number, text: string) => {
    await copyText(text)
    setCopied(index)
    window.setTimeout(() => setCopied(-1), 1600)
  }

  // A long conversation is work, and every other destructive action in this
  // console asks first.
  const clearConversation = async () => {
    const question = clearQuestion(
      targetName ?? 'your model',
      messages.filter(message => message.role === 'user').length,
    )
    const { ok } = await confirmBox({ ...question, confirmLabel: 'Clear conversation', danger: true })
    if (!ok) return
    setMessages([])
    setStats('')
    setStatus('')
    setOpenWork([])
    pinToLatest()
    focusDraft()
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
          <div className="faint">Serving on this Spark</div>
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
        {/* A conversation that is on the screen is the only one there is to
            clear, and stopping the answer comes first while one arrives. */}
        {messages.length > 0 && !streaming && (
          <button className="quiet" onClick={clearConversation}>Clear conversation</button>
        )}
      </div>
      <div className="chatpane">
        <div
          className="chat-scroll"
          ref={chatRef}
          tabIndex={0}
          role="log"
          aria-label="Conversation"
          /* The answer changes tens of times a second. A live region here reads
             the whole conversation again on every token, so the state words
             under the composer carry the news instead. */
          aria-live="off"
          onScroll={onChatScroll}
          onWheel={endJump}
          onTouchStart={endJump}
          onKeyDown={endJump}
        >
          <div className="thread">
            {messages.length === 0 && (
              <p className="chat-hint">
                {council ? 'Send a message to the council.' : `Send a message to ${targetName ?? 'your model'}.`}
              </p>
            )}
            {messages.map((message, index) => {
              const last = index === messages.length - 1
              if (message.role === 'user') {
                return <div key={index} className="turn user"><div className="msg user">{message.content}</div></div>
              }
              const text = visibleText(message.content)
              const thought = showThinking ? thinkingText(message) : ''
              const live = streaming && last
              const waiting = live && !text
              const record = message.council
              const byline = record ? councilByline(record) : null
              return (
                <div key={index} className="turn">
                  <div className={`msg assistant ${live ? 'streaming' : ''}`}>
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
                      <div className="md">
                        <p><span className="thinking">Thinking</span><span className="caret" aria-hidden="true">▍</span></p>
                      </div>
                    ) : live ? (
                      <LiveAnswer text={text} />
                    ) : (
                      <Answer text={text} />
                    )}
                    {/* The stream broke. What arrived stays, and the reason for the
                        short answer reads under it. */}
                    {message.error && <p className="chat-error error-text">{message.error}</p>}
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
                  {/* The tools wait for the answer to finish: text that is still
                      arriving is not text to take away yet. */}
                  {live ? (
                    stats !== '' && (
                      <div className="turn-foot"><span className="chat-meta live">{stats}</span></div>
                    )
                  ) : (text || message.error || message.meter) ? (
                    <div className="turn-foot">
                      {text && (
                        <button className="copy-btn" onClick={() => copyAnswer(index, text)}>
                          {copied === index ? 'Copied' : COPY_LABEL}
                        </button>
                      )}
                      <button className="copy-btn" onClick={() => retry(index)} disabled={streaming}>Retry</button>
                      {message.meter && <span className="chat-meta">{message.meter}</span>}
                    </div>
                  ) : null}
                </div>
              )
            })}
          </div>
        </div>
        <div className="composer-dock">
          <div className="dock-inner">
            {/* Shown only while the reader is away from the end of the
                transcript, which is the only time it has anything to do. */}
            {!pinned && (
              <button className="jump" onClick={jumpToLatest}>
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 5v14M19 12l-7 7-7-7" /></svg>
                Jump to latest
              </button>
            )}
            <div className="composer">
              <textarea
                ref={draftRef}
                value={draft}
                rows={1}
                placeholder={council ? 'Ask the council' : `Message ${targetName ?? 'your model'}`}
                aria-label={council ? 'Ask the council' : `Message ${targetName ?? 'your model'}`}
                onChange={event => {
                  setDraft(event.target.value)
                  resizeDraft()
                }}
                onKeyDown={event => {
                  if (event.key !== 'Enter' || event.shiftKey) return
                  // Enter confirms the candidate word in Japanese, Chinese and
                  // Korean input. Sending there sends half a sentence.
                  if (event.nativeEvent.isComposing) return
                  event.preventDefault()
                  if (streaming) {
                    setBusyNote(true)
                    return
                  }
                  send()
                }}
              />
              {streaming ? (
                /* Orange marks the one thing to do next, and stopping is never
                   that. Stop keeps its own shape, its own place and its name. */
                <button className="stop" aria-label="Stop generating" title="Stop generating" onClick={() => abortRef.current?.abort()}>
                  <i aria-hidden="true" />Stop
                </button>
              ) : (
                <button className="send" aria-label="Send" title="Send" onClick={send} disabled={!draft.trim()}>
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 19V5M5 12l7-7 7 7" /></svg>
                </button>
              )}
            </div>
            {busyNote && <p className="composer-note">The model is answering. Press Stop first.</p>}
            <p className="composer-hint">
              <b>Enter</b> sends. <b>Shift</b> and <b>Enter</b> add a line. <b>Esc</b> stops the answer.
            </p>
          </div>
        </div>
      </div>
      {/* The state of the answer in one word, for a screen reader, where the
          transcript itself now says nothing. */}
      <p className="sr-live" role="status" aria-live="polite">{status}</p>
    </div>
  )
}
