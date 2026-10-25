import { useEffect, useMemo, useState } from 'react'
import { api, idempotency, type FleetPowerMode, type FleetSummary, type Peer } from './api'
import { noticeBox } from './confirm'
import {
  fleetRows, localPowerRow, powerFanOut, powerFanOutBusy, powerRefusalLine, powerRefusedTitle, powerRow,
  retiredPowerSets,
  FLEET_POWER_MODE_PATH, LOCAL_POWER_MODE_PATH, POWER_REFUSED_TITLE,
  type FleetRow, type PowerRow, type PowerState,
} from './fleetModels'

// How hard each Spark runs its chip, as one control the whole console shares.
// The switch used to live only in the Sparks table; a box's detail drawer now
// carries it too, and the drawer opens from every screen, so the state and the
// calls behind the switch live here and both places draw from the same answer.
// Nothing about the calls changed: one call per Spark through the fleet door
// on a console that leads a fleet, and this Spark's own door otherwise.

// What this Spark calls itself when it answers for its own switch.
const THIS_SPARK = 'This Spark'

export interface PowerSwitchState {
  row: PowerRow
  set: (mode: string) => void
}

export interface PowerControl {
  // The fleet summary has answered, so the console knows whose switches it
  // holds. Until then it holds none.
  ready: boolean
  leadsFleet: boolean
  isMember: boolean
  // The switch one box carries, or null where this console holds none: a
  // member's settings belong to its controller.
  switchFor: (row: FleetRow) => PowerSwitchState | null
  // The one change that reaches every Spark in the fleet, where there is more
  // than one to send it to.
  fanOut: { busy: boolean; mode: string; set: (mode: string) => void } | null
}

