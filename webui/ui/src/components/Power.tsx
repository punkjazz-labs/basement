import {
  COOL_MODE, COOL_MODE_LABEL, COOL_MODE_SHORT, EVERY_SPARK, FULL_MODE, FULL_MODE_LABEL, FULL_MODE_SHORT,
  POWER_MODE_BUSY, type PowerRow,
} from '../fleetModels'

// One Spark's power switch: the machine, the two modes, and whatever else
// that row carries. The mode holds across restarts, so this is a setting and
// not an action. A Spark that has reported no mode gets a dead switch and no
// selection: nothing here guesses that a silent Spark runs at full speed.
// compact is the same switch inside a table row: the machine is named by the
// row, so the switch drops its name and says each mode in one word.
export function PowerSwitch({ name, power, onSet, compact = false }: {
  name: string
  power: PowerRow
  onSet: (mode: string) => void
  compact?: boolean
}) {
  return (
    <div className={compact ? 'pcell' : 'prow'}>
      {!compact && <span className="nm">{name}</span>}
      <span className="seg" role="group" aria-label={`Power mode for ${name}`}>
        <button
          type="button"
          aria-pressed={power.mode === FULL_MODE}
          aria-label={compact ? FULL_MODE_LABEL : undefined}
          title={compact ? FULL_MODE_LABEL : undefined}
          disabled={power.disabled}
          onClick={() => onSet(FULL_MODE)}
        >
          {compact ? FULL_MODE_SHORT : FULL_MODE_LABEL}
        </button>
        <button
          type="button"
          aria-pressed={power.mode === COOL_MODE}
          aria-label={compact ? COOL_MODE_LABEL : undefined}
          title={compact ? COOL_MODE_LABEL : undefined}
          disabled={power.disabled}
          onClick={() => onSet(COOL_MODE)}
        >
          {compact ? COOL_MODE_SHORT : COOL_MODE_LABEL}
        </button>
      </span>
      {power.busy && <span className="busy">{POWER_MODE_BUSY}</span>}
      {/* That Spark's own words about a GPU that did not take the mode. The
          mode the owner chose stays chosen while this stands: the setting is
          stored, and it goes back on the chip at the next start. A refusal is
          not tooltip material, so it stays on the screen. */}
      {power.failure !== '' && <p className="powerfail">{power.failure}</p>}
    </div>
  )
}

// The one fleet-wide change: each button sends that mode to every Spark in
// the fleet. It names no machine to copy the mode from on purpose. The Spark
// the console runs on used to be that source, and a controller whose own GPU
// reports no mode then had no way at all to set the fleet, which is a real
// state on a machine with no nvidia-smi command. The only thing that holds
// this control is a run already under way (powerFanOutBusy). The pressed
// button is the mode every Spark already holds, and neither is pressed while
// they differ: it reads the fleet, it never stands in for it.
export function PowerFanOut({ busy, onSet, mode = '' }: { busy: boolean; onSet: (mode: string) => void; mode?: string }) {
  return (
    <div className="prow every">
      <span className="nm">{EVERY_SPARK}</span>
      <span className="seg" role="group" aria-label={EVERY_SPARK}>
        <button type="button" aria-pressed={mode === FULL_MODE} disabled={busy} onClick={() => onSet(FULL_MODE)}>
          {FULL_MODE_LABEL}
        </button>
        <button type="button" aria-pressed={mode === COOL_MODE} disabled={busy} onClick={() => onSet(COOL_MODE)}>
          {COOL_MODE_LABEL}
        </button>
      </span>
    </div>
  )
}
