import { useState, useSyncExternalStore } from 'react'
import { formatBytes, type SystemInfo } from '../api'
import { relativeTime } from '../feed'
import { COOL_MODE, COOL_MODE_LABEL, FULL_MODE, FULL_MODE_LABEL, type FleetRow } from '../fleetModels'
import { loadsRefused, shortAge, temperatureLight } from '../fleetHealth'
import type { FleetView } from '../fleetView'
import { HeadroomBar } from '../headroomBar'
import type { PowerSwitchState } from '../powerControl'
import { machineSeries, subscribeTelemetry, telemetryRevision, type Series } from '../telemetry'
import { figure } from './Figure'
import { HealthDot } from './FleetHealth'
import { Icon } from './Icon'
import { PowerSwitch } from './Power'
import { MetricChart } from './Sparkline'

// One box, opened from anywhere its name or its dot appears (Better Stack's
// monitor page, Modal's container drawer): who it is and what it does, three
// facts, one chart with a metric switch, the machine's own description, and
// the two things the owner does to a box from here. Every figure is one the
// box reported; n/a means it has reported none yet.

// The metrics the chart can draw. The manager reports no GPU utilisation in
// either the meter read or the heartbeat, so the chart offers what it does
// report; power draw is the closest reading of how hard the GPU works.
type Metric = 'power' | 'temperature' | 'clock' | 'memFree'

const METRICS: { key: Metric; label: string; title: string; unit: string }[] = [
  { key: 'power', label: 'Power', title: 'GPU power', unit: 'W' },
  { key: 'temperature', label: 'Temp', title: 'GPU temperature', unit: '°C' },
  { key: 'clock', label: 'Clock', title: 'GPU clock', unit: 'MHz' },
  { key: 'memFree', label: 'Memory free', title: 'Memory free', unit: 'GB' },
]

const GB = 1_000_000_000

const pointsOf = (series: Series, metric: Metric): number[] =>
  metric === 'memFree' ? series.memFree.map(value => value / GB) : series[metric]

// The line under the name: how fresh the box's word is.
export function heartbeatLine(row: FleetRow, ageMs: number | undefined): string {
  if (row.isSelf) return 'this console · live'
  if (row.legacyPeerOnly) return 'reports to this console only'
  return ageMs === undefined ? 'no heartbeat yet' : `heartbeat ${shortAge(ageMs)} ago`
}

const modeWord = (mode: string): string =>
  mode === FULL_MODE ? FULL_MODE_LABEL : mode === COOL_MODE ? COOL_MODE_LABEL : 'mode not reported'

