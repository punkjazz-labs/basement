import { describe, expect, it } from 'vitest'
import {
  COMPOSER_MAX_HEIGHT, NO_DELTA, PIN_GAP, answerMeter, clearQuestion, composerHeight, hasDelta,
  jumpInFlight, mergeDelta, pinnedToBottom, shouldReleasePin, splitStreamTail, stoppedMeter, tokenMeter,
  waitMeter, withCaret,
} from './chat'

describe('pinnedToBottom', () => {
  it('holds the pin at the end of the transcript', () => {
    expect(pinnedToBottom({ scrollHeight: 2000, scrollTop: 1600, clientHeight: 400 })).toBe(true)
  })

  it('holds the pin inside the gap', () => {
    expect(pinnedToBottom({ scrollHeight: 2000, scrollTop: 1600 - PIN_GAP, clientHeight: 400 })).toBe(true)
  })

  it('releases the pin one pixel past the gap', () => {
    expect(pinnedToBottom({ scrollHeight: 2000, scrollTop: 1599 - PIN_GAP, clientHeight: 400 })).toBe(false)
  })

  it('releases the pin when the reader scrolls up to read', () => {
    expect(pinnedToBottom({ scrollHeight: 2000, scrollTop: 300, clientHeight: 400 })).toBe(false)
  })

  it('counts content shorter than the box as the end', () => {
    expect(pinnedToBottom({ scrollHeight: 200, scrollTop: 0, clientHeight: 400 })).toBe(true)
  })
})

describe('shouldReleasePin', () => {
  // The scroll a token causes lands at the end of the transcript, so it can
  // never be read as the reader going somewhere.
  it('keeps the pin when a token scrolls the transcript to the end', () => {
    const afterToken = { scrollHeight: 2000, scrollTop: 1600, clientHeight: 400 }
    expect(shouldReleasePin(pinnedToBottom(afterToken), 1000, 0)).toBe(false)
  })

  // No jump is travelling, so a position away from the end is the reader's.
  it('releases the pin when the reader scrolls up to read', () => {
    const scrolledUp = { scrollHeight: 2000, scrollTop: 300, clientHeight: 400 }
    expect(shouldReleasePin(pinnedToBottom(scrolledUp), 1000, 0)).toBe(true)
  })

  // The positions a smooth jump passes through belong to the jump.
  it('keeps the pin through the positions a jump passes on its way', () => {
    expect(shouldReleasePin(false, 1000, 1900)).toBe(false)
  })

  // The wheel and a finger zero the window, which is the whole fix: the
  // reader outranks a jump that is still in flight.
  it('releases the pin when the reader takes the wheel during a jump', () => {
    const jumpUntil = 1900
    const afterWheel = 0
    expect(shouldReleasePin(false, 1000, jumpUntil)).toBe(false)
    expect(shouldReleasePin(false, 1000, afterWheel)).toBe(true)
  })

  it('releases the pin once the window for the jump has passed', () => {
    expect(shouldReleasePin(false, 1900, 1900)).toBe(true)
  })

  // What the pill produces: the trip ends at the end of the transcript.
  it('keeps the pin where a jump lands', () => {
    const landed = { scrollHeight: 2000, scrollTop: 1600, clientHeight: 400 }
    expect(shouldReleasePin(pinnedToBottom(landed), 1400, 1900)).toBe(false)
  })

  // The reader who scrolls back down by hand gets the pin back without the
  // pill, as long as the last line is within the gap.
  it('keeps the pin when the reader returns to the end by hand', () => {
    const nearlyBack = { scrollHeight: 2000, scrollTop: 1600 - PIN_GAP, clientHeight: 400 }
    expect(pinnedToBottom(nearlyBack)).toBe(true)
    expect(shouldReleasePin(pinnedToBottom(nearlyBack), 3000, 0)).toBe(false)
  })
})

describe('composerHeight', () => {
  it('takes the height of the text it holds', () => {
    expect(composerHeight(31)).toBe(31)
  })

  it('stops at the limit, and the box scrolls from there', () => {
    expect(composerHeight(900)).toBe(COMPOSER_MAX_HEIGHT)
    expect(composerHeight(COMPOSER_MAX_HEIGHT)).toBe(COMPOSER_MAX_HEIGHT)
  })
})

describe('waitMeter', () => {
  it('starts at zero', () => {
    expect(waitMeter(0)).toBe('0.0 s · no token yet')
  })

  it('counts the wait in seconds', () => {
    expect(waitMeter(3400)).toBe('3.4 s · no token yet')
  })

  it('never counts below zero', () => {
    expect(waitMeter(-50)).toBe('0.0 s · no token yet')
  })
})

describe('tokenMeter', () => {
  it('states the count and the rate', () => {
    expect(tokenMeter(119, 6000)).toBe('119 tokens · 19.8 tok/s')
  })

  it('states the count alone before there is an interval to measure', () => {
    expect(tokenMeter(0, 0)).toBe('0 tokens')
  })
})

describe('answerMeter', () => {
  it('states the count, the rate and the wait for the first token', () => {
    expect(answerMeter(412, 21237, 380.4)).toBe('412 tokens · 19.4 tok/s · first token in 380 ms')
  })
})

