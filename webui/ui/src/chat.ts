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

// ---- The answer while it arrives ------------------------------------------

export interface StreamSplit {
  // Everything up to and including the last blank line. Markdown closes a
  // block at a blank line, so this part cannot change again: it is parsed
  // once and the browser keeps the nodes it built for it.
  closed: string
  // What arrived after that line. Only this part parses again on the next
  // token, so the finished text above it holds still and a selection in it
  // survives.
  tail: string
}

// A fence that is still open makes the two halves of a cut parse as broken
// markdown, so a cut is only allowed where the count of fence lines above it
// is even.
const balancedFences = (text: string) => (text.match(/^ {0,3}```/gm) ?? []).length % 2 === 0

export function splitStreamTail(text: string): StreamSplit {
  let from = text.length
  for (;;) {
    const blank = text.lastIndexOf('\n\n', from - 1)
    // A blank line at the very start closes nothing worth keeping.
    if (blank <= 0) return { closed: '', tail: text }
    const cut = blank + 2
    const closed = text.slice(0, cut)
    if (balancedFences(closed)) return { closed, tail: text.slice(cut) }
    from = blank
  }
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
