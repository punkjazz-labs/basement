// Pure rules for the chat surface: when the transcript follows the answer,
// how tall the composer grows, and the words in the meter under it. No DOM,
// no React and no fetch, so each rule can be tested on its own.

// The reader is pinned to the end of the transcript while the gap under the
// last line is this small. 32px is about one line and a half: one notch of
// the wheel releases the pin, a rounding error does not.
export const PIN_GAP = 32

// Eight lines at 13.5px and 1.55 line height, plus the padding of the field.
// Past this the box scrolls, so the composer never eats the transcript.
export const COMPOSER_MAX_HEIGHT = 176

export interface ScrollPosition {
  scrollHeight: number
  scrollTop: number
  clientHeight: number
}

// True while the reader sits at the end of the transcript. Only then may a
// new token pull the view down. Content shorter than the box counts as the
// end, so a short conversation never shows the jump control.
export function pinnedToBottom(position: ScrollPosition, gap = PIN_GAP): boolean {
  return position.scrollHeight - position.scrollTop - position.clientHeight <= gap
}

// Whether a scroll position releases the pin.
//
// A position away from the end normally releases it: the reader has gone to
// read something. The one exception is a jump that is still on its way to the
// end, because a smooth scroll reports every position it passes through and
// none of those positions are the reader's. `jumpUntil` is when that trip
// gives up its claim. It is zero while no jump is travelling, and the caller
// zeroes it the moment the reader touches the wheel or the screen, so a real
// scroll always wins over a jump in flight.
export function shouldReleasePin(atEnd: boolean, now: number, jumpUntil: number): boolean {
  if (atEnd) return false
  return now >= jumpUntil
}

// Whether a jump is still travelling. The answer grows under a jump in
// flight, so the trip must be told the new end each time rather than raced:
// an instant scroll under a running animation loses to it and lands short of
// the last line.
export function jumpInFlight(now: number, jumpUntil: number): boolean {
  return now < jumpUntil
}

// The height the composer takes for the text it holds.
export function composerHeight(contentHeight: number, max = COMPOSER_MAX_HEIGHT): number {
  return Math.min(contentHeight, max)
}

// What the meter says before the first token of the answer. A large model on
// a Spark can think for a long time, and without a clock a slow answer and a
// dead connection look the same.
export function waitMeter(elapsedMs: number): string {
  return `${(Math.max(0, elapsedMs) / 1000).toFixed(1)} s · no token yet`
}

// Tokens and speed. The rate counts from the first token, so the wait before
// it does not make a fast model look slow. Before there is a measurable
// interval there is no rate to state, only the count.
export function tokenMeter(tokens: number, generationMs: number): string {
  const seconds = generationMs / 1000
  if (seconds <= 0) return `${tokens} tokens`
  return `${tokens} tokens · ${(tokens / seconds).toFixed(1)} tok/s`
}

// The receipt for a finished answer: what it cost, how fast it came, and how
// long the model thought before it started.
export function answerMeter(tokens: number, generationMs: number, firstTokenMs: number): string {
  return `${tokenMeter(tokens, generationMs)} · first token in ${Math.round(firstTokenMs)} ms`
}

// An answer can be short because the owner stopped it, not because the model
// had no more to say. The meter says which.
export function stoppedMeter(meter: string): string {
  return meter ? `${meter} · Stopped` : 'Stopped'
}

// The line under a finished answer: who wrote it, then what it cost. The
// model leads because the composer can change model between turns, and a
// receipt says nothing about speed until you know whose speed it is. A turn
// that carries no name, which is every turn from before the console recorded
// one, reads exactly as it did.
export function answerMeta(model: string | undefined, meter: string | undefined): string {
  return [model, meter].filter(Boolean).join(' · ')
}

// ---- The queue ------------------------------------------------------------

// What Enter does with the draft.
//
// A question typed while the model answers is neither lost nor allowed to cut
// the answer short: it waits in the transcript and sends itself when the
// answer ends. A question typed while other questions already wait goes behind
// them, so the answers arrive in the order the questions were asked. The queue
// is passed whole rather than as a count so the caller can hold whatever shape
// of item it needs.
export type SendChoice = 'nothing' | 'queue' | 'send'

export function sendChoice(
  streaming: boolean,
  queue: readonly unknown[],
  draft: string,
): SendChoice {
  if (draft.trim() === '') return 'nothing'
  return streaming || queue.length > 0 ? 'queue' : 'send'
}

// ---- The answer while it arrives ------------------------------------------

export interface StreamSplit {
  // Everything up to and including the last blank line. Markdown closes a
  // block at a blank line, so the text of this part cannot change again. It
  // parses once for each blank line that arrives, not once for each token,
  // and the caller keeps its nodes between those points.
  closed: string
  // What arrived after that line. This part parses again on every token, so
  // the tokens alone never touch the text above it. A selection in the closed
  // part lives until the next blank line closes another block.
  tail: string
}

