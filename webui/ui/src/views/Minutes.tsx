import { useCallback, useEffect, useState } from 'react'
import { copyText, type APIKey } from '../api'
import { noticeBox } from '../confirm'
import { createKey, listKeys, revokeKey } from '../keys'
import {
  MINUTES_DOWNLOAD_URL, MINUTES_KEY_NAME, MINUTES_MODEL,
  lastUsedLine, minutesEndpoint, minutesKey, minutesLines, minutesState, servingLine,
} from '../minutes'
import { SecretReveal } from '../secret'
import { Tip } from '../tip'

// The Minutes page. Minutes is a Mac menu bar app, so this screen is not the
// app: it is the three settings the app asks for, and the proof that the Mac
// has reached this Spark. The steps go away once the first request arrives.

// The sentence beside the page title. App draws it, because that is where the
// title is.
export const MINUTES_HEAD_TIP =
  'Minutes records and transcribes on your Mac. Only the transcript comes to this Spark, to write the notes.'

const SETTINGS_TIP = 'In Minutes: Settings, Endpoint. Paste all three values.'
const MODEL_TIP = 'standard follows whatever model is serving. Assign a fixed model on the Roles page.'

// How often the keys are read again while the page waits for the first
// request. It is the pace the other live reads in the console keep, and it
// runs only while this page is on screen and waiting.
export const MINUTES_KEYS_POLL_MS = 10_000

// How often the page redraws its own clock. The last-used line is written in
// minutes, so it is redrawn every minute; a done page asks the manager for
// nothing, and this costs nothing but a render.
export const MINUTES_CLOCK_MS = 60_000

export interface MinutesPageProps {
  endpoint: string
  keys: readonly APIKey[]
  // The model this Spark serves right now, or nothing while none does.
  servingModel?: string
  nowMs: number
  secret: { name: string; secret: string } | null
  // The owner asked for the steps again on a page that is already set up.
  stepsOpen: boolean
  copied: string
  onCopy: (label: string, value: string) => void
  onCreate: () => void
  onRevoke: (key: APIKey) => void
  onToggleSteps: () => void
  onSecretDone: () => void
}

// Everything the page draws, from data it is given. The screen below holds the
// keys and the presses; this holds the design.
export function MinutesPage(props: MinutesPageProps) {
  const key = minutesKey(props.keys)
  const waiting = !key?.last_used_at
  const done = minutesState(props.keys) === 'done'
  const steps = !done || props.stepsOpen

  const copyButton = (label: string, value: string, name: string) => (
    <button className="ghost act" aria-label={name} onClick={() => props.onCopy(label, value)}>
      {props.copied === label ? 'Copied' : 'Copy'}
    </button>
  )

  const card = (
    <div className="minutes-card">
      <div className="minutes-row">
        <span className="k">Endpoint</span>
        <span className="v">{props.endpoint}</span>
        {copyButton('endpoint', props.endpoint, 'Copy the endpoint')}
      </div>
      <div className="minutes-row">
        <span className="k">Model</span>
        <span className="v">{MINUTES_MODEL}</span>
        <Tip text={MODEL_TIP} label="What the standard model is" />
        {copyButton('model', MINUTES_MODEL, 'Copy the model name')}
      </div>
      <div className="minutes-row">
        <span className="k">API key</span>
        {key === null
          ? <span className="sub">n/a</span>
          : <>
              <span className="v">{key.name}</span>
              <span className="sub">{lastUsedLine(key, props.nowMs)}</span>
            </>}
        {key === null
          ? <button className="primary act" onClick={props.onCreate}>Create key</button>
          : <button className="danger act" onClick={() => props.onRevoke(key)}>Revoke</button>}
      </div>
    </div>
  )

  const settings = (
    <>
      {card}
      {props.secret && (
        <SecretReveal
          name={props.secret.name}
          secret={props.secret.secret}
          copied={props.copied === 'secret'}
          onCopy={() => props.onCopy('secret', props.secret?.secret ?? '')}
          onDone={props.onSecretDone}
        />
      )}
    </>
  )

  // Green is the serving light and nothing else, so a line that is not about a
  // model that answers stays amber.
  const lines = minutesLines(steps, waiting, Boolean(props.servingModel))
  const dotLine = (kind: 'waiting' | 'serving') => (
    <p className="minutes-state">
      <span
        className={kind === 'serving' && props.servingModel ? 'minutes-dot' : 'minutes-dot warn'}
        aria-hidden="true"
      />
      {kind === 'waiting' ? 'Waiting for the first request.' : servingLine(props.servingModel)}
    </p>
  )

  return (
    <div className="stack minutes">
      {done && (
        <div className="minutes-head">
          <button className="quiet" aria-pressed={props.stepsOpen} onClick={props.onToggleSteps}>Setup</button>
        </div>
      )}
      {steps ? (
        <div className="minutes-steps">
          <div className="minutes-step">
            <span className="num">1</span>
            <div className="body">
              <div className="hrow">
                <span className="h">Install Minutes on your Mac</span>
                <a className="minutes-download" href={MINUTES_DOWNLOAD_URL} target="_blank" rel="noopener noreferrer">
                  Download
                </a>
              </div>
            </div>
          </div>
          <div className="minutes-step">
            <span className="num">2</span>
            <div className="body">
              <div className="hrow">
                <span className="h">Point Minutes at this Spark</span>
                <Tip text={SETTINGS_TIP} label="Where these values go in Minutes" />
              </div>
              {settings}
            </div>
          </div>
          <div className="minutes-step">
            <span className="num">3</span>
            <div className="body">
              <div className="hrow"><span className="h">Record a meeting</span></div>
              {lines.step !== 'none' && dotLine(lines.step)}
            </div>
          </div>
        </div>
      ) : settings}
      {lines.foot === 'serving' && dotLine('serving')}
    </div>
  )
}

