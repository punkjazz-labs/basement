import { describe, expect, it } from 'vitest'
import {
  COMPOSER_MAX_HEIGHT, PIN_GAP, answerMeter, composerHeight, pinnedToBottom, shouldReleasePin, stoppedMeter,
  tokenMeter, waitMeter,
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
