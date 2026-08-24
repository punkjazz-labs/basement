import { memo, useEffect, useMemo, useRef, useState, type MouseEvent } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { copyText } from '../api'
import { logoFor } from '../catalog'
import { confirmBox } from '../confirm'
import {
  answerMeta, answerMeter, clearQuestion, composerHeight, hasDelta, jumpInFlight, mergeDelta,
  pinnedToBottom, retryQuestion, sendChoice, shouldReleasePin, splitStreamTail, stoppedMeter,
  tokenMeter, waitMeter, withCaret, NO_DELTA, type PendingDelta,
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
  // Which model the composer was set to when this turn was sent. A
  // conversation can change model between turns, so the turn records the one
  // that answered it rather than the one the picker holds now. Turns from
  // before the console recorded this carry nothing, and a council turn carries
  // the name without an id because a council is more than one model.
  model?: { id?: string; name: string }
  // A stream that failed keeps the text that had already arrived. The reason
  // it stopped goes here, under that text.
  error?: string
}

// A question the owner asked while an answer was arriving. It waits in the
// transcript until the answer ends, then sends itself. The id survives a
// removal in the middle of the line, which an index does not.
interface Queued {
  id: number
  content: string
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
// runs after the sanitize step, which parses the answer and writes it out
// again, so every closing tag here has an opening tag of its own. The opening
// tag is matched with whatever attributes it carries: an answer can write a
// `<pre class="...">` as raw HTML, and a wrap that only knew the bare tag
// would close a div it never opened.
const withCodeCopy = (html: string) =>
  html
    .replace(/<pre(?=[\s>])/g, '<div class="codeblock"><button class="copy" type="button">Copy</button><pre')
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

// A finished answer. The memo holds the text it was given, so the turn does
// not render again while a later turn streams under it, and its nodes are left
// exactly as they are.
const Answer = memo(function Answer({ text }: { text: string }) {
  const html = useMemo(() => renderMarkdown(text), [text])
  return <div className="md" onClick={copyCodeBlock} dangerouslySetInnerHTML={{ __html: html }} />
})

// The part of the answer a blank line has closed.
//
// The memo is what keeps these nodes still. React writes
// dangerouslySetInnerHTML from a fresh object on every render and does not
// compare the string inside it, so the same html would rewrite the whole
// subtree sixty times a second. Equal props stop the render before that, and
// the innerHTML is written again only when a new blank line closes more of the
// answer.
const Closed = memo(function Closed({ html }: { html: string }) {
  return <div className="closed" dangerouslySetInnerHTML={{ __html: html }} />
})

// The answer that is arriving. Everything above the last blank line is closed
// and parses only when a new blank line closes more of it; the tail after it
// is the only part that parses again on the next token. The caret sits at the
// end of that tail, where the next word appears.
//
// So a selection lives as long as the block it is in stays closed. Tokens do
// not take it. The blank line that closes the next block rewrites the closed
// node and takes it, and so does the end of the stream, where the turn is
// rendered once more as a finished answer.
const LiveAnswer = memo(function LiveAnswer({ text }: { text: string }) {
  const { closed, tail } = useMemo(() => splitStreamTail(text), [text])
  const closedHTML = useMemo(() => renderMarkdown(closed, false), [closed])
  const tailHTML = useMemo(() => withCaret(renderMarkdown(tail, false)), [tail])
  return (
    <div className="md">
      {closed !== '' && <Closed html={closedHTML} />}
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
  // The questions typed while an answer was arriving, in the order they were
  // typed. The first of them sends itself when the answer ends.
  const [queue, setQueue] = useState<Queued[]>([])
  const queuedIDRef = useRef(0)
  // The state of the answer, in a few words, for a screen reader: it is
  // answering, the answer is ready, it stopped, it failed, or a question of
  // yours waits for it. The transcript itself says nothing: it changes tens of
  // times a second.
  const [status, setStatus] = useState('')
  // Which answer the owner has just copied, so its button can say so.
  const [copied, setCopied] = useState(-1)
  const abortRef = useRef<AbortController | null>(null)
  // Whether an answer is arriving, without waiting for a render. The queue
  // starts the next answer from an effect, so between that effect and the
  // render it causes, the state still reads false while a stream is running.
  // Anything that would throw work away asks the ref, not the state.
  const streamingRef = useRef(false)
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
  }, [messages, queue])
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
    // The answer records the model the composer resolved now, so the turn can
    // say who wrote it long after the picker has moved on. A council is not one
    // model and has no id of its own; the name is what the reader sees and what
    // a retry compares.
    const wrote = targetName ? { id: council ? undefined : targetID, name: targetName } : undefined
    // Where this answer sits in the transcript. The receipt lands on that turn
    // by its place, not on whatever turn is last when the stream ends: the
    // next question in the queue can start the moment this one finishes.
    const turnAt = prior.length + 1
    setMessages([...prior, { role: 'user', content }, { role: 'assistant', content: '', model: wrote }])
    // A retry drops the turns under the question, so a work panel that was
    // open on one of them has nothing left to show.
    setOpenWork(previous => previous.filter(item => item < prior.length))
    streamingRef.current = true
    setStreaming(true)
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
      // on the screen says so. An answer that broke keeps what it measured
      // before it broke: the numbers belong to the turn that produced them,
      // and a short answer with no numbers says nothing about what arrived.
      const receipt = controller.signal.aborted
        ? stoppedMeter(receiptRef.current || live)
        : receiptRef.current || live
      if (receipt) {
        setMessages(previous => {
          const turn = previous[turnAt]
          if (!turn || turn.role !== 'assistant') return previous
          const next = [...previous]
          next[turnAt] = { ...turn, meter: receipt }
          return next
        })
      }
      // The turn carries its own numbers now, so the live line has nothing
      // left to count.
      setStats('')
      if (!failed) setStatus(controller.signal.aborted ? 'Stopped' : 'Answer ready')
      streamingRef.current = false
      setStreaming(false)
      setCouncilStage(null)
      abortRef.current = null
      // The stop control and the send button trade places, so the caret has
      // nowhere to be until the field takes it back.
      focusDraft(true)
    }
  }

  // Enter, wherever the answer is. A quiet moment sends the question; an
  // answer on its way puts it in the line instead, and the composer is empty
  // either way so the next question can be written at once.
  const send = () => {
    if (!ready) return
    const choice = sendChoice(streaming, queue, draft)
    if (choice === 'nothing') return
    const content = draft.trim()
    setDraft('')
    if (choice === 'queue') {
      setQueue(previous => [...previous, { id: (queuedIDRef.current += 1), content }])
      // The bubble and the empty composer say it on the screen. This is the
      // same news for a reader who hears the console instead.
      setStatus('A question waits')
      // The question the owner just asked comes into view, queued or sent.
      pinToLatest()
      return
    }
    void ask(content, messages)
  }

  // A question waits only until the owner changes their mind about it.
  const removeQueued = (id: number) => setQueue(previous => previous.filter(item => item.id !== id))

  // The line moves when the answer ends, however it ended: finished, stopped
  // or broken. This runs after the render that both cleared `streaming` and
  // put the receipt on the finished turn, so the queued question is asked with
  // a settled transcript above it and the numbers of the turn before it are
  // already where they belong.
  useEffect(() => {
    if (streaming || !ready || queue.length === 0) return
    const [next, ...rest] = queue
    setQueue(rest)
    void ask(next.content, messages)
  }, [streaming, queue, ready, messages])

  // The same question again, in place of the answer it produced. The model
  // reads the history that answer read, so everything under the answer goes
  // with it. On the last answer there is nothing under it and the retry is
  // immediate; anywhere else the owner is asked first, because those turns are
  // work and nothing brings them back.
  const retry = async (index: number) => {
    if (streaming || !ready) return
    let at = index - 1
    while (at >= 0 && messages[at].role !== 'user') at -= 1
    if (at < 0) return
    const under = messages.slice(index + 1).filter(message => message.role === 'user').length
    if (under > 0) {
      // The retry asks whichever model the composer holds now. When that is
      // not the model that wrote the answer, the question says so before the
      // work goes.
      const { ok } = await confirmBox({
        ...retryQuestion(under, targetName, messages[index].model?.name),
        confirmLabel: 'Ask again',
        danger: true,
      })
      if (!ok) return
    }
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
    // The control is hidden while an answer arrives, but the queue can start
    // the next answer between that render and this click, and the owner can
    // take a long time over the dialog. The ref is the state of the stream
    // right now, and an answer on its way is never thrown away under it.
    if (streamingRef.current) return
    // A question still waiting in the line is work as much as one that was
    // answered, so it is counted and it goes with the rest.
    const question = clearQuestion(
      targetName ?? 'your model',
      messages.filter(message => message.role === 'user').length + queue.length,
    )
    const { ok } = await confirmBox({ ...question, confirmLabel: 'Clear conversation', danger: true })
    if (!ok || streamingRef.current) return
    setMessages([])
    setQueue([])
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
        {/* A conversation that is on the screen, or a question still waiting
            for one, is the only thing there is to clear. Stopping the answer
            comes first while one arrives. */}
        {(messages.length > 0 || queue.length > 0) && !streaming && (
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
              // A council turn already names its models in the byline under
              // the answer, so the meta line does not name them again.
              const meta = answerMeta(record ? undefined : message.model?.name, message.meter)
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
                      <div className="byline">
                        <span className="mono">{byline.model}</span>{byline.text}
                        <button
                          onClick={() => setOpenWork(previous =>
                            previous.includes(index) ? previous.filter(item => item !== index) : [...previous, index])}
                        >
                          Show the work
                        </button>
                      </div>
                    )}
                  </div>
                  {/* The panel is a row of the turn rather than a part of the
                      answer, so it can take the width of the transcript while
                      the answer keeps the reading column. */}
                  {byline && openWork.includes(index) && record && work(record)}
                  {/* The tools wait for the answer to finish: text that is still
                      arriving is not text to take away yet. */}
                  {live ? (
                    stats !== '' && (
                      <div className="turn-foot"><span className="chat-meta live">{stats}</span></div>
                    )
                  ) : (text || message.error || meta) ? (
                    <div className="turn-foot">
                      {text && (
                        <button className="copy-btn" onClick={() => copyAnswer(index, text)}>
                          {copied === index ? 'Copied' : COPY_LABEL}
                        </button>
                      )}
                      <button className="copy-btn" onClick={() => void retry(index)} disabled={streaming}>Retry</button>
                      {meta && <span className="chat-meta">{meta}</span>}
                    </div>
                  ) : null}
                </div>
              )
            })}
            {/* The questions that wait for the answer. Each one is the bubble
                it will be when it sends, with the word that says it has not
                gone yet and the one control that takes it back. */}
            {queue.map(item => (
              <div key={`queued-${item.id}`} className="turn user">
                <div className="msg user">{item.content}</div>
                <div className="turn-foot">
                  <span className="queued-note">Waits for the answer</span>
                  <button className="copy-btn" onClick={() => removeQueued(item.id)}>Remove</button>
                </div>
              </div>
            ))}
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
