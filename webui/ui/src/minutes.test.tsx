/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { APIKey } from './api'
import {
  MINUTES_DOWNLOAD_URL, MINUTES_KEY_NAME, MINUTES_MODEL,
  lastUsedLine, minutesEndpoint, minutesKey, minutesLines, minutesState, servingLine,
} from './minutes'
import { MINUTES_CLOCK_MS, MINUTES_KEYS_POLL_MS, MinutesPage, type MinutesPageProps } from './views/Minutes'
import minutesSource from './views/Minutes.tsx?raw'
import appSource from './App.tsx?raw'

const NOW = Date.parse('2026-08-28T12:40:00Z')
const MINUTE = 60 * 1000
const ago = (ms: number): string => new Date(NOW - ms).toISOString()

const ENDPOINT = 'http://192.168.10.148:7070/v1'
const SERVING = 'Qwen3.8 27B'

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
    expect(servingLine(SERVING)).toBe('Qwen3.8 27B is serving.')
    expect(servingLine(undefined)).toBe('No model is serving.')
  })
})

// One dot line per part of the page, and green only for a model that answers.
describe('which dot line each part of the page draws', () => {
  it('says only that it is waiting, while a model serves', () => {
    expect(minutesLines(true, true, true)).toEqual({ step: 'waiting', foot: 'none' })
  })

  // Step 3 cannot finish on a Spark with nothing serving, whatever the owner
  // does on the Mac, so that one fact earns the second line.
  it('warns under the steps when no model serves', () => {
    expect(minutesLines(true, true, false)).toEqual({ step: 'waiting', foot: 'serving' })
  })

  // Reopened steps: the request arrived, so step 3 says what is serving
  // instead of standing empty.
  it('gives the reopened steps their one line under step 3', () => {
    expect(minutesLines(true, false, true)).toEqual({ step: 'serving', foot: 'none' })
    expect(minutesLines(true, false, false)).toEqual({ step: 'serving', foot: 'none' })
  })

  it('keeps the serving line under the settled page, in both colours', () => {
    expect(minutesLines(false, false, true)).toEqual({ step: 'none', foot: 'serving' })
    expect(minutesLines(false, false, false)).toEqual({ step: 'none', foot: 'serving' })
  })
})

describe('the page while it waits for the first request', () => {
  const markup = page({ keys: [], servingModel: SERVING })

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

  // Nothing green stands beside the steps. A working Spark says all it has to
  // say with the one amber line under step 3. The words "is serving" also sit
  // in the model tooltip, so these assertions read the whole line.
  it('draws no serving line while a model serves', () => {
    expect(markup).not.toContain(`${SERVING} is serving.`)
    expect(markup).not.toContain('No model is serving.')
    expect(markup).not.toContain('minutes-dot"')
  })

  it('warns instead when no model is serving', () => {
    const stalled = page({ keys: [] })
    expect(stalled).toContain('Waiting for the first request.')
    expect(stalled).toContain('No model is serving.')
    expect(stalled).not.toContain('minutes-dot"')
  })
})

describe('the page once Minutes has called', () => {
  const markup = page({ keys: [used], servingModel: SERVING })

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

  // Revoking breaks a running Mac app, so the button says so in the same red
  // the API page uses for the same action.
  it('wears the console red on Revoke', () => {
    expect(markup).toContain('class="danger act"')
    expect(markup).not.toContain('class="ghost act">Revoke')
  })

  it('offers the steps again', () => {
    expect(markup).toContain('Setup')
    expect(markup).toContain('aria-pressed="false"')
  })

  it('names the model that is serving, in green', () => {
    expect(markup).toContain('Qwen3.8 27B is serving.')
    expect(markup).not.toContain('minutes-dot warn')
  })

  it('says when no model is serving, in amber', () => {
    const quiet = page({ keys: [used] })
    expect(quiet).toContain('No model is serving.')
    expect(quiet).toContain('minutes-dot warn')
  })
})

