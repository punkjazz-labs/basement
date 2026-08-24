import { describe, expect, it } from 'vitest'
import {
  COMPOSER_MAX_HEIGHT, PIN_GAP, answerMeter, composerHeight, pinnedToBottom, stoppedMeter, tokenMeter, waitMeter,
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