describe('stoppedMeter', () => {
  it('marks a turn the owner stopped', () => {
    expect(stoppedMeter('412 tokens · 19.4 tok/s')).toBe('412 tokens · 19.4 tok/s · Stopped')
  })

  it('says only that it stopped when no token arrived', () => {
    expect(stoppedMeter('')).toBe('Stopped')
  })
})

describe('splitStreamTail', () => {
  it('keeps everything as tail until a blank line closes something', () => {
    expect(splitStreamTail('The answer starts here')).toEqual({ closed: '', tail: 'The answer starts here' })
  })

  it('closes the paragraphs above the last blank line', () => {
    expect(splitStreamTail('One.\n\nTwo.\n\nThree so')).toEqual({
      closed: 'One.\n\nTwo.\n\n',
      tail: 'Three so',
    })
  })

  // The closed part is the whole point: it is what parses once and then holds
  // still while the tail moves.
  it('closes a paragraph the moment the blank line arrives', () => {
    expect(splitStreamTail('One.\n\n')).toEqual({ closed: 'One.\n\n', tail: '' })
  })

  it('never cuts inside an open code fence', () => {
    const text = 'Try this:\n\n```go\nserver := &http.Server{\n\n    Handler: mux,'
    expect(splitStreamTail(text)).toEqual({ closed: 'Try this:\n\n', tail: '```go\nserver := &http.Server{\n\n    Handler: mux,' })
  })

  it('closes a code block once its fence is closed', () => {
    const text = 'Try this:\n\n```go\nx := 1\n```\n\nThen run'
    expect(splitStreamTail(text)).toEqual({ closed: 'Try this:\n\n```go\nx := 1\n```\n\n', tail: 'Then run' })
  })

  it('keeps everything as tail when the only blank line opens the answer', () => {
    expect(splitStreamTail('\n\nstill arriving')).toEqual({ closed: '', tail: '\n\nstill arriving' })
  })
})

describe('withCaret', () => {
  it('puts the caret inside the last paragraph', () => {
    expect(withCaret('<p>arriving</p>\n', '#')).toBe('<p>arriving#</p>\n')
  })

  it('puts the caret inside the last item of a list', () => {
    expect(withCaret('<ul>\n<li>one</li>\n<li>two</li>\n</ul>', '#'))
      .toBe('<ul>\n<li>one</li>\n<li>two#</li>\n</ul>')
  })

  it('puts the caret after the last block when several close', () => {
    expect(withCaret('<pre><code>x := 1</code></pre>\n<p>then</p>\n', '#'))
      .toBe('<pre><code>x := 1</code></pre>\n<p>then#</p>\n')
  })

  it('follows the text into a code block that is still open', () => {
    expect(withCaret('<pre><code>x := 1</code></pre>\n', '#')).toBe('<pre><code>x := 1#</code></pre>\n')
  })

  it('stands alone when no block has closed yet', () => {
    expect(withCaret('', '#')).toBe('#')
  })
})

describe('mergeDelta', () => {
  it('adds up the chunks that arrive between two frames', () => {
    const first = mergeDelta(NO_DELTA, { text: 'Start' })
    expect(mergeDelta(first, { text: ' here' })).toEqual({ text: 'Start here', thinking: '', replace: false })
  })

  it('keeps the reasoning apart from the answer', () => {
    const pending = mergeDelta(mergeDelta(NO_DELTA, { thinking: 'weigh' }), { text: 'so' })
    expect(pending).toEqual({ text: 'so', thinking: 'weigh', replace: false })
  })

  it('drops everything waiting when a replacement arrives', () => {
    const pending = mergeDelta(mergeDelta(NO_DELTA, { text: 'wrong' }), { text: 'right', replace: true })
    expect(pending).toEqual({ text: 'right', thinking: '', replace: true })
  })

  it('adds what follows a replacement to the new text', () => {
    const replaced = mergeDelta(NO_DELTA, { text: 'right', replace: true })
    expect(mergeDelta(replaced, { text: ' again' })).toEqual({ text: 'right again', thinking: '', replace: true })
  })

  it('has nothing to apply until a chunk arrives', () => {
    expect(hasDelta(NO_DELTA)).toBe(false)
    expect(hasDelta(mergeDelta(NO_DELTA, { text: 'a' }))).toBe(true)
  })

  // A replacement with no text is still a change: it empties the answer.
  it('applies a replacement that carries no text', () => {
    expect(hasDelta(mergeDelta(NO_DELTA, { replace: true }))).toBe(true)
  })
})

describe('clearQuestion', () => {
  it('names the model and counts the turns', () => {
    expect(clearQuestion('Laguna S 2.1', 6)).toEqual({
      title: 'Clear the conversation with Laguna S 2.1?',
      body: 'This removes 6 turns. You cannot get them back.',
    })
  })

  it('counts one turn as one turn', () => {
    expect(clearQuestion('Qwen 3.6 27B', 1).body).toBe('This removes 1 turn. You cannot get it back.')
  })
})

describe('jumpInFlight', () => {
  it('is travelling while the window is open', () => {
    expect(jumpInFlight(1000, 1900)).toBe(true)
  })

  it('has landed when the window closes', () => {
    expect(jumpInFlight(1900, 1900)).toBe(false)
  })

  // No jump is travelling, so a token scroll is the instant one.
  it('is never travelling with no jump', () => {
    expect(jumpInFlight(1000, 0)).toBe(false)
  })
})