export default function Minutes({ servingModel }: { servingModel?: string }) {
  const [keys, setKeys] = useState<APIKey[]>([])
  const [secret, setSecret] = useState<{ name: string; secret: string } | null>(null)
  const [stepsOpen, setStepsOpen] = useState(false)
  const [copied, setCopied] = useState('')
  const [nowMs, setNowMs] = useState(() => Date.now())

  const endpoint = minutesEndpoint(window.location.origin)
  const waiting = !minutesKey(keys)?.last_used_at

  const refresh = useCallback(async () => {
    try {
      setKeys(await listKeys())
      setNowMs(Date.now())
    } catch {
      /* the next round catches up */
    }
  }, [])

  // The keys are read once when the page opens. This read stands apart from
  // the poll below so that a page opening on a Spark that Minutes already uses
  // reads the list once and not twice.
  useEffect(() => {
    void refresh()
  }, [refresh])

  // The page turns itself over when the Mac calls: while it is waiting for
  // that first request it reads the keys again on a slow tick. A page that has
  // its answer starts no timer at all, a hidden tab is skipped, and the timer
  // dies with the view, so nothing is read while this screen is not on the
  // console.
  useEffect(() => {
    if (!waiting) return
    const timer = setInterval(() => {
      if (!document.hidden) void refresh()
    }, MINUTES_KEYS_POLL_MS)
    return () => clearInterval(timer)
  }, [refresh, waiting])

  // The page's own clock, so "last used 5 minutes ago" is still true ten
  // minutes later. It asks the manager for nothing.
  useEffect(() => {
    const clock = setInterval(() => setNowMs(Date.now()), MINUTES_CLOCK_MS)
    return () => clearInterval(clock)
  }, [])

  const copy = async (label: string, value: string) => {
    await copyText(value)
    setCopied(label)
    setTimeout(() => setCopied(''), 1600)
  }

  const create = async () => {
    try {
      const result = await createKey(MINUTES_KEY_NAME)
      setSecret({ name: result.key.name, secret: result.secret })
      await refresh()
    } catch (problem) {
      noticeBox('Could not create the key', problem instanceof Error ? problem.message : undefined)
    }
  }

  const revoke = async (key: APIKey) => {
    try {
      if (!await revokeKey(key)) return
      // The secret on screen belongs to the key that just went.
      setSecret(null)
      await refresh()
    } catch (problem) {
      noticeBox('Could not revoke the key', problem instanceof Error ? problem.message : undefined)
    }
  }

  return (
    <MinutesPage
      endpoint={endpoint}
      keys={keys}
      servingModel={servingModel}
      nowMs={nowMs}
      secret={secret}
      stepsOpen={stepsOpen}
      copied={copied}
      onCopy={(label, value) => void copy(label, value)}
      onCreate={() => void create()}
      onRevoke={key => void revoke(key)}
      onToggleSteps={() => setStepsOpen(open => !open)}
      onSecretDone={() => setSecret(null)}
    />
  )
}