// A fence line: three or more backticks or three or more tildes, indented by
// no more than three spaces, and whatever else the line carries. Markdown
// allows both characters, and a fence closes only on the character that opened
// it. The rest of the line decides opener from closer.
const FENCE = /^ {0,3}(`{3,}|~{3,})([^\n\r]*)/gm

// A blank line, whichever line ending the runtime writes. A model that answers
// with CRLF is answering the same markdown, and a run of blank lines closes
// the block at the last of them, exactly where a single blank line closes it.
const BLANK_LINE = /\r?\n(?:[ \t]*\r?\n)+/g

// Whether a fence is still open at the end of this text. Inside an open fence
// a blank line closes nothing, so a cut there would leave both halves parsing
// as broken markdown.
//
// A line closes the fence only when it is the opening character, run at least
// as long as the run that opened the fence, and nothing after it but spaces or
// tabs. That last rule matters: an answer about markdown writes a "```js" line
// inside its block, and the renderer reads that line as content because a
// closing fence carries no information string. A splitter that took it for the
// end of the block would cut there and leave a code block running away down
// the closed part until the real closing line arrived. An opening line may
// carry a name, so only the closer is held to it.
function fenceOpen(text: string): boolean {
  let open = ''
  let length = 0
  for (const match of text.matchAll(FENCE)) {
    const marker = match[1]
    const rest = match[2]
    if (open === '') {
      open = marker[0]
      length = marker.length
      continue
    }
    if (marker[0] === open && marker.length >= length && /^[ \t]*$/.test(rest)) {
      open = ''
      length = 0
    }
  }
  return open !== ''
}

export function splitStreamTail(text: string): StreamSplit {
  // Every place a blank line could cut the answer, in the order they arrived.
  const cuts: number[] = []
  for (const match of text.matchAll(BLANK_LINE)) {
    const at = match.index ?? 0
    // A blank line at the very start closes nothing worth keeping.
    if (at <= 0) continue
    cuts.push(at + match[0].length)
  }
  // The last cut that leaves no fence open. An open fence pushes the cut
  // further up the answer, and a fence that opened and has not closed yet
  // keeps the whole answer in the tail until it does.
  for (let index = cuts.length - 1; index >= 0; index -= 1) {
    const closed = text.slice(0, cuts[index])
    if (!fenceOpen(closed)) return { closed, tail: text.slice(cuts[index]) }
  }
  return { closed: '', tail: text }
}

// The caret marks the place the next word appears, so it goes inside the last
// block that holds text. Put after the answer it reads as a finished answer.
export const CARET = '<span class="caret" aria-hidden="true">▍</span>'

// The blocks that can hold text on their last line. The one that closes last
// is the one the caret belongs to.
const CARET_HOSTS = ['</p>', '</li>', '</h1>', '</h2>', '</h3>', '</h4>', '</h5>', '</h6>',
  '</blockquote>', '</td>', '</th>', '</code>']

export function withCaret(html: string, caret = CARET): string {
  let at = -1
  for (const tag of CARET_HOSTS) at = Math.max(at, html.lastIndexOf(tag))
  return at < 0 ? html + caret : html.slice(0, at) + caret + html.slice(at)
}

// ---- Batched deltas -------------------------------------------------------

// What arrived since the last repaint. One chunk of the stream is far smaller
// than one frame, so the chunks are added up here and applied together.
export interface PendingDelta {
  text: string
  thinking: string
  // The answer so far is wrong and the text here is the whole of it.
  replace: boolean
}

export const NO_DELTA: PendingDelta = { text: '', thinking: '', replace: false }

export function mergeDelta(
  pending: PendingDelta,
  delta: { text?: string; thinking?: string; replace?: boolean },
): PendingDelta {
  // A replacement cancels everything that waits, and what follows it in the
  // same frame is added to the new text.
  if (delta.replace) return { text: delta.text ?? '', thinking: delta.thinking ?? '', replace: true }
  return {
    text: pending.text + (delta.text ?? ''),
    thinking: pending.thinking + (delta.thinking ?? ''),
    replace: pending.replace,
  }
}

// Whether there is anything to apply. A replacement with no text still is:
// it empties the answer.
export function hasDelta(pending: PendingDelta): boolean {
  return pending.replace || pending.text !== '' || pending.thinking !== ''
}

// ---- Clearing the conversation --------------------------------------------

// A long conversation is work, so the question names what goes away: which
// model answered, and how many turns the owner loses.
export function clearQuestion(model: string, turns: number): { title: string; body: string } {
  const one = turns === 1
  return {
    title: `Clear the conversation with ${model}?`,
    body: `This removes ${one ? '1 turn' : `${turns} turns`}. You cannot get ${one ? 'it' : 'them'} back.`,
  }
}

// Asking a question again replaces the answer to it, and the turns under that
// answer go with it. On the last answer there is nothing under it and the
// question is not asked at all.
//
// A retry asks the model the composer is set to now, which is not always the
// model that wrote the answer. `asks` is the model that would answer now and
// `wrote` is the model recorded on the turn, so the sentence appears only when
// the two differ. A turn from before the console recorded the model has no
// name to compare, and a guess reads worse than silence.
export function retryQuestion(turns: number, asks?: string, wrote?: string): { title: string; body: string } {
  const one = turns === 1
  const changed = Boolean(asks && wrote && asks !== wrote)
  return {
    title: 'Ask this question again?',
    body: `The new answer replaces this one, and ${one ? 'the turn' : `the ${turns} turns`} under it ${
      one ? 'goes' : 'go'} as well. You cannot get ${one ? 'it' : 'them'} back.${
      changed ? ` The new answer comes from ${asks}.` : ''}`,
  }
}
