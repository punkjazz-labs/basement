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