export function usePowerControl(membership: FleetSummary | null | undefined, peers: Peer[]): PowerControl {
  // The power mode this console last set on each Spark, and what that Spark
  // answered about it. The membership poll is up to ten seconds behind a
  // change, and that Spark's own heartbeat is behind that again, so without
  // this the switch would jump back to the old mode for a moment.
  const [powerSet, setPowerSet] = useState<Map<string, PowerState>>(new Map())
  // Every Spark a power change is running on now. A fleet-wide run names all
  // of them at once, so no row can take a click the run is about to overwrite.
  const [powerSetting, setPowerSetting] = useState<Set<string>>(new Set())
  // What this Spark says about its own chip, for a console that leads no
  // fleet. null is a mode this console has not read, which is not a mode.
  const [localPower, setLocalPower] = useState<PowerState | null>(null)
  const [localPowerBusy, setLocalPowerBusy] = useState(false)

  const leadsFleet = membership?.role === 'controller'
  const isMember = membership?.role === 'member'

  // Every Spark this console can set the mode on, in the order the fleet
  // reports them. Read at the moment the summary arrives, which is the only
  // moment any of it is fresh.
  const sparks = useMemo(
    () => (leadsFleet ? fleetRows(membership ?? null, peers, window.location.origin, Date.now()) : []),
    [leadsFleet, membership, peers],
  )
  const powerRows = useMemo(
    () => new Map(sparks.map(spark => [spark.nodeID, powerRow(spark, powerSet.get(spark.nodeID), powerSetting)])),
    [sparks, powerSet, powerSetting],
  )

  // A mode this console set is kept only until the fleet read carries it.
  // This runs on every read, so the answer is retired the moment it has
  // nothing left to add, and every later change is read from the fleet like
  // any other fact about that Spark.
  useEffect(() => {
    const retired = retiredPowerSets(sparks, powerSet, powerSetting)
    if (retired.length === 0) return
    setPowerSet(previous => {
      const next = new Map(previous)
      for (const nodeID of retired) next.delete(nodeID)
      return next
    })
  }, [sparks, powerSet, powerSetting])

  // Set the power mode on one Spark, or on every Spark in the fleet. One call
  // per Spark, sent one after another, through the fleet door: the controller
  // takes its own node id there too, so this console holds one way to write
  // the setting rather than a local one and a remote one.
  //
  // Every Spark named is locked for the whole run. A Spark answers with the
  // mode it now holds and with its own sentence if the GPU refused the
  // change, and both stand until that Spark's next heartbeat carries them. A
  // call that was refused outright changed nothing there, so that row goes
  // back to what it last reported and the refusal is said in one notice at
  // the end, one line per Spark.
  const setPowerMode = async (targets: FleetRow[], mode: string) => {
    if (mode === '' || targets.length === 0) return
    if (targets.some(target => powerSetting.has(target.nodeID))) return
    const named = targets.map(target => target.nodeID)
    setPowerSetting(previous => new Set([...previous, ...named]))
    setPowerSet(previous => {
      const next = new Map(previous)
      for (const id of named) next.set(id, { mode, failure: '' })
      return next
    })
    const refused: string[] = []
    try {
      for (const target of targets) {
        try {
          const answer = await api<FleetPowerMode>(FLEET_POWER_MODE_PATH, {
            method: 'POST',
            headers: idempotency(),
            body: JSON.stringify({ node_id: target.nodeID, mode }),
          })
          setPowerSet(previous =>
            new Map(previous).set(target.nodeID, { mode: answer.mode, failure: answer.failure ?? '' }))
        } catch (problem) {
          setPowerSet(previous => {
            const next = new Map(previous)
            next.delete(target.nodeID)
            return next
          })
          refused.push(powerRefusalLine(
            target.displayName, problem instanceof Error ? problem.message : String(problem)))
        }
      }
    } finally {
      setPowerSetting(previous => {
        const next = new Set(previous)
        for (const id of named) next.delete(id)
        return next
      })
    }
    // A run over one Spark has one Spark to name; a fleet-wide run names none
    // in the title and every refused one in the lines under it.
    if (refused.length > 0) {
      noticeBox(
        targets.length === 1 ? powerRefusedTitle(targets[0].displayName) : POWER_REFUSED_TITLE,
        refused.join('\n'),
      )
    }
  }

  // What this Spark holds now, read from its own door. A console that leads a
  // fleet never asks: the summary already carries the mode of every Spark in
  // it, this machine included.
  useEffect(() => {
    // undefined is a summary that has not answered yet, which is not the same
    // as no fleet: the read waits for it rather than asking on a guess.
    if (membership === undefined || leadsFleet || isMember) return
    let cancelled = false
    api<PowerState>(LOCAL_POWER_MODE_PATH)
      .then(state => {
        if (!cancelled) setLocalPower({ mode: state.mode ?? '', failure: state.failure ?? '' })
      })
      .catch(() => {
        /* a manager that cannot read the mode leaves the switch dead */
      })
    return () => {
      cancelled = true
    }
  }, [membership, leadsFleet, isMember])

  // The same change on a Spark that answers for itself. The answer is the
  // whole new state, so what the machine now holds and its own sentence about
  // a GPU that refused the cap both come back together.
  const setLocalPowerMode = async (mode: string) => {
    if (mode === '' || localPowerBusy) return
    setLocalPowerBusy(true)
    try {
      const answer = await api<PowerState>(LOCAL_POWER_MODE_PATH, {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ mode }),
      })
      setLocalPower({ mode: answer.mode ?? '', failure: answer.failure ?? '' })
    } catch (problem) {
      noticeBox(powerRefusedTitle(THIS_SPARK), problem instanceof Error ? problem.message : String(problem))
    } finally {
      setLocalPowerBusy(false)
    }
  }

  // The switch a box carries: the fleet's row for that box on a controller,
  // this Spark's own on a console that leads no fleet, and none on a member,
  // whose settings the controller owns.
  const switchFor = (row: FleetRow): PowerSwitchState | null => {
    if (isMember || membership === undefined) return null
    if (!leadsFleet) {
      return row.isSelf ? { row: localPowerRow(localPower, localPowerBusy), set: setLocalPowerMode } : null
    }
    const power = powerRows.get(row.nodeID)
    const spark = sparks.find(item => item.nodeID === row.nodeID)
    if (!power || !spark) return null
    return { row: power, set: mode => void setPowerMode([spark], mode) }
  }

  // Only where there is more than one Spark to send it to. A Spark added by
  // address never joined this fleet, so powerFanOut leaves it out and it is
  // not counted here either. The pressed mode is the one every Spark in the
  // fleet holds, and none while they differ.
  const targets = powerFanOut(sparks)
  const modes = new Set(targets.map(spark => powerRows.get(spark.nodeID)?.mode ?? ''))
  const fanOut = leadsFleet && targets.length > 1
    ? {
      busy: powerFanOutBusy(sparks, powerSetting),
      mode: modes.size === 1 ? [...modes][0] : '',
      set: (mode: string) => void setPowerMode(targets, mode),
    }
    : null

  return { ready: membership !== undefined, leadsFleet, isMember, switchFor, fanOut }
}