export function BoxDrawerBody({ view, row, system, power, onOpenUnit }: {
  view: FleetView
  row: FleetRow
  // This Spark's own description, for the box the console runs on.
  system: SystemInfo | null
  power: PowerSwitchState | null
  onOpenUnit: (id: string) => void
}) {
  useSyncExternalStore(subscribeTelemetry, telemetryRevision, telemetryRevision)
  const [metric, setMetric] = useState<Metric>('power')
  const health = view.healthOf(row.nodeID)
  const room = view.headroomOf(row)
  const unit = view.unitOf(row)
  const key = view.seriesKey(row)
  const held = key ? machineSeries(key) : undefined
  const temperature = view.temperatureOf(row)
  const watts = view.powerOf(row)
  const node = view.node(row.nodeID)
  const inventory = row.inventory
  const chosen = METRICS.find(item => item.key === metric) ?? METRICS[0]
  const points = held ? pointsOf(held.series, metric) : []
  const heat = temperatureLight(temperature)
  const refused = loadsRefused(room)
  const facts: [string, string][] = row.isSelf
    ? [
      ['Product', system?.product_name || inventory?.product_name || 'n/a'],
      ['OS', system?.os || inventory?.os || 'n/a'],
      ['Kernel', inventory?.kernel || 'n/a'],
      ['GPU', inventory?.gpu_description || 'n/a'],
      ['Disk free', system?.storage_available_bytes
        ? `${formatBytes(system.storage_available_bytes)} of ${formatBytes(system.storage_total_bytes)}` : 'n/a'],
      ['Manager version', system?.manager_version || 'n/a'],
      ['Models installed', `${row.installedModels.length}`],
    ]
    : [
      ['Product', inventory?.product_name || 'n/a'],
      ['OS', inventory?.os || 'n/a'],
      ['Kernel', inventory?.kernel || 'n/a'],
      ['GPU', inventory?.gpu_description || 'n/a'],
      ['Disk free', inventory?.storage_available_bytes
        ? `${formatBytes(inventory.storage_available_bytes)} of ${formatBytes(inventory.storage_total_bytes)}` : 'n/a'],
      ['Manager version', node?.manager_version || 'n/a'],
      ['Last heartbeat', relativeTime(node?.last_heartbeat_at, Date.now()) || 'n/a'],
      ['Models installed', inventory ? `${row.installedModels.length}` : 'n/a'],
    ]
  const role = view.roleLine(row)
  const [roleWord, ...unitWords] = role.split(' · ')
  return (
    <div className="dr box">
      <header className="dr-head">
        <HealthDot box={health} />
        <div className="dr-title">
          <h2>{row.displayName}</h2>
          <p className="dr-role">
            {unit ? (
              <>
                {roleWord} ·{' '}
                <button type="button" className="linkish" onClick={() => onOpenUnit(unit.key)}>{unitWords.join(' · ')}</button>
              </>
            ) : role}
          </p>
          <p className="dr-beat">{heartbeatLine(row, view.ageOf(row))}</p>
        </div>
      </header>

      {/* The drawer is a column: the name stays at the top, the actions at
          the bottom, and only what is between them scrolls. */}
      <div className="dr-body">
        {health && health.issues.length > 0 && (
          <ul className="dr-issues">
            {health.issues.map(issue => (
              <li key={issue.kind} className={`state-chip ${issue.light}`}>
                <Icon name={issue.light === 'fail' ? 'failure' : 'alert'} size={14} />
                {issue.text.charAt(0).toUpperCase() + issue.text.slice(1)}
              </li>
            ))}
          </ul>
        )}

        <div className="dr-facts3">
          <div className={`fcard ${room.state}`}>
            <span className="l">Memory free</span>
            <span className="v">{room.state === 'unknown' ? 'n/a' : figure(formatBytes(room.box.availableBytes))}</span>
            <HeadroomBar headroom={room} />
            <span className="s">
              {room.state === 'unknown'
                ? 'not reported yet'
                : `of ${formatBytes(room.box.totalBytes)} · ${formatBytes(room.reserve)} kept free`}
            </span>
            {refused && <span className="s warn">Loads refused</span>}
          </div>
          <div className={`fcard ${heat}`}>
            <span className="l">GPU temp</span>
            <span className="v">{temperature ? figure(`${temperature} °C`) : 'n/a'}</span>
            <span className="s">{temperature ? (heat === 'ok' ? 'in range' : 'hot') : 'not reported'}</span>
          </div>
          <div className="fcard">
            <span className="l">Power</span>
            <span className="v">{watts ? figure(`${watts.toFixed(0)} W`) : 'n/a'}</span>
            <span className="s">{modeWord(power ? power.row.mode : row.power.mode)}</span>
          </div>
        </div>

        <section className="dr-chart" aria-label="History">
          <span className="seg" role="group" aria-label="Metric">
            {METRICS.map(item => (
              <button type="button" key={item.key} aria-pressed={metric === item.key} onClick={() => setMetric(item.key)}>
                {item.label}
              </button>
            ))}
          </span>
          <MetricChart
            points={points}
            stamps={held?.series.at}
            title={chosen.title}
            unit={chosen.unit}
            format={value => (metric === 'memFree' ? value.toFixed(1) : value.toFixed(0))}
            tone={metric === 'temperature' && heat !== 'ok' ? heat : 'plain'}
            max={metric === 'memFree' && room.box.totalBytes ? room.box.totalBytes / GB : undefined}
            empty={!key ? 'Telemetry unavailable from this console' : held?.latest === null ? 'Waiting for telemetry…' : undefined}
          />
        </section>

        <dl className="dr-list">
          {facts.map(([label, value]) => (
            <div key={label} className="dr-fact"><dt>{label}</dt><dd>{value}</dd></div>
          ))}
        </dl>
      </div>

      {(power || (!row.isSelf && row.consoleURL)) && (
        <footer className="dr-actions">
          {power && <PowerSwitch name={row.displayName} power={power.row} onSet={power.set} />}
          {!row.isSelf && row.consoleURL && (
            <button type="button" className="ghost" onClick={() => window.open(row.consoleURL, '_blank', 'noopener,noreferrer')}>
              Open console
            </button>
          )}
        </footer>
      )}
    </div>
  )
}