describe('the Setup link on a page that is set up', () => {
  const markup = page({ keys: [used], stepsOpen: true, servingModel: SERVING })

  it('brings the steps back', () => {
    expect(markup).toContain('Install Minutes on your Mac')
    expect(markup).toContain('Point Minutes at this Spark')
    expect(markup).toContain('aria-pressed="true"')
  })

  // The Mac has already called, so step 3 says what is serving rather than
  // waiting for a request that arrived, and it never stands empty.
  it('gives step 3 the serving line instead of the waiting one', () => {
    expect(markup).not.toContain('Waiting for the first request.')
    expect(markup).toContain(`${SERVING} is serving.`)
    // Once, under step 3, and not again at the foot.
    expect(markup.match(/Qwen3\.8 27B is serving\./g)).toHaveLength(1)
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

describe('a key revoked', () => {
  it('takes the page back to the steps', () => {
    const before = page({ keys: [used], servingModel: SERVING })
    expect(before).not.toContain('Install Minutes on your Mac')
    // The list after the revoke: the key is gone, so the setup is back.
    const after = page({ keys: [], servingModel: SERVING })
    expect(after).toContain('Install Minutes on your Mac')
    expect(after).toContain('Create key')
    expect(after).not.toContain('Revoke')
    expect(after).toContain('Waiting for the first request.')
  })
})

// What this page does over time cannot be rendered: there is no browser in
// this suite. The console guards such rules by reading the screen as source,
// the way power.test.tsx does. These are rename fragile on purpose: an edit
// that moves one of these lines has to come here and say so.
describe('when the page reads the key list', () => {
  it('reads the list once when it opens', () => {
    expect(minutesSource).toMatch(/useEffect\(\(\) => \{\s*void refresh\(\)\s*\}, \[refresh\]\)/)
  })

  it('starts no timer once the first request has arrived', () => {
    expect(minutesSource).toMatch(/if \(!waiting\) return\s+const timer = setInterval\(/)
  })

  it('reads again at the pace the view names, and skips a hidden tab', () => {
    expect(MINUTES_KEYS_POLL_MS).toBe(10_000)
    expect(minutesSource).toMatch(/setInterval\(\(\) => \{\s*if \(!document\.hidden\) void refresh\(\)\s*\}, MINUTES_KEYS_POLL_MS\)/)
  })

  it('stops the timer when the view goes away', () => {
    expect(minutesSource).toContain('return () => clearInterval(timer)')
  })

  // A page nobody is looking at must ask nothing, so this view is mounted with
  // its tab. Chat and the Redactor stay mounted behind display:none; moving
  // Minutes to that pattern would leave the timer running for ever.
  it('is mounted with its tab rather than hidden behind it', () => {
    expect(appSource).toContain("{tab === 'Minutes' && <Minutes")
    expect(appSource).not.toMatch(/display: tab === 'Minutes'/)
  })

  // The clock is the one timer a settled page keeps, and it asks the manager
  // for nothing: it only redraws the last used line.
  it('keeps its own clock without reading anything', () => {
    expect(MINUTES_CLOCK_MS).toBe(60_000)
    expect(minutesSource).toMatch(/setInterval\(\(\) => setNowMs\(Date\.now\(\)\), MINUTES_CLOCK_MS\)/)
  })
})

describe('what Revoke does on this page', () => {
  const revoke = /const revoke = async \(key: APIKey\) => \{[\s\S]*?\n {2}\}/.exec(minutesSource)?.[0] ?? ''

  it('asks through the shared flow, so the question is asked once everywhere', () => {
    expect(revoke).not.toBe('')
    expect(revoke).toContain('revokeKey(key)')
  })

  // The secret block belongs to the key. Left standing after the key goes, it
  // offers a credential that no longer opens anything.
  it('takes the secret of that key off the screen', () => {
    expect(revoke).toContain('setSecret(null)')
  })

  it('reads the list again, so the page falls back to the steps', () => {
    expect(revoke).toContain('await refresh()')
  })
})
