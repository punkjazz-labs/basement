import { Fragment, useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { api, copyText, formatBytes, idempotency, type Recipe, type Role } from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { Icon } from '../components/Icon'
import { readableWeights, sortCatalog } from '../catalog'
import { fleetRows, shortSparkName } from '../fleetModels'
import type { FitResult } from '../headroom'
import { fleetMemory, loadFit, loadTargets } from '../loadFit'
import { DEFAULT_ROLE, combinedFit, distinctModelsAfter, isValidRoleName, normalizeRoleName, roleRows } from '../roles'
import { LOCAL_MACHINE, machineSeries, subscribeTelemetry, telemetryRevision } from '../telemetry'
import { boxesWord, servesModel, unitFor, unitPlacement, unitServing } from '../units'

// The swap warning, in the words the design settled on. This Spark serves one
// model at a time, so it is shown whenever a choice would leave roles pointing
// at more than one model, which is exactly when models swap in and out.
const SWAP_NOTE = 'Switching models delays the first response.'

export default function Roles({ system, recipes, models, jobs, peers, fleet, deployments }: AppState) {
  const [roles, setRoles] = useState<Role[]>([])
  // The role whose picker is open. '' is every row closed; 'new' is the
  // add-a-custom-role form.
  const [open, setOpen] = useState('')
  const [choice, setChoice] = useState('')
  const [newName, setNewName] = useState('')
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState('')
  // A read that failed is said out loud: an empty table would otherwise read
  // as "no roles assigned", which is a different statement from "this console
  // could not ask".
  const [readProblem, setReadProblem] = useState('')

  const load = () =>
    api<Role[]>('/api/v1/roles')
      .then(next => {
        setRoles(next)
        setReadProblem('')
      })
      .catch(problem => setReadProblem(problem instanceof Error ? problem.message : String(problem)))
  useEffect(() => {
    load()
  }, [])

  const installed = useMemo(() => new Map(models.map(model => [model.recipe_id, model])), [models])
  const catalog = useMemo(() => new Map(recipes.map(recipe => [recipe.id, recipe])), [recipes])
  const choices = useMemo(
    () => sortCatalog(recipes.filter(recipe => installed.has(recipe.id))),
    [recipes, installed],
  )
  const assignment = useMemo(() => new Map(roles.map(role => [role.name, role.recipe_id])), [roles])
  const rows = useMemo(() => roleRows(roles), [roles])
  const labelOf = (name: string) => rows.find(row => row.name === name)?.label ?? name

  // The fleet, read the way the Models page reads it, so a model serving on
  // two EdgeXperts is serving here too and never reads as missing. Only the
  // console that leads the fleet holds the other boxes.
  useSyncExternalStore(subscribeTelemetry, telemetryRevision, telemetryRevision)
  const sparks = useMemo(
    () => fleetRows(fleet?.role === 'controller' ? fleet : null, peers, window.location.origin, Date.now()),
    [fleet, peers],
  )
  const { rows: boxes, grouping, self, view: memory } = fleetMemory({
    sparks, models, recipes, deployments, jobs, system, latest: machineSeries(LOCAL_MACHINE).latest,
  })
  const inFleet = boxes.length > 1
  const boxNames = boxes.map(box => box.displayName)
  const shortName = (name: string) => shortSparkName(name, boxNames)

  // Where a model runs: the unit the fleet placed it on, the box that serves
  // or holds it, or this Spark. A model held nowhere runs nowhere.
  const runsOn = (recipeID: string): string => {
    const unit = unitFor(grouping, [recipeID], self.nodeID)
    if (unit) return unitPlacement(unit)
    if (installed.has(recipeID)) return inFleet ? self.displayName : 'This Spark'
    const holders = boxes.filter(box => !box.isSelf && box.installedModels.some(model => model.recipe_id === recipeID))
    const serving = holders.find(box => servesModel(box, recipeID))
    if (serving) return serving.displayName
    if (holders.length === 1) return holders[0].displayName
    return holders.length > 1 ? boxesWord(holders) : 'n/a'
  }

  // Whether this model could load on this Spark now, which is what a role
  // asks of it on its first request. A model already serving loads nothing.
  const loadHere = (recipe: Recipe): FitResult | null => {
    const model = installed.get(recipe.id)
    if (!model || (model.active && model.status === 'ready')) return null
    return loadFit(recipe, loadTargets(recipe, self, grouping), memory)
  }

  const base = `${window.location.origin}/v1`
  const shortBase = `${window.location.host}/v1`
  const memoryTotal = system?.memory_total_bytes ?? 0

  // What the models behind roles need in memory together, and what a given
  // choice would change that to. Both are estimates from each recipe's own
  // declared memory model; a recipe without one leaves the answer unknown.
  const recipesFor = (assignments: Map<string, string>): Recipe[] =>
    [...assignments.values()].map(id => catalog.get(id)).filter((item): item is Recipe => Boolean(item))
  const currentFit = combinedFit(recipesFor(assignment), memoryTotal)
  const fitWith = (roleName: string, recipeID: string) => {
    const next = new Map(assignment)
    next.set(roleName, recipeID)
    return combinedFit(recipesFor(next), memoryTotal)
  }

  const copy = async (label: string, value: string) => {
    await copyText(value)
    setCopied(label)
    setTimeout(() => setCopied(''), 1600)
  }

  const closePicker = () => {
    setOpen('')
    setChoice('')
    setNewName('')
  }

  const openPicker = (roleName: string) => {
    setOpen(roleName)
    setChoice(assignment.get(roleName) ?? '')
    setNewName('')
  }

  const toggleAddRole = () => {
    if (open === 'new') {
      closePicker()
      return
    }
    setOpen('new')
    setChoice('')
    setNewName('')
  }

  const assign = async (roleName: string, recipeID: string) => {
    if (busy || !recipeID) return
    setBusy(true)
    try {
      await api<Role>('/api/v1/roles', {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ role: roleName, recipe_id: recipeID }),
      })
      await load()
      closePicker()
    } catch (problem) {
      noticeBox('That did not work', problem instanceof Error ? problem.message : String(problem))
    } finally {
      setBusy(false)
    }
  }

  const clear = async (roleName: string) => {
    const { ok } = await confirmBox({
      title: `Clear the ${labelOf(roleName)} role?`,
      body: `Apps asking for role/${roleName} get an error naming this page until you assign a model again.`,
      confirmLabel: 'Clear role',
      danger: true,
    })
    if (!ok) return
    setBusy(true)
    try {
      await api(`/api/v1/roles/${encodeURIComponent(roleName)}`, { method: 'DELETE', body: '{}' })
      await load()
      closePicker()
    } catch (problem) {
      noticeBox('That did not work', problem instanceof Error ? problem.message : String(problem))
    } finally {
      setBusy(false)
    }
  }

  // What a model is called in a picker row: the weights format the recipe
  // pins and the size of the download, both read from the recipe itself.
  const weightsLine = (recipe: Recipe) => {
    const quant =
      recipe.service.sglang?.quantization ??
      (recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant : undefined)
    const size = formatBytes(recipe.artifact_bytes)
    return quant ? `${quant} · ${size}` : size
  }

  // The model serving right now, which is what the standard role answers from
  // until it is assigned one of its own.
  const servingModel = models.find(model => model.active && model.status === 'ready')
  const servingRecipe = servingModel ? catalog.get(servingModel.recipe_id) : undefined

  const stateOf = (roleName: string): { word: string; dot: string } => {
    const recipeID = assignment.get(roleName)
    if (!recipeID) {
      if (roleName !== DEFAULT_ROLE) return { word: 'Not assigned', dot: '' }
      return servingModel
        ? { word: 'Uses active model', dot: 'on' }
        : { word: 'Nothing is serving', dot: '' }
    }
    const model = installed.get(recipeID)
    if (model?.active && model.status === 'ready') return { word: 'Serving', dot: 'on' }
    // Serving elsewhere in the fleet is serving: green, and named.
    const unit = unitFor(grouping, [recipeID])
    if (unit && unitServing(unit)) {
      return { word: `Serving on ${boxesWord(unit.members.map(member => member.row), unit.size)}`, dot: 'on' }
    }
    const elsewhere = boxes.find(box => !box.isSelf && servesModel(box, recipeID))
    if (elsewhere) return { word: `Serving on ${shortName(elsewhere.displayName)}`, dot: 'on' }
    if (model) return { word: 'Loads on first request', dot: 'wait' }
    const holder = boxes.find(box => !box.isSelf && box.installedModels.some(item => item.recipe_id === recipeID))
    if (holder) return { word: `Installed on ${shortName(holder.displayName)}`, dot: 'wait' }
    // A model that is not installed is a setup to finish, not a failure.
    return { word: 'Not installed', dot: 'warn' }
  }

  const picker = (roleName: string, label: string) => {
    const current = assignment.get(roleName)
    return (
      <>
        <div className="picker-head">
          <h3>{current ? `Change model for ${label}` : `Choose a model for ${label}`}</h3>
        </div>
        {/* No memory line here: the section head above already carries it,
            and each choice below says for itself whether it would fit. */}
        {choices.length === 0 ? (
          <p className="muted" style={{ fontSize: 12.5 }}>
            No models installed. Install one from the Models page.
          </p>
        ) : (
          <div className="install-choice" role="radiogroup" aria-label={`Choose a model for ${label}`}>
            {choices.map(recipe => {
              const model = installed.get(recipe.id)
              const serving = Boolean(model?.active && model.status === 'ready')
              const alsoFor = roles.filter(role => role.recipe_id === recipe.id && role.name !== roleName)
              const notes = [weightsLine(recipe)]
              if (recipe.topology.spark_count > 1) notes.push(`runs across ${recipe.topology.spark_count} Sparks`)
              if (recipe.id === current) notes.push('currently assigned')
              if (serving) notes.push('serving now')
              if (alsoFor.length > 0) notes.push(`already assigned to ${alsoFor.map(role => labelOf(role.name)).join(' and ')}`)
              // One model runs at a time here, so choosing a model no other
              // role uses is what makes models start swapping.
              const swaps = distinctModelsAfter(roles, roleName, recipe.id) > 1
              if (fitWith(roleName, recipe.id).overBudget) {
                notes.push('may not fit together in memory')
              }
              // A role loads its model on the first request. One that has no
              // room to load is not offered: the owner's rule holds here too.
              const room = loadHere(recipe)
              const held = room?.fits === false && recipe.id !== current
              return (
                <label className={`confirm-check ${swaps || held ? 'over' : ''}`} key={recipe.id}>
                  <input
                    type="radio"
                    name={`${roleName}-model`}
                    checked={choice === recipe.id}
                    disabled={held}
                    onChange={() => setChoice(recipe.id)}
                  />
                  <span>
                    {recipe.display_name}
                    <small>{notes.join(' · ')}</small>
                    {held ? <span className="swap-note">{room?.reason}</span>
                      : swaps && <span className="swap-note">{SWAP_NOTE}</span>}
                  </span>
                  <span className="row-check" />
                </label>
              )
            })}
          </div>
        )}
        <div className="picker-foot">
          {current && (
            <button type="button" className="quiet" disabled={busy} onClick={() => clear(roleName)}>
              Clear this role
            </button>
          )}
          <button type="button" className="ghost" onClick={closePicker}>Cancel</button>
          <button
            type="button"
            className="primary"
            disabled={busy || !choice || choice === current ||
              (catalog.get(choice) ? loadHere(catalog.get(choice) as Recipe)?.fits === false : false)}
            onClick={() => assign(roleName, choice)}
          >
            Assign to {label}
          </button>
        </div>
      </>
    )
  }

  const rowFor = ({ name, label }: { name: string; label: string; use: string }) => {
    const expanded = open === name
    const recipeID = assignment.get(name)
    const recipe = recipeID ? catalog.get(recipeID) : undefined
    const state = stateOf(name)
    // The model that answers this role now: its own, or for the standard
    // role the one serving.
    const answering = recipe ?? (name === DEFAULT_ROLE && !recipeID ? servingRecipe : undefined)
    const room = recipe ? loadHere(recipe) : null
    const toggle = () => (expanded ? closePicker() : openPicker(name))
    const act = (work: () => void) => (event: React.MouseEvent) => {
      event.stopPropagation()
      work()
    }
    return (
      <Fragment key={name}>
        <div
          className={`rrow ${expanded ? 'open' : ''}`}
          role="button"
          tabIndex={0}
          aria-expanded={expanded}
          onClick={toggle}
          onKeyDown={event => {
            if (event.target === event.currentTarget && (event.key === 'Enter' || event.key === ' ')) {
              event.preventDefault()
              toggle()
            }
          }}
        >
          <div className="r-id">
            <div className="glyph" aria-hidden="true">{label.slice(0, 1).toUpperCase()}</div>
            <div>
              <div className="nm">{label}</div>
            </div>
          </div>
          <div className="r-endpoint">
            <div className="url">
              <span>{shortBase}</span>
              <button type="button" className="copy-btn" onClick={act(() => copy(name, base))}>
                {copied === name ? 'Copied' : 'Copy'}
              </button>
            </div>
            <div className="modelname"><code>model: role/{name}</code></div>
          </div>
          <div className="r-model">
            {recipe ? (
              <>
                <div className="nm">{recipe.display_name}</div>
                <div className="fp">{weightsLine(recipe)}</div>
              </>
            ) : recipeID ? (
              <div className="nm faint">{recipeID}</div>
            ) : name === DEFAULT_ROLE && servingRecipe ? (
              <>
                <div className="nm">{servingRecipe.display_name}</div>
              </>
            ) : (
              <div className="nm faint">No model yet</div>
            )}
          </div>
          <div className="r-runs">
            {answering ? runsOn(answering.id) : recipeID ? runsOn(recipeID) : <span className="faint">n/a</span>}
          </div>
          <div className="r-status">
            <span className={`sdot ${state.dot}`} aria-hidden="true" />
            <span>
              {state.word}
              {/* The first request would have to load this model, and this
                  Spark has no room for it now. */}
              {room?.fits === false && <span className="room-flag" title={room.reason}>No room to load</span>}
            </span>
          </div>
          <div className="r-actions" onKeyDown={event => event.stopPropagation()}>
            <button className={recipeID ? 'ghost' : 'primary'} onClick={act(toggle)}>
              {recipeID ? 'Change model' : 'Choose a model'}
            </button>
          </div>
          <span className={`r-caret ${expanded ? 'open' : ''}`} aria-hidden="true">
            <Icon name="chevronRight" size={14} />
          </span>
        </div>
        {expanded && (
          <div className="rdetail">
            <dl className="facts">
              <dt>Endpoint</dt>
              <dd><code>{base}</code></dd>
              {room?.fits === false && (
                <>
                  <dt>Memory</dt>
                  <dd className="room-line">{room.reason}</dd>
                </>
              )}
              <dt>Model name apps use</dt>
              <dd><code>role/{name}</code></dd>
              <dt>Answering now</dt>
              <dd>
                {recipe
                  ? `${recipe.display_name}, ${state.word.toLowerCase()}`
                  : recipeID
                    ? `${recipeID}, ${state.word.toLowerCase()}`
                    : name === DEFAULT_ROLE
                      ? servingRecipe
                        ? `${servingRecipe.display_name}, until you assign one`
                        : 'Nothing is serving yet.'
                      : 'Nothing yet. Pick a model below.'}
              </dd>
            </dl>
            {picker(name, label)}
          </div>
        )}
      </Fragment>
    )
  }


  return (
    <div className="stack">
      {readProblem && (
        <div className="error-note" role="alert">
          <strong>Could not load roles</strong>
          <p>{readProblem}</p>
        </div>
      )}
      <div className="section-head">
        <h2>Your roles</h2>
        <span className="spacer" />
        {memoryTotal > 0 && (
          <span className="faint" style={{ fontSize: 12 }}>
            {formatBytes(memoryTotal)} unified memory on this Spark
            {currentFit.bytes !== null && roles.length > 0 &&
              ` · roles need an estimated ${formatBytes(currentFit.bytes)} together`}
          </span>
        )}
      </div>

      <div className="rtable">
        <div className="rthead" aria-hidden="true">
          <span>Role</span><span>Endpoint</span><span>Assigned model</span><span>Runs on</span><span>State</span>
          <span /><span />
        </div>
        {rows.map(rowFor)}
        <div className="rrow add">
          <div className="add-role">
            <div className="txt">
              <strong>Add a custom role</strong>
            </div>
            <button type="button" className="ghost" onClick={toggleAddRole}>+ Add custom role</button>
          </div>
        </div>
        {open === 'new' && (
          <div className="rdetail">
            <div className="picker-head">
              <h3>Name the role</h3>
            </div>
            <label className="field" style={{ marginBottom: 14, maxWidth: 320 }}>
              <span>role/{isValidRoleName(newName) ? normalizeRoleName(newName) : 'name'}</span>
              <input
                value={newName}
                onChange={event => setNewName(event.target.value)}
                placeholder="e.g. code-review"
                aria-label="Role name"
                maxLength={32}
                style={{ background: 'var(--surface-2)', color: 'var(--ink)', border: '1px solid var(--line-strong)', borderRadius: 8, padding: '8px 12px' }}
              />
              {newName !== '' && !isValidRoleName(newName) && (
                <small className="error-text">Lowercase letters, numbers and dashes, like code-review.</small>
              )}
            </label>
            {isValidRoleName(newName) && picker(normalizeRoleName(newName), normalizeRoleName(newName))}
          </div>
        )}
      </div>

    </div>
  )
}
