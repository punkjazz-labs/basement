/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { APIKey } from './api'
import {
  MINUTES_DOWNLOAD_URL, MINUTES_KEY_NAME, MINUTES_MODEL,
  lastUsedLine, minutesEndpoint, minutesKey, minutesState, servingLine,
} from './minutes'
import { MinutesPage, type MinutesPageProps } from './views/Minutes'

const NOW = Date.parse('2026-08-28T12:40:00Z')
const MINUTE = 60 * 1000
const ago = (ms: number): string => new Date(NOW - ms).toISOString()

const ENDPOINT = 'http://192.168.10.148:7070/v1'

const key = (over: Partial<APIKey> = {}): APIKey => ({
  id: 'key-1',
  name: MINUTES_KEY_NAME,
  created_at: ago(3 * 24 * 60 * MINUTE),
  ...over,
})

const used = key({ last_used_at: ago(5 * MINUTE) })
const unused = key()
const other = key({ id: 'key-2', name: 'laptop', last_used_at: ago(MINUTE) })

const page = (over: Partial<MinutesPageProps> = {}) =>
  renderToStaticMarkup(
    <MinutesPage
      endpoint={ENDPOINT}
      keys={[]}
      nowMs={NOW}
      secret={null}
      stepsOpen={false}
      copied=""
      onCopy={() => {}}
      onCreate={() => {}}
      onRevoke={() => {}}
      onToggleSteps={() => {}}
      onSecretDone={() => {}}
      {...over}
    />,
  )

// The whole state of this page is one key: whether the console made it, and
// whether a request has ever arrived with it. Nothing else is remembered.
describe('which state the Minutes page is in', () => {
  it('asks for the setup while there is no key', () => {
    expect(minutesState([])).toBe('setup')
    expect(minutesKey([])).toBe(null)
  })

  it('reads only the key the console makes for Minutes', () => {
    expect(minutesState([other])).toBe('setup')
    expect(minutesKey([other])).toBe(null)
  })

  // A key that has served nothing proves nothing about the Mac.
  it('keeps asking while the key has served nothing', () => {
    expect(minutesState([unused])).toBe('setup')
    expect(minutesKey([unused])).toBe(unused)
  })

  it('is done once a request has arrived', () => {
    expect(minutesState([other, used])).toBe('done')
    expect(minutesKey([other, used])).toBe(used)
  })

  // Revoking takes the key out of the list, and the page goes back to step
  // one: there is nothing left for the Mac to call with.
  it('goes back to the setup when the key is revoked', () => {
    expect(minutesState([other])).toBe('setup')
  })

  // A second key of the same name must not hide a setup that already works.
  it('reads the used key when two carry the name', () => {
    expect(minutesKey([unused, used])).toBe(used)
    expect(minutesState([unused, used])).toBe('done')
  })
})

describe('the three values Minutes is given', () => {
  it('takes the endpoint from the address the console is open on', () => {
    expect(minutesEndpoint('http://192.168.10.148:7070')).toBe(ENDPOINT)
    expect(minutesEndpoint('https://spark-f1cc.tail3740ce.ts.net')).toBe('https://spark-f1cc.tail3740ce.ts.net/v1')
  })

  // The name that keeps working after the model behind it changes.
  it('names the standard role rather than a model', () => {
    expect(MINUTES_MODEL).toBe('role/standard')
  })

  it('points the download at the release page', () => {
    expect(MINUTES_DOWNLOAD_URL).toBe('https://github.com/punkjazz-labs/minutes/releases/latest')
  })

  it('says when the key was last used, and n/a until it is', () => {
    expect(lastUsedLine(used, NOW)).toBe('last used 5 minutes ago')
    expect(lastUsedLine(unused, NOW)).toBe('n/a')
    expect(lastUsedLine(null, NOW)).toBe('n/a')
  })

  it('names the model that is serving, or says none is', () => {
    expect(servingLine('Qwen3.8 27B')).toBe('Qwen3.8 27B is serving.')
    expect(servingLine(undefined)).toBe('No model is serving.')
  })
})

describe('the page while it waits for the first request', () => {
  const markup = page({ keys: [], servingModel: 'Qwen3.8 27B' })

  it('draws the three steps', () => {
    expect(markup).toContain('Install Minutes on your Mac')
    expect(markup).toContain('Point Minutes at this Spark')
    expect(markup).toContain('Record a meeting')
  })

  it('offers the download in a new tab', () => {
    expect(markup).toContain(`href="${MINUTES_DOWNLOAD_URL}"`)
    expect(markup).toContain('target="_blank"')
    expect(markup).toContain('rel="noopener noreferrer"')
  })

  it('shows the endpoint and the model, with the key still to make', () => {
    expect(markup).toContain(ENDPOINT)
    expect(markup).toContain(MINUTES_MODEL)
    expect(markup).toContain('>n/a<')
    expect(markup).toContain('Create key')
    expect(markup).not.toContain('Revoke')
  })

  it('says it is waiting, and offers no way back to a setup it is showing', () => {
    expect(markup).toContain('Waiting for the first request.')
    expect(markup).toContain('minutes-dot warn')
    expect(markup).not.toContain('Setup')
  })

  it('names the model that is serving under it', () => {
    expect(markup).toContain('Qwen3.8 27B is serving.')
  })
})

describe('the page once Minutes has called', () => {
  const markup = page({ keys: [used], servingModel: 'Qwen3.8 27B' })

  it('drops the steps', () => {
    expect(markup).not.toContain('Install Minutes on your Mac')
    expect(markup).not.toContain('Record a meeting')
    expect(markup).not.toContain('Waiting for the first request.')
  })

  it('keeps the settings, and says when the key was last used', () => {
    expect(markup).toContain(ENDPOINT)
    expect(markup).toContain(MINUTES_MODEL)
    expect(markup).toContain(`>${MINUTES_KEY_NAME}<`)
    expect(markup).toContain('last used 5 minutes ago')
    expect(markup).toContain('Revoke')
    expect(markup).not.toContain('Create key')
  })

  it('offers the steps again', () => {
    expect(markup).toContain('Setup')
    expect(markup).toContain('aria-pressed="false"')
  })

  // Green is the serving light. A Spark with nothing serving says so here
  // whatever the setup above says.
  it('says when no model is serving', () => {
    const quiet = page({ keys: [used] })
    expect(quiet).toContain('No model is serving.')
    expect(quiet).toContain('minutes-dot warn')
    expect(markup).not.toContain('minutes-dot warn')
  })
})

describe('the Setup link on a page that is set up', () => {
  const markup = page({ keys: [used], stepsOpen: true, servingModel: 'Qwen3.8 27B' })

  it('brings the steps back', () => {
    expect(markup).toContain('Install Minutes on your Mac')
    expect(markup).toContain('Point Minutes at this Spark')
    expect(markup).toContain('aria-pressed="true"')
  })

  // The Mac has already called, so nothing here is waiting for it.
  it('does not say it is waiting for a request that arrived', () => {
    expect(markup).not.toContain('Waiting for the first request.')
  })
})

// The secret exists once. It is shown in the same words the API page uses,
// because it is the same key list underneath.
describe('a key just created', () => {
  it('shows the secret once, with a way to copy it', () => {
    const markup = page({
      keys: [unused],
      secret: { name: MINUTES_KEY_NAME, secret: 'bsk_live_example' },
    })
    expect(markup).toContain('created. Copy the key now.')
    expect(markup).toContain('It will not be shown again.')
    expect(markup).toContain('bsk_live_example')
    expect(markup).toContain('Copy key')
  })
})
