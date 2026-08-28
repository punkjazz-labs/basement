import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import {
  api, fetchFleetDeployments, idempotency, terminal, formatBytes, formatTokens, runtimeLabel, startTimeoutMinutes,
  modelStateWord, peerModelList,
  updatePlan, installRequest, installConfirmationsComplete, licenceArtifacts, territoryEligibilityLabel, trustLine,
  type FleetDeploymentView, type FleetSummary, type InstalledModel, type Job, type Peer,
  type Preflight,
  type PlacementPlan, type Recipe, type RecipeFeedCheck, type StorageInfo, type PeerSummary, type TokenUsage,
} from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { RECOMMENDED_ID, readableWeights, sortCatalog } from '../catalog'
import { Mark } from '../mark'
import { REVOKE_TITLE, feedNote, revokeBody, revoked, rowRevocation } from '../feed'
import { fleetSummary, MEMBERSHIP_POLL_MS, NO_ANSWER } from '../fleetInvite'
import {
  deploymentActionPath, deploymentIndex, deploymentKey, fleetInstallRequest, fleetRows,
  heldTabLabel, initialPlacement, installRoute, mergePlacements, modelChips, placedTarget,
  placementBusy, placementOptions, placementSwitchFrom, placementTargets, placementVerb,
  placementWord, powerRow,
  recipeBusy, rowActionRoute, rowPlacement,
  shouldShowMemberBanner, splitModels,
  clearRecordBody, clearRecordTitle, releasePath, workingNodes, workingPlacement,
  ACTION_REFUSAL, ADOPT_PATH, CATALOG_EMPTY, CATALOG_TAB, CLEAR_RECORD, CLEAR_RECORD_CONFIRM,
  COOL_TAG, DISRUPTIVE_KINDS, FLEET_DEPLOYMENTS_PATH,
  NO_PLACEMENT_BACK, NO_PLACEMENT_LEFT, PLACEMENT_PLAN_PATH,
  type ActionTarget, type FleetDeploymentAction, type FleetRow, type ModelsTab,
  type PlacementTarget, type PowerRow,
} from '../fleetModels'

// What each model is, not what it is worth. Every line states the same class
// of fact the model catalogs state: dense or mixture of experts, the parameter
// count with the active count where the two differ, what the model reads, and
// the work it is for. Every claim traces to the recipe, to
// docs/MODEL-CANDIDATES-2026-08.md, or to the publisher's own model card.
//
// What a model reads is read from the recipe, never from the card: a card can
// describe a multimodal model that this recipe serves with images off, and the
// line has to state what the install actually serves. So a recipe with
// multimodal_image_limit 0 says it serves text today, and one with a limit
// above 0 says it reads images.
const USE: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s':
    'Mixture of experts, 35B total with 3B active for each token. Reads text and images. The quick daily model for chat, coding help and tool use.',
  'qwen36-27b-nvfp4-1s':
    'Dense 27B tuned for coding and agent work. Reads text and images. Serves with speculative decoding for faster answers.',
  'qwen35-122b-a10b-nvfp4-1s':
    'Mixture of experts, 122B total with 10B active. Serves text today, with a built-in speculative head for faster decoding. 262K context.',
  'qwen38-27b-nvfp4-1s':
    'Dense 27.8B with hybrid attention and built-in vision. Thinks before it answers by default. 262K context.',
  'qwen38-27b-obliterated-q8-0-1s':
    'A community build of Qwen 3.8 27B with the refusal behaviour removed by weight ablation. You own what you make with it.',
  'qwen38-flash-next-nvfp4-2s':
    'Mixture of experts, 176B total with 6B active. Reads text, images and video. 262K context. Runs across both Sparks.',
  'laguna-s-2-1-nvfp4-dflash-1s':
    "poolside's coding model with its DFlash draft companion for faster decoding. Built for long agent runs.",
  'deepseek-v4-flash-0731-2s':
    "DeepSeek's official V4 Flash release in its own weights, FP8 attention with FP4 experts. Runs across both Sparks.",
  'deepseek-v4-flash-0731-ud-iq3-xxs-1s':
    'The same DeepSeek V4 Flash compressed to a 3-bit GGUF so one Spark can hold it alone.',
  'minimax-h3-comfyui-1s':
    'A 33B video model. Makes short clips with sound from a prompt or a source image, through ComfyUI.',
  'nemotron-omni-30b-a3b-nvfp4-1s':
    'Mixture of experts, 31B total with 3B active, built for reasoning. Serves text today. 131K context.',
  'inkling-small-nvfp4-2s':
    'Mixture of experts, 276B total with 12B active. Reads text, images and audio. Runs across both Sparks.',
}
// What a row says about a model this build has written no line for. The feed
// can add a recipe without a console build, and the catalog no longer groups
// by Spark count, so the fallback states the one thing every recipe knows and
// the owner has to plan for: how many machines it needs.
const fallbackUse = (recipe: Recipe): string =>
  recipe.topology.spark_count > 1
    ? `Local model that runs across ${recipe.topology.spark_count} Sparks.`
    : 'Local model for your Spark.'

// Community-reported typical speeds on a DGX Spark, shown until this device
// measures its own number. Each figure traces to a corroborated measurement
// recorded in docs/MODEL-CANDIDATES-2026-08.md.
const REFERENCE_TPS: Record<string, number> = {
  'qwen36-35b-a3b-nvfp4-1s': 80,
  'qwen36-27b-nvfp4-1s': 33,
  'laguna-s-2-1-nvfp4-dflash-1s': 19.4,
  'nemotron-omni-30b-a3b-nvfp4-1s': 57, // 56.94 median, dev.classmethod.jp, Omni NVFP4
  'qwen35-122b-a10b-nvfp4-1s': 28, // 28.3 corroborated baseline, ice-ice-bear bench + NVIDIA forum 365639
  'deepseek-v4-flash-0731-2s': 68, // 67.58 measured on 2x Spark vLLM NVFP4; inside the forum's 42-76 range
  'deepseek-v4-flash-0731-ud-iq3-xxs-1s': 17, // 16.56 measured, dev.classmethod.jp 2026-08-02; the sweep's tweet agrees
}
interface ConfirmState {
  recipe: Recipe
  preflight: Preflight
  switchFrom?: string
}

// The card one strip chip opens is rendered under the strip, so the chip has
// to name it.
const NODE_CARD_ID = 'fleet-node-card'

// No power change is ever running from this screen: the switch lives on the
// Sparks page. The strip still reads each Spark's mode through powerRow, and
// this is the empty run list that read is made against.
const NO_POWER_RUNS: ReadonlySet<string> = new Set()

// The fleet in one line above the table: every Spark, whether it serves
// something, and how much memory and disk it has free. Every number is one
// that Spark reported about itself, and n/a means it has reported none yet.
// Each chip opens that Spark's own card below the line. A Spark in the cool
// mode carries the small "cool" tag: the mode the row shows, which is what
// that Spark reported, or what this console has just set on it while the read
// catches up. A Spark that has reported no mode carries no tag.
function FleetStrip({ sparks, power, open, onOpen }: {
  sparks: FleetRow[]
  power: Map<string, PowerRow>
  open: string
  onOpen: (nodeID: string) => void
}) {
  return (
    <div className="fleet-strip" role="status">
      <span className="fs-label">Fleet</span>
      {sparks.map(spark => (
        <button
          type="button"
          className="fs-node"
          key={spark.nodeID}
          aria-expanded={open === spark.nodeID}
          // Named only while the card is really there: a control that points
          // at nothing is worse than one that points at nothing named.
          aria-controls={open === spark.nodeID ? NODE_CARD_ID : undefined}
          onClick={() => onOpen(spark.nodeID)}
        >
          <i className={`sdot ${spark.status.dot}`} aria-hidden="true" />
          <span className="fs-name">{spark.displayName}</span>
          <b>
            {formatBytes(spark.inventory?.memory_available_bytes)} memory ·{' '}
            {formatBytes(spark.inventory?.storage_available_bytes)} disk
          </b>
          {power.get(spark.nodeID)?.tag && <span className="fs-cool">{COOL_TAG}</span>}
        </button>
      ))}
      <span className="fs-end">{sparks.length} Sparks</span>
    </div>
  )
}

// What one machine has free, as the card states it. A Spark that has reported
// nothing about itself says so once rather than twice.
const sizeLine = (free?: number, total?: number): string =>
  free === undefined && total === undefined ? 'n/a' : `${formatBytes(free)} free of ${formatBytes(total)}`

// One Spark's card: what that machine reported about itself. The power mode
// is set on the Sparks page, where every Spark has a switch of its own; this
// screen is about models, and it only reads the mode for the "cool" tag on
// the strip above.
function NodeCard({ spark }: { spark: FleetRow }) {
  return (
    <div className="node-card" id={NODE_CARD_ID}>
      <header>
        <strong>{spark.displayName}</strong>
        {spark.role && <span className="role">{spark.role}</span>}
      </header>
      <div className="rows">
        <div className="row">
          <span className="k">Memory</span>
          <span className="v">
            {sizeLine(spark.inventory?.memory_available_bytes, spark.inventory?.memory_total_bytes)}
          </span>
        </div>
        <div className="row">
          <span className="k">Disk</span>
          <span className="v">
            {sizeLine(spark.inventory?.storage_available_bytes, spark.inventory?.storage_total_bytes)}
          </span>
        </div>
      </div>
    </div>
  )
}

// The banner a member console shows above its table instead of the fleet
// line: this Spark's models answer to another console, not this one.
// shouldShowMemberBanner (fleetModels.ts) says when to render it; the button
// only appears once the controller has told this Spark where its own console
// answers, so a summary with no URL yet still gets the sentence alone.
function MemberBanner({ controllerConsoleURL }: { controllerConsoleURL: string }) {
  return (
    <div className="fleet-strip" role="status">
      <span>The fleet controller manages this Spark's models.</span>
      {controllerConsoleURL !== '' && (
        <button
          type="button"
          className="ghost"
          onClick={() => window.open(controllerConsoleURL, '_blank', 'noopener,noreferrer')}
        >
          Open the fleet controller
        </button>
      )}
    </div>
  )
}

// Where an install is about to run, on a console that leads no fleet. A
// recipe that needs two Sparks always runs across both, so this choice only
// exists for one-Spark recipes. A console that leads a fleet asks the
// controller for a placement plan instead and offers every Spark in it.
type Placement = 'local' | 'peer'

export default function Models({
  system, recipes, models, jobs, peers, refresh, refreshModelsAndJobs, openDeployment, openFleetDeployment,
  openPlayground, openGenerate, openFleet,
}: AppState) {
  const [confirm, setConfirm] = useState<ConfirmState | null>(null)
  const [licence, setLicence] = useState(false)
  const [territoryEligibility, setTerritoryEligibility] = useState(false)
  // Whether the install switches to the new model as soon as it is ready.
  // Only meaningful when another model is serving; defaults to the
  // historical behaviour so a single click still installs and serves.
  const [activate, setActivate] = useState(true)
  const [pending, setPending] = useState<Set<string>>(new Set())
  const [expanded, setExpanded] = useState('')
  // Which of the two tabs over the table is in front. It opens on the models
  // the owner already has, and it is state of this screen alone: nothing
  // writes it down, so every visit starts on the same tab.
  const [tab, setTab] = useState<ModelsTab>('mine')
  const [storage, setStorage] = useState<StorageInfo | null>(null)
  const [tokens, setTokens] = useState<TokenUsage | null>(null)
  // The feed check under the table. checkingFeed is the button's busy state;
  // feedError only ever holds a problem with the request itself, because a
  // feed this Spark could not reach comes back as feed health and is said by
  // the line beside the button.
  const [checkingFeed, setCheckingFeed] = useState(false)
  const [feedError, setFeedError] = useState('')
  const [placement, setPlacement] = useState<Placement>('local')
  // What the controller answered when it planned this install across the
  // fleet: every Spark it would consider, and the one it recommends. null
  // means the dialog offers the choice it has always offered, which is what a
  // standalone Spark, a fleet member and a plan this console could not read
  // all get.
  const [placementPlan, setPlacementPlan] = useState<PlacementPlan | null>(null)
  // The row of the "Run on" list that is picked now: a node id, or the word
  // for "Choose for me".
  const [placementChoice, setPlacementChoice] = useState('')
  const [peerPreflight, setPeerPreflight] = useState<Preflight | null>(null)
  const [peerChecking, setPeerChecking] = useState(false)
  const [peerError, setPeerError] = useState('')
  const [peerSummary, setPeerSummary] = useState<PeerSummary | null>(null)
  // Recipes this console has asked the paired Spark to install during this
  // session. That Spark only lists a model once its install finishes, and
  // the fleet API has no remote job stream, so this is all the row can
  // honestly say in between. It clears as soon as the model shows up.
  const [delegated, setDelegated] = useState<Set<string>>(new Set())
  const dialogRef = useRef<HTMLDialogElement>(null)
  // Every Spark in the fleet, as the Spark that leads it reports them. null
  // means this console has no fleet to show: a standalone Spark, a member, or
  // a summary this console could not read.
  const [fleet, setFleet] = useState<FleetSummary | null>(null)
  // The same read, kept whatever the role, only for the member banner. fleet
  // above stays controller-only because every row and every poll beneath it
  // already assumes that; the banner needs the member's own answer instead,
  // so it is carried separately rather than by loosening what fleet means.
  const [fleetRoleSummary, setFleetRoleSummary] = useState<FleetSummary | null>(null)
  // Which placement owns which model on which Spark. Row actions on another
  // Spark are sent through it, and its rows also say whether that Spark still
  // answers, so it is polled beside the summary rather than fetched at the
  // moment of a click.
  const [placements, setPlacements] = useState<Map<string, FleetDeploymentView>>(new Map())
  // The last job this console started on each placement. The placement poll
  // and the other Spark's heartbeat are both seconds behind a click, so
  // without this a row would unlock while its own action was still starting
  // and a second click would create a second job.
  const [startedOnPlacement, setStartedOnPlacement] = useState<Map<string, string>>(new Map())
  // Every placement this console has recorded or acted on in this session. It
  // is a ref rather than state because only the placement poll reads it, and
  // it must not restart that poll when it grows.
  const touchedPlacements = useRef<Set<string>>(new Set())
  // Which Spark's card is open under the fleet line. One at a time, and empty
  // for none: it is state of this screen alone.
  const [openSpark, setOpenSpark] = useState('')

  // Storage tells us which recipes already have model files on disk, so a
  // partially downloaded model can offer to resume instead of start over.
  useEffect(() => {
    api<StorageInfo>('/api/v1/storage').then(setStorage).catch(() => {})
  }, [jobs.length])

  // Usage totals only move while a model serves, and the manager samples the
  // runtime on its own slow tick, so this reads them at a similar pace
  // rather than with every render.
  useEffect(() => {
    let cancelled = false
    const read = () => {
      if (document.hidden) return
      api<TokenUsage>('/api/v1/tokens')
        .then(next => {
          if (!cancelled) setTokens(next)
        })
        .catch(() => {})
    }
    read()
    const timer = setInterval(read, 30000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [])

  // The fleet holds one other Spark at most today. Everything this view says
  // about a peer names that one machine, so with any other number it says
  // nothing rather than picking a Spark to speak for.
  const peer: Peer | undefined = peers.length === 1 ? peers[0] : undefined
  const peerID = peer?.id

  // The same merged read the Fleet tab polls, for the same reason: it is the
  // only source for what the other Spark has installed and is serving.
  useEffect(() => {
    if (!peerID) {
      setPeerSummary(null)
      return
    }
    let cancelled = false
    const sample = async () => {
      if (document.hidden) return
      try {
        const next = await api<PeerSummary>(`/api/v1/peers/${encodeURIComponent(peerID)}/summary`)
        if (!cancelled) setPeerSummary(next)
      } catch {
        if (!cancelled) setPeerSummary({ reachable: false })
      }
    }
    sample()
    const timer = setInterval(sample, 10000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [peerID])

  // The fleet-wide table is a controller feature: only the console of the
  // Spark that leads the fleet holds rows for the other Sparks, so only that
  // console keeps reading. Every other console asks once, learns it leads
  // nothing, and stops there.
  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setInterval> | undefined
    // The first read always runs, because a tab that opens in the background
    // still has to learn what this console leads. Later rounds skip a hidden
    // tab, as every other poll in the console does.
    const read = async (first = false) => {
      if (!first && document.hidden) return
      try {
        const summary = fleetSummary(await api<unknown>('/api/v1/fleet'))
        if (cancelled) return
        setFleetRoleSummary(summary)
        const controller = summary !== null && summary.role === 'controller'
        setFleet(controller ? summary : null)
        if (controller && timer === undefined) timer = setInterval(read, MEMBERSHIP_POLL_MS)
      } catch {
        if (!cancelled) {
          setFleet(null)
          setFleetRoleSummary(null)
        }
      }
    }
    void read(true)
    return () => {
      cancelled = true
      if (timer !== undefined) clearInterval(timer)
    }
  }, [])

  const leadsFleet = fleet !== null

  useEffect(() => {
    if (!leadsFleet) return
    let cancelled = false
    const read = async (first = false) => {
      if (!first && document.hidden) return
      try {
        const deployments = await fetchFleetDeployments()
        if (!cancelled) {
          setPlacements(previous =>
            mergePlacements(deploymentIndex(deployments), previous, touchedPlacements.current))
        }
      } catch {
        /* the next read tries again */
      }
    }
    void read(true)
    const timer = setInterval(read, MEMBERSHIP_POLL_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [leadsFleet])

  const peerModels = peerModelList(peerSummary)
  const peerInstalled = new Map(peerModels.map(model => [model.recipe_id, model]))
  // What the paired Spark holds, for the tab split below. That Spark is in no
  // fleet, so no fleet row speaks for it and only this summary knows what
  // lives there. The rows already read it, so the tabs have to read it too.
  const peerRecipeIDs = useMemo(
    () => new Set(peerModels.map(model => model.recipe_id)),
    [peerSummary], // eslint-disable-line react-hooks/exhaustive-deps
  )

  // A delegated install has landed once the peer lists that model itself;
  // from then on its own status is the truthful thing to show. The map read
  // here is derived from this very summary, so it is always the fresh one.
  useEffect(() => {
    setDelegated(previous => {
      if (previous.size === 0) return previous
      const next = new Set([...previous].filter(id => !peerInstalled.has(id)))
      return next.size === previous.size ? previous : next
    })
  }, [peerSummary]) // eslint-disable-line react-hooks/exhaustive-deps

  // Every Spark this console can speak for, in table order. Only the console
  // that leads the fleet has more than its own machine to show, so on every
  // other console the fleet line and the chips below stay off and the screen
  // reads exactly as it did before the fleet existed.
  // Read at the moment the summary arrives, which is the only moment any of
  // it is fresh: one row's answer depends on how old its heartbeat is, and
  // the summary this reads is replaced on every poll.
  const sparks = useMemo(() => fleetRows(fleet, peers, window.location.origin, Date.now()), [fleet, peers])
  const showFleet = leadsFleet && sparks.length > 1
  // What each Spark's power mode says on the strip: the mode that Spark
  // reported, read through the same row the switch on the Sparks page reads.
  // Nothing is set from this screen, so no answer of this console's own is
  // held here and no change can be running.
  const powerRows = useMemo(
    () => new Map(sparks.map(spark => [spark.nodeID, powerRow(spark, undefined, NO_POWER_RUNS)])),
    [sparks],
  )
  // The Spark whose card is open. A Spark that has left the fleet between the
  // click and this render has no row, and so no card.
  const openRow = sparks.find(spark => spark.nodeID === openSpark)
  const showMemberBanner = shouldShowMemberBanner(fleetRoleSummary)

  // Which Sparks hold this model, and which one serves it now. The Spark this
  // console runs on is named only while another Spark is on screen beside it.
  const nodeChips = (recipe: Recipe) =>
    showFleet ? modelChips(sparks, recipe.id, recipe.topology.spark_count) : []

  const installed = useMemo(() => new Map(models.map(model => [model.recipe_id, model])), [models])
  // The Spark that holds this model, when the Spark this console runs on does
  // not. A model on both machines keeps this Spark in front, exactly as the
  // status line already reads it. A Spark added by address only is never one
  // of these: the fleet holds no placement for it, so nothing here can act on
  // it. A model that needs more than one Spark is not one of these either: it
  // runs across the fleet rather than on a machine that could be named.
  const hostOf = (recipe: Recipe): FleetRow | undefined => {
    if (!showFleet || installed.has(recipe.id) || recipe.topology.spark_count !== 1) return undefined
    return sparks.find(spark =>
      !spark.isSelf && !spark.legacyPeerOnly &&
      spark.installedModels.some(model => model.recipe_id === recipe.id))
  }
  // The model and the machine one row acts on. Without a host the row acts on
  // this Spark, which is what it has always done. The host row travels with
  // the target because it carries what that Spark reported about itself,
  // which is what says whether the fleet can adopt the model it runs.
  const targetOf = (recipe: Recipe, host?: FleetRow): ActionTarget =>
    ({ nodeID: host?.nodeID ?? '', recipeID: recipe.id, isSelf: host === undefined, host })

  // Whether this console can offer the whole fleet in the install dialog.
  // Only the Spark that leads the fleet plans placements, only a fleet of
  // more than one Spark has a choice to make, and a controller that cannot
  // name its own node could not tell a local install from a remote one.
  const canPlaceAcrossFleet =
    fleet !== null && fleet.nodes.length > 1 && fleet.controller_node_id !== ''
  // The Sparks the fleet is already working on the planned model on. The plan
  // reads heartbeats, and a Spark names a model in one only once its install
  // has finished, so without this the list would offer a machine that is
  // downloading this very model right now.
  const placementBusyNodes = useMemo(
    () => workingNodes(placements, startedOnPlacement, placementPlan?.recipe_id ?? '', DISRUPTIVE_KINDS),
    [placements, startedOnPlacement, placementPlan],
  )
  // The "Run on" list, resolved once against both the plan and the fleet
  // table. Everything the dialog shows about a Spark and everything it sends
  // for that Spark read this same list, so the screen and the request cannot
  // name two different machines.
  const placementList = useMemo(
    () => placementTargets(placementPlan, fleet, sparks, fleet?.controller_node_id ?? '', placementBusyNodes),
    [placementPlan, fleet, sparks, placementBusyNodes],
  )
  // The Spark the list points at now. "Choose for me" resolves here.
  const placementTarget = placedTarget(
    placementChoice, placementList, placementPlan?.recommended_node_id)
  const usage = useMemo(() => new Map((tokens?.models ?? []).map(item => [item.recipe_id, item])), [tokens])
  const sorted = useMemo(() => sortCatalog(recipes), [recipes])
  const detected = system?.hardware_scope.detected_spark_count ?? 0
  // Every Spark this console can install on: the one it runs on, plus the
  // ones paired on the Fleet tab. Pairing a second Spark is what makes a
  // two-Spark recipe installable; the distributed preflight still runs when
  // the install starts and can still refuse.
  const available = detected + peers.length
  const fitsOn = (recipe: Recipe) => available >= recipe.topology.spark_count
  // Short of exactly one machine, with none paired yet: something the user
  // can fix right now rather than a dead end.
  const pairable = (recipe: Recipe) =>
    !fitsOn(recipe) && peers.length === 0 && detected >= 1 && detected + 1 >= recipe.topology.spark_count
  const blockers = system?.blocking_conditions ?? []
  const activeOther = (id: string) => models.find(model => model.active && model.recipe_id !== id)
  const nameOf = (id?: string) => recipes.find(recipe => recipe.id === id)?.display_name ?? id ?? ''
  const downloadedBytes = (recipe: Recipe) =>
    storage?.artifacts.filter(a => a.recipe_ids.includes(recipe.id)).reduce((sum, a) => sum + a.bytes, 0) ?? 0
  // Label honesty for a not-installed model with files already on disk:
  // partial data resumes, a kept complete download reinstalls quickly. An
  // already-installed model whose recipe has since moved to a newer
  // version reads as an update, not a fresh install, whether or not it is
  // currently serving.
  const installVerb = (recipe: Recipe) => {
    const own = installed.get(recipe.id)
    if (own) return own.recipe_version < recipe.version ? 'Update' : 'Install'
    const bytes = downloadedBytes(recipe)
    if (bytes <= 0) return 'Install'
    return bytes >= recipe.artifact_bytes * 0.99 ? 'Reinstall' : 'Resume install'
  }
  // On the other Spark this console only knows what that Spark reports:
  // whether it has the model, and at which recipe version. Partial downloads
  // over there are its own business, so a resume is never claimed.
  const peerInstallVerb = (recipe: Recipe) => {
    const there = peerInstalled.get(recipe.id)
    return there && there.recipe_version < recipe.version ? 'Update' : 'Install'
  }
  const verbFor = (recipe: Recipe, where: Placement) =>
    where === 'peer' ? peerInstallVerb(recipe) : installVerb(recipe)
  const setBusy = (id: string, busy: boolean) => {
    setPending(previous => {
      const next = new Set(previous)
      if (busy) next.add(id)
      else next.delete(id)
      return next
    })
  }

  const acceptJob = (result: { job: Job }) => {
    openDeployment(result.job.id)
    refreshModelsAndJobs()
  }

  // Ask the feed now instead of waiting for the next scheduled check. The
  // manager answers with the feed health the check produced; this screen then
  // reads the system and the catalog again, so a newer index moves the line
  // under the table and the rows above it together.
  const checkForRecipes = async () => {
    if (checkingFeed) return
    setCheckingFeed(true)
    setFeedError('')
    try {
      await api<RecipeFeedCheck>('/api/v1/recipes/refresh', { method: 'POST' })
      await refresh()
    } catch (problem) {
      setFeedError(problem instanceof Error ? problem.message : 'The check did not work.')
    } finally {
      setCheckingFeed(false)
    }
  }

  const run = async (id: string, work: () => Promise<void>) => {
    if (pending.has(id)) return
    setBusy(id, true)
    try {
      await work()
    } catch (problem) {
      noticeBox('That did not work', problem instanceof Error ? problem.message : String(problem))
    } finally {
      setBusy(id, false)
    }
  }

  // Which Sparks the controller would place this model on. A model that needs
  // more than one Spark runs across the whole fleet and has no placement to
  // choose, so it is never planned. A plan this console cannot read leaves
  // the dialog exactly as it was before the fleet existed, which is why the
  // refusal is answered with null rather than raised.
  const placementPlanFor = async (recipe: Recipe): Promise<PlacementPlan | null> => {
    if (!canPlaceAcrossFleet || recipe.topology.spark_count !== 1) return null
    try {
      return await api<PlacementPlan>(PLACEMENT_PLAN_PATH, {
        method: 'POST',
        body: JSON.stringify({ recipe_id: recipe.id }),
      })
    } catch {
      return null
    }
  }

  const startInstall = (recipe: Recipe) =>
    run(recipe.id, async () => {
      const preflight = await api<Preflight>(`/api/v1/preflight?recipe_id=${encodeURIComponent(recipe.id)}`)
      // switchFrom drives the "switch now vs later" choice below. It is
      // either a different model actively serving, or this same model
      // updating itself while it is the one actively serving — both stop
      // something before the new download can take over.
      const own = installed.get(recipe.id)
      const switchFrom = activeOther(recipe.id)?.recipe_id ?? (own?.active ? recipe.id : undefined)
      const plan = await placementPlanFor(recipe)
      setPlacementPlan(plan)
      setPlacementChoice(initialPlacement(
        placementTargets(plan, fleet, sparks, fleet?.controller_node_id ?? '',
          workingNodes(placements, startedOnPlacement, recipe.id, DISRUPTIVE_KINDS)),
        plan?.recommended_node_id))
      setConfirm({ recipe, preflight, switchFrom })
      setLicence(false)
      setTerritoryEligibility(false)
      setActivate(true)
      setPlacement('local')
      setPeerPreflight(null)
      setPeerError('')
      setPeerChecking(false)
      requestAnimationFrame(() => dialogRef.current?.showModal())
    })

  // Every machine answers its own preflight, so choosing the other Spark
  // means asking that Spark before any number in the dialog can be trusted.
  const choosePlacement = (where: Placement) => {
    setPlacement(where)
    if (where === 'local' || !confirm || !peer || peerPreflight || peerChecking) return
    setPeerChecking(true)
    setPeerError('')
    const path = `/api/v1/peers/${encodeURIComponent(peer.id)}/preflight?recipe_id=${encodeURIComponent(confirm.recipe.id)}`
    api<Preflight>(path)
      .then(setPeerPreflight)
      .catch(problem => setPeerError(problem instanceof Error ? problem.message : `Could not reach ${peer.name}`))
      .finally(() => setPeerChecking(false))
  }

  // Which model has to give way, on the machine the install targets. Here it
  // was worked out when the dialog opened; on the other Spark it comes from
  // that Spark's own model list.
  const switchFromFor = (where: Placement): string | undefined => {
    if (!confirm) return undefined
    if (where === 'local') return confirm.switchFrom
    const other = peerModels.find(model => model.active && model.recipe_id !== confirm.recipe.id)
    if (other) return other.recipe_id
    return peerInstalled.get(confirm.recipe.id)?.active ? confirm.recipe.id : undefined
  }

  // An install the fleet carries to another Spark. The controller plans it,
  // reserves that machine, starts the job there and answers with the
  // placement it made, so the progress window follows that Spark's own job
  // through the placement stream every other fleet action already uses.
  const installOnFleetSpark = async (recipe: Recipe, spark: PlacementTarget) => {
    const answer = await api<{ deployment?: FleetDeploymentView }>(FLEET_DEPLOYMENTS_PATH, {
      method: 'POST',
      headers: idempotency(),
      body: JSON.stringify(fleetInstallRequest(recipe.id, spark.nodeID, installRequest(
        licence,
        territoryEligibility,
        placementSwitchFrom(spark) ? activate : true,
      ))),
    })
    const deployment = answer.deployment
    const job = deployment?.job
    // The window below follows one job on one placement. An answer carrying
    // neither is not one this console can go on with, and saying so plainly
    // beats opening a window on nothing.
    if (!deployment?.deployment_id || !job) throw new Error(NO_PLACEMENT_BACK)
    const deploymentID = deployment.deployment_id
    dialogRef.current?.close()
    setConfirm(null)
    // The row locks on the placement it just gained, without waiting for the
    // next placement poll to report it.
    touchedPlacements.current.add(deploymentID)
    setPlacements(previous => new Map(previous).set(deploymentKey(spark.nodeID, recipe.id), deployment))
    setStartedOnPlacement(previous => new Map(previous).set(deploymentID, job.id))
    openFleetDeployment(deploymentID, job)
  }

  const confirmInstall = () =>
    confirm &&
    run(confirm.recipe.id, async () => {
      const recipe = confirm.recipe
      if (!installConfirmationsComplete(recipe, licence, territoryEligibility)) return
      // The fleet-wide list is in front, so this install goes where it
      // points. A Spark that is not this one is installed on through the
      // fleet, which reserves it there and answers with that Spark's own job.
      if (placementPlan && recipe.topology.spark_count === 1) {
        const route = installRoute(placementChoice, placementList, placementPlan.recommended_node_id)
        if (route.where === 'none') throw new Error(NO_PLACEMENT_LEFT)
        if (route.where === 'fleet') {
          await installOnFleetSpark(recipe, route.target)
          return
        }
        // A local route falls through to the install call below, unchanged.
      }
      // The picker is only ever shown with a peer in hand; without one there
      // is nothing to delegate to, and quietly installing here instead would
      // not be what was asked for.
      if (placement === 'peer' && !peer) return
      const body = JSON.stringify(installRequest(
        licence,
        territoryEligibility,
        switchFromFor(placement) ? activate : true,
      ))
      if (placement === 'peer' && peer) {
        await api<{ job?: Job }>(
          `/api/v1/peers/${encodeURIComponent(peer.id)}/models/${encodeURIComponent(recipe.id)}/install`,
          { method: 'POST', headers: idempotency(), body },
        )
        dialogRef.current?.close()
        setConfirm(null)
        setDelegated(previous => new Set(previous).add(recipe.id))
        noticeBox(
          `${peer.name} is installing ${recipe.display_name}`,
          `Progress shows on ${peer.name}'s own console.`,
        )
        return
      }
      const result = await api<{ job: Job }>(`/api/v1/models/${recipe.id}/install`, {
        method: 'POST',
        headers: idempotency(),
        body,
      })
      dialogRef.current?.close()
      setConfirm(null)
      acceptJob(result)
    })

  // One action on one model, sent to the Spark that holds it. This Spark keeps
  // the call it has always used. Another Spark is reached through the
  // placement that owns the model on it, and answers with its own job, which
  // the placement's own stream then follows. A model that Spark already runs
  // with no placement behind it is adopted first, which records it and starts
  // nothing, and then the action goes out exactly as it would on any other
  // placed row.
  const sendAction = async (
    recipe: Recipe,
    host: FleetRow | undefined,
    action: FleetDeploymentAction,
    body: string,
    local: () => Promise<{ job: Job }>,
  ) => {
    const route = rowActionRoute(targetOf(recipe, host), placements, action)
    if (route.where === 'local') {
      acceptJob(await local())
      return
    }
    const dispatch = async (deploymentID: string, path: string) => {
      touchedPlacements.current.add(deploymentID)
      const result = await api<{ job: Job }>(path, { method: 'POST', headers: idempotency(), body })
      setStartedOnPlacement(previous => new Map(previous).set(deploymentID, result.job.id))
      openFleetDeployment(deploymentID, result.job)
    }
    if (route.where === 'fleet') {
      await dispatch(route.deploymentID, route.path)
      return
    }
    // The first action on a model this fleet never placed. The manager
    // refuses an adoption it cannot make, in its own words, and run() shows
    // those words unchanged rather than replacing them with a guess.
    if (route.where === 'adopt') {
      const answer = await api<{ deployment?: FleetDeploymentView }>(ADOPT_PATH, {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ node_id: route.nodeID, recipe_id: route.recipeID }),
      })
      const deployment = answer.deployment
      // Everything below acts on a record. An answer carrying none is not one
      // this console can go on with, and saying so plainly beats a broken
      // path built from nothing.
      if (!deployment?.deployment_id) throw new Error(NO_PLACEMENT_BACK)
      // From here the row reads the placement it just gained, so it locks on
      // the action below without waiting for the next placement poll.
      touchedPlacements.current.add(deployment.deployment_id)
      setPlacements(previous => new Map(previous).set(deploymentKey(route.nodeID, route.recipeID), deployment))
      // An adoption can find a Spark that has since gone quiet. The record is
      // real and the row keeps it, which is what makes the row read "Not
      // answering" from here; the action itself would only fail, so it is not
      // sent.
      if (deployment.stale) throw new Error(ACTION_REFUSAL['not-answering'])
      await dispatch(deployment.deployment_id, deploymentActionPath(deployment.deployment_id, action))
      return
    }
    // A row only offers a button the route allows, so anything else here is
    // the fleet moving between the render and the click. run() turns this into
    // the same notice every other refused action gets.
    throw new Error(ACTION_REFUSAL[route.reason])
  }

  const simpleAction = (recipe: Recipe, host: FleetRow | undefined, action: FleetDeploymentAction) =>
    run(recipe.id, () =>
      sendAction(recipe, host, action, '{}', () =>
        api<{ job: Job }>(`/api/v1/models/${recipe.id}/${action}`, {
          method: 'POST',
          headers: idempotency(),
          body: '{}',
        })))

  const startOrSwitch = async (recipe: Recipe, host?: FleetRow) => {
    // Which model has to give way, on the machine this start runs on.
    const active = host
      ? (host.serving && host.serving.recipe_id !== recipe.id ? host.serving.recipe_id : '')
      : activeOther(recipe.id)?.recipe_id ?? ''
    if (active) {
      const from = recipes.find(item => item.id === active)?.display_name ?? active
      const { ok } = await confirmBox({
        title: `Switch to ${recipe.display_name}?`,
        body: `${from} will stop. Basement restores it if the new model fails.`,
        confirmLabel: 'Switch model',
      })
      if (!ok) return
    }
    simpleAction(recipe, host, 'start')
  }

  const remove = async (recipe: Recipe, host?: FleetRow) => {
    const serving = host ? host.serving?.recipe_id === recipe.id : installed.get(recipe.id)?.active
    const { ok, checked } = await confirmBox({
      title: `Uninstall ${recipe.display_name}?`,
      body: serving
        ? 'It will stop first. The runtime and config are removed.'
        : 'The runtime and configuration are removed.',
      confirmLabel: 'Uninstall',
      danger: true,
      checkbox: {
        label: `Also delete ${formatBytes(recipe.artifact_bytes)} of downloaded model files`,
        note: 'Faster reinstall later.',
      },
    })
    if (!ok) return
    run(recipe.id, () =>
      // The fleet API takes the same choice about the downloaded files. It
      // takes no reclaim figure, because only the machine holding those files
      // can count them.
      sendAction(recipe, host, 'remove', JSON.stringify({ remove_artifacts: checked }), () =>
        api<{ job: Job }>(`/api/v1/models/${recipe.id}`, {
          method: 'DELETE',
          headers: idempotency(),
          body: JSON.stringify({
            remove_artifacts: checked,
            expected_reclaim_bytes: checked ? recipe.artifact_bytes : 0,
          }),
        })))
  }

  // The one repair for a row the fleet can no longer read. A stale placement
  // pins that row to "No answer" and kills every button on it, and adoption
  // cannot write a fresh record while the stale one stands. Removing the
  // placement ends it, and the row can then be rebuilt from what that Spark
  // reports.
  //
  // This is deliberately not sendAction: rowActionRoute refuses a stale
  // placement, which is exactly the state this tool exists for. It is offered
  // only while the placement really is stale.
  //
  // The remove goes first, because while that Spark still answers, removing
  // the model is the honest thing to ask for. When the call cannot be
  // addressed at all, the record is ended on the controller instead. The
  // manager refuses that for a record it can still read, so a remove that
  // failed for a reason of its own keeps its own words rather than being
  // quietly turned into a cleared record.
  const clearRecord = async (
    recipe: Recipe, host: FleetRow, placement: FleetDeploymentView,
  ) => {
    const { ok } = await confirmBox({
      title: clearRecordTitle(recipe.display_name),
      body: clearRecordBody(host.displayName),
      confirmLabel: CLEAR_RECORD_CONFIRM,
      danger: true,
    })
    if (!ok) return
    run(recipe.id, async () => {
      touchedPlacements.current.add(placement.deployment_id)
      try {
        const result = await api<{ job: Job }>(
          deploymentActionPath(placement.deployment_id, 'remove'),
          { method: 'POST', headers: idempotency(), body: JSON.stringify({ remove_artifacts: false }) },
        )
        setStartedOnPlacement(previous => new Map(previous).set(placement.deployment_id, result.job.id))
        openFleetDeployment(placement.deployment_id, result.job)
      } catch (unreachable) {
        let released: FleetDeploymentView | undefined
        try {
          const answer = await api<{ deployment?: FleetDeploymentView }>(
            releasePath(placement.deployment_id), { method: 'POST', headers: idempotency(), body: '{}' })
          released = answer.deployment
        } catch {
          throw unreachable
        }
        // The record is over. The row reads the released record at once, so it
        // lets go without waiting for the next placement poll, and the Spark's
        // own heartbeat speaks for the model again.
        if (released) {
          setPlacements(previous =>
            new Map(previous).set(deploymentKey(host.nodeID, recipe.id), released))
        }
        refreshModelsAndJobs()
      }
    })
  }

  const preflightBlockers = (preflight: Preflight): string[] => {
    const list = preflight.checks.filter(check => !check.ok).map(check => check.error ?? check.operation)
    for (const [name, present] of Object.entries(preflight.secrets)) if (!present) list.push(`${name} is missing`)
    return list
  }
  // Two installs of different recipes are allowed to run at once; the
  // per-recipe guard already covers the same recipe, so this only needs to
  // know whether a different recipe has one in flight.
  const anotherInstallRunning = confirm
    ? jobs.some(job => job.kind === 'install' && job.recipe_id !== confirm.recipe.id && !terminal(job.state))
    : false
  const reclaimFrom = (preflight: Preflight) =>
    preflight.checks.find(check => !check.ok && check.operation === 'verify_disk')?.receipt?.reclaim_candidates

  // The two tabs over the table: the models the owner already has, and the
  // models the owner can still get. Both keep the curated order they arrive
  // in, and every row below renders the same way whichever tab holds it.
  const localRecipeIDs = useMemo(() => new Set(installed.keys()), [installed])
  const split = splitModels(sorted, localRecipeIDs, peerRecipeIDs, sparks)
  // Nothing is installed on any machine this console speaks for. That, and
  // only that, is the first run: it keeps the hero and the one plain table it
  // has always had, and it shows no tabs. The hero and the tabs read the same
  // signal, so they can never both be on screen. The split above is read
  // before this, and over the whole catalog rather than over the rows below,
  // because the rows are what the hero has already taken its model out of.
  const firstRun = split.held.length === 0
  const showTabs = !firstRun
  // A recipe the publisher has withdrawn is never the first thing a new owner
  // is offered. It keeps its place in the table below, where the row says it
  // is revoked and why, instead of being recommended and then refused.
  const featured = firstRun ? sorted.find(recipe => recipe.id === RECOMMENDED_ID && !recipe.revoked) : undefined
  const rows = featured ? sorted.filter(recipe => recipe.id !== featured.id) : sorted

  const rowFor = (recipe: Recipe) => {
    const model = installed.get(recipe.id)
    // Which of the two revocation surfaces this row is in, if either: a
    // withdrawn version nobody here installed, or a withdrawn version that is
    // installed and carries on exactly as before.
    const revocation = rowRevocation(recipe, model)
    const isMedia = Boolean(recipe.media_generation)
    // Only jobs that change what is running should lock the row. Smoke tests
    // and benchmarks run against a serving model — Open must stay available.
    const running = (kinds: (kind: string) => boolean) =>
      jobs.some(job => job.recipe_id === recipe.id && !terminal(job.state) && kinds(job.kind))
    // A placement anywhere in the fleet that is still working on this model.
    // It is the only thing that knows a remote install started: the Spark
    // running it names the model in a heartbeat only once the install has
    // finished, so without this a row with no model here would keep a live
    // Install button for the whole download, and a second click would start a
    // second install.
    const working = workingPlacement(placements, startedOnPlacement, recipe.id, DISRUPTIVE_KINDS)
    const workingSpark = working ? sparks.find(spark => spark.nodeID === working.owner_node_id) : undefined
    // A model this Spark holds answers to this Spark's own work and nothing
    // else, so an install running on another Spark never takes its buttons
    // away. See recipeBusy.
    const localBusy = pending.has(recipe.id) || running(kind => DISRUPTIVE_KINDS.has(kind))
    const busy = recipeBusy(localBusy, model !== undefined, working)
    const measuring = running(kind => kind === 'benchmark' || kind === 'smoke-test')
    const isActive = Boolean(model?.active && model.status === 'ready')
    const fits = fitsOn(recipe)
    const canPair = pairable(recipe)
    // The Spark in the fleet that holds this model when this one does not, the
    // placement this console acts on it through, and what that Spark last
    // reported about it.
    const host = hostOf(recipe)
    const hostModel = host?.installedModels.find(item => item.recipe_id === recipe.id)
    const placement = host ? rowPlacement(targetOf(recipe, host), placements) : undefined
    // That Spark has stopped answering, so every state below is only the last
    // one this console was told. A placement carries the fresher answer: the
    // controller read that Spark for it moments ago. Without one, the Spark's
    // own heartbeat is the only signal there is, and it is the same one the
    // route reads before it offers to adopt.
    const noAnswer = placement ? placement.stale === true : host !== undefined && !host.answering
    const hostServing = Boolean(hostModel?.active && hostModel.status === 'ready')
    const hostWord = noAnswer ? NO_ANSWER : hostModel ? modelStateWord(hostModel) : ''
    // What the paired Spark says about this same model. Its own word for its
    // own state, plus the one thing this console knows that it cannot see
    // yet: an install this session asked it to run.
    const peerModel: InstalledModel | undefined = peerInstalled.get(recipe.id)
    const peerWord = peerModel ? modelStateWord(peerModel) : delegated.has(recipe.id) ? 'Installing' : ''
    // What another Spark says about this same model, and how to reach it. The
    // fleet speaks for one of its own members, because it also knows whether
    // that member still answers; a Spark added by address only still speaks
    // for itself.
    // A Spark the fleet is working on speaks through its placement until it
    // names the model itself, which is what puts the work on screen while an
    // install runs there.
    const otherName = host ? host.displayName : workingSpark?.displayName ?? peer?.name ?? ''
    const otherURL = host ? host.consoleURL : workingSpark?.consoleURL ?? peer?.base_url ?? ''
    const otherWord = host ? hostWord : working ? placementWord(working) : peerWord
    const otherServing = host
      ? hostServing && !noAnswer
      : Boolean(peerModel?.active && peerModel.status === 'ready')
    const otherBusy = working !== undefined ||
      otherWord === 'Installing' || otherWord === 'Starting' || otherWord === 'Switching'
    // No button on another Spark's row while that Spark is already changing
    // this model, and none at all while it does not answer for it. The
    // placement locks the row from the moment the action is accepted; the
    // heartbeat word only follows a few seconds later.
    const hostLocked = busy || otherBusy || noAnswer ||
      placementBusy(placement, placement && startedOnPlacement.get(placement.deployment_id))
    const localStatus = busy ? 'Working' : isActive ? (measuring ? 'Serving · measuring' : 'Serving') : model ? 'Installed' : 'Not installed'
    // Nothing of this recipe runs anywhere in the fleet and its version has
    // been withdrawn, so the row's whole state is the revocation. A Spark
    // that is running it keeps its own word instead: what a machine is doing
    // outranks what a publisher has decided.
    const readsRevoked = revocation.revoked && !otherWord
    // A model that lives only on the other Spark reads as that Spark's
    // status; one that lives on both keeps this Spark's status in front.
    const statusText = readsRevoked ? 'Revoked' : !model && otherWord ? otherWord : localStatus
    const otherNote = otherName && otherWord ? (!model ? `on ${otherName}` : `${otherWord} on ${otherName}`) : ''
    // Serving is serving, whichever Spark is doing it; the annotation says
    // which one.
    const dotClass = readsRevoked ? 'fail' : isActive || otherServing ? 'on' : busy || otherBusy ? 'busy' : ''
    const measured = model?.tokens_per_second
    const reference = REFERENCE_TPS[recipe.id]
    // Counting happens only while basement serves the model on this Spark,
    // so a model without a reading yet says so instead of showing zeros.
    const served = usage.get(recipe.id)
    const servedTotal = served ? served.prompt_tokens + served.generation_tokens : 0
    const updateAvailable = Boolean(model && model.recipe_version < recipe.version)
    const chips = nodeChips(recipe)
    const open = expanded === recipe.id
    const toggle = () => setExpanded(open ? '' : recipe.id)
    // Buttons act without toggling the row; empty space anywhere else in the
    // row — including inside the actions column — expands it.
    const act = (work: () => void) => (event: React.MouseEvent) => {
      event.stopPropagation()
      work()
    }
    return (
      <Fragment key={recipe.id}>
        <div
          className={`mrow ${open ? 'open' : ''}`}
          role="button"
          tabIndex={0}
          aria-expanded={open}
          onClick={toggle}
          onKeyDown={event => {
            if (event.target === event.currentTarget && (event.key === 'Enter' || event.key === ' ')) {
              event.preventDefault()
              toggle()
            }
          }}
        >
          <div className="m-id">
            <Mark recipe={recipe} recipeIDs={[recipe.id]} size={28} />
            <div>
              <div className="nm">
                {recipe.display_name}{' '}
                {revocation.revoked && <span className="tag revoked">Revoked</span>}
                {/* Which Sparks in the fleet hold this model. The lit chip is
                    the one serving it now. */}
                {chips.map(chip => (
                  <span key={chip.key} className={`node-chip ${chip.live ? 'live' : ''}`}>
                    <i aria-hidden="true" />
                    {chip.name}
                  </span>
                ))}
              </div>
              <div className="use">{USE[recipe.id] ?? fallbackUse(recipe)}</div>
            </div>
          </div>
          <div className="m-num">
            <span className="n">
              {isMedia ? 'n/a' : measured ? measured.toFixed(1) : reference ? `~${reference}` : 'n/a'}
              {!isMedia && <small>tok/s</small>}
            </span>
            <span className={`sub ${measured && !isMedia ? 'ok' : ''}`}>
              {isMedia ? 'media generation' : measured ? 'measured here' : 'typical'}
            </span>
          </div>
          <div className="m-num">
            <span className="n">{formatBytes(recipe.artifact_bytes)}</span>
          </div>
          <div className="m-status">
            <span className={`sdot ${dotClass}`} aria-hidden="true" />
            <span>
              {statusText}
              {otherNote && <small className="peer-note">{otherNote}</small>}
              {/* The status above is true and stays true: this only adds what
                  the publisher has since said about the version it runs. */}
              {revocation.installedRevoked && <small className="peer-note warn">Recipe revoked</small>}
            </span>
          </div>
          <div className="m-actions" onKeyDown={event => event.stopPropagation()}>
            {/* This model lives on another Spark in the fleet, so the row acts
                on that Spark. The fleet may hold no placement for it yet; the
                first action records one. While that Spark does not answer,
                its buttons stay in place and stay dead: the row says what it
                last knew, and promises nothing. */}
            {host ? (
              <>
                {hostServing ? (
                  <button
                    className="ghost"
                    disabled={hostLocked}
                    onClick={act(() => simpleAction(recipe, host, 'stop'))}
                  >
                    Stop
                  </button>
                ) : (
                  <button
                    className="primary"
                    disabled={hostLocked}
                    onClick={act(() => startOrSwitch(recipe, host))}
                  >
                    {host.serving && host.serving.recipe_id !== recipe.id ? 'Switch to' : 'Start'}
                  </button>
                )}
                {/* The playground and the generate tab only reach the model
                    this Spark serves, so a live model on another Spark is
                    opened on that Spark's own console. */}
                {otherServing && otherURL && (
                  <button
                    className="primary"
                    onClick={act(() => window.open(otherURL, '_blank', 'noopener,noreferrer'))}
                  >
                    Open on {otherName}
                  </button>
                )}
              </>
            ) : (
              <>
                {otherURL && otherWord && (
                  <button
                    className="ghost"
                    onClick={act(() => window.open(otherURL, '_blank', 'noopener,noreferrer'))}
                  >
                    Open on {otherName}
                  </button>
                )}
                {/* A withdrawn version is refused by the manager, so the button
                    that would only fail is offered dead rather than hidden: the
                    row still says what this model is and why it cannot start. */}
                {!model && (fits || !canPair || revocation.installBlocked) && (
                  <button
                    className="primary"
                    disabled={busy || !fits || revocation.installBlocked}
                    onClick={act(() => startInstall(recipe))}
                  >
                    {busy ? 'Working' : fits || revocation.installBlocked ? installVerb(recipe) : 'Needs a Spark'}
                  </button>
                )}
                {!model && !fits && canPair && !revocation.installBlocked && (
                  <button className="primary" onClick={act(openFleet)}>Pair a second Spark</button>
                )}
                {model && isActive && (
                  <>
                    <button className="ghost" disabled={busy} onClick={act(() => simpleAction(recipe, undefined, 'stop'))}>Stop</button>
                    {updateAvailable && (
                      <button
                        className="ghost"
                        disabled={busy || revocation.installBlocked}
                        onClick={act(() => startInstall(recipe))}
                      >
                        Update
                      </button>
                    )}
                    <button className="primary" disabled={busy} onClick={act(isMedia ? openGenerate : openPlayground)}>
                      {isMedia ? 'Generate' : 'Open'}
                    </button>
                  </>
                )}
                {model && !isActive && model.status !== 'recovering' && (
                  <>
                    {updateAvailable && (
                      <button
                        className="ghost"
                        disabled={busy || revocation.installBlocked}
                        onClick={act(() => startInstall(recipe))}
                      >
                        Update
                      </button>
                    )}
                    <button className="primary" disabled={busy} onClick={act(() => startOrSwitch(recipe))}>
                      {activeOther(recipe.id) ? 'Switch to' : 'Start'}
                    </button>
                  </>
                )}
                {model?.status === 'recovering' && <button className="ghost" disabled onClick={act(() => {})}>Recovering</button>}
              </>
            )}
          </div>
          <span className={`m-caret ${open ? 'open' : ''}`} aria-hidden="true">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9 6l6 6-6 6" /></svg>
          </span>
        </div>
        {open && (
          <div className="mdetail">
            {revoked(revocation) && (() => {
              const body = revokeBody(revocation, isActive)
              return (
                <div className="revoke-line">
                  <strong>{REVOKE_TITLE}</strong>
                  {body && <span>{body}</span>}
                </div>
              )
            })()}
            <div className="board">
              <div className="cell">
                <div className="l">Speed</div>
                <div className="v">
                  {isMedia ? 'n/a' : measured ? measured.toFixed(1) : reference ? `~${reference}` : 'n/a'}{' '}
                  {!isMedia && <small>tok/s</small>}
                </div>
                <div className={`q ${measured && !isMedia ? 'ok' : ''}`}>
                  {isMedia ? 'not token based' : measured ? 'measured on this Spark' : 'typical on a Spark'}
                </div>
              </div>
              <div className="cell">
                <div className="l">First token</div>
                <div className="v">{model?.time_to_first_token_ms ? model.time_to_first_token_ms : 'n/a'} <small>ms</small></div>
              </div>
              <div className="cell">
                <div className="l">Tokens served</div>
                <div className="v" title={served ? servedTotal.toLocaleString() : undefined}>
                  {served ? formatTokens(servedTotal) : 'n/a'}
                </div>
                <div className="q">{served ? 'since basement started counting' : 'no usage counted yet'}</div>
              </div>
              <div className="cell">
                <div className="l">Download</div>
                <div className="v">{formatBytes(recipe.artifact_bytes)}</div>
              </div>
              <div className="cell">
                <div className="l">Space needed</div>
                <div className="v">{formatBytes(recipe.required_bytes)}</div>
              </div>
            </div>
            <dl className="facts">
              <dt>Model by</dt><dd>{recipe.model_by || recipe.publisher}</dd>
              <dt>Released</dt><dd>{recipe.model_released || 'n/a'}</dd>
              <dt>Quantization</dt>
              <dd>{recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant ?? 'Original weights' : 'n/a'}</dd>
              <dt>Recipe by</dt><dd>{recipe.recipe_by || 'n/a'}</dd>
              <dt>Recipe version</dt><dd>v{recipe.version}</dd>
              {updateAvailable && model && (
                <>
                  <dt>Update</dt>
                  <dd className="update-line">v{model.recipe_version} installed, v{recipe.version} available</dd>
                </>
              )}
              {served && (
                <>
                  <dt>Prompt tokens</dt>
                  <dd title={served.prompt_tokens.toLocaleString()}>{formatTokens(served.prompt_tokens)}</dd>
                  <dt>Generated tokens</dt>
                  <dd title={served.generation_tokens.toLocaleString()}>{formatTokens(served.generation_tokens)}</dd>
                </>
              )}
              <dt>Model ID</dt><dd><code>{recipe.service.served_model_id}</code></dd>
              <dt>Runtime</dt><dd><code>{runtimeLabel(recipe.runtime.kind)} · pinned digest</code></dd>
              <dt>Source</dt><dd><a href={recipe.source.url} target="_blank" rel="noreferrer">{recipe.source.url} ↗</a></dd>
              {recipe.artifacts.map(artifact => (
                <Fragment key={artifact.role}>
                  <dt>{artifact.role === 'primary' ? 'Weights' : artifact.role === 'drafter' ? 'Draft weights' : artifact.role}</dt>
                  <dd>
                    <code>{artifact.repository}@{artifact.revision.slice(0, 12)}</code>{' '}
                    <a href={artifact.licence_url} target="_blank" rel="noreferrer">{artifact.licence} licence ↗</a>
                  </dd>
                </Fragment>
              ))}
            </dl>
            {/* The same tools, on the machine that holds the model. Another
                Spark in the fleet carries them there, through the placement
                its first action records. */}
            {(model || host) && (() => {
              // A tool on this Spark's own row answers to this Spark alone.
              // What another Spark is doing with the same recipe has never
              // stopped a local button and must not start now.
              const toolsLocked = host ? hostLocked : busy
              return (
                <div className="row-tools">
                  {(host ? hostServing : isActive) && (
                    <>
                      {!isMedia && (
                        <button className="ghost" disabled={toolsLocked} onClick={() => simpleAction(recipe, host, 'benchmark')}>Measure speed</button>
                      )}
                      <button className="ghost" disabled={toolsLocked} onClick={() => simpleAction(recipe, host, 'smoke-test')}>Check health</button>
                    </>
                  )}
                  {/* The fleet holds a record it can no longer read, so every
                      button above is dead and stays dead. This is the one way
                      out: it ends the record, and the model goes with it. */}
                  {host && placement && placement.stale === true && (
                    <button
                      className="danger"
                      disabled={pending.has(recipe.id)}
                      onClick={() => clearRecord(recipe, host, placement)}
                    >
                      {CLEAR_RECORD}
                    </button>
                  )}
                  <button className="danger" disabled={toolsLocked} onClick={() => remove(recipe, host)}>Uninstall</button>
                </div>
              )
            })()}
          </div>
        )}
      </Fragment>
    )
  }

  return (
    <div className="stack">
      {blockers.length > 0 && (
        <div className="alert" role="alert">
          <strong>Setup needed before models can run</strong>
          <ul>{blockers.map(item => <li key={item}>{item}</li>)}</ul>
        </div>
      )}

      {featured && (
        <section className="hero" aria-label="Recommended model">
          <div className="hero-top">
            <Mark recipe={featured} recipeIDs={[featured.id]} size={68} />
            <div className="hero-name">
              <p className="kicker">Recommended for your Spark</p>
              <h2>{featured.display_name}</h2>
              <p className="pub">{featured.model_by || featured.publisher}</p>
            </div>
            <div className="hero-get">
              {(() => {
                // Same lock the table rows use, read the same way: work on
                // this Spark, and work the fleet is doing on another Spark.
                // The hero is only shown before anything is installed here,
                // so a placement is the whole of what it can learn about an
                // install already running for this model.
                const heroBusy = recipeBusy(
                  pending.has(featured.id) ||
                    jobs.some(job => job.recipe_id === featured.id && !terminal(job.state) &&
                      DISRUPTIVE_KINDS.has(job.kind)),
                  installed.has(featured.id),
                  workingPlacement(placements, startedOnPlacement, featured.id, DISRUPTIVE_KINDS),
                )
                const heroFits = fitsOn(featured)
                if (!heroFits && pairable(featured)) {
                  return <button className="brand" onClick={openFleet}>Pair a second Spark</button>
                }
                return (
                  <button
                    className="brand"
                    disabled={heroBusy || !heroFits}
                    onClick={() => startInstall(featured)}
                  >
                    {!heroFits ? 'Needs a Spark' : heroBusy ? 'Working' : installVerb(featured)}
                  </button>
                )
              })()}
              <small>{formatBytes(featured.artifact_bytes)} download</small>
            </div>
          </div>
          <p className="hero-line">
            {USE[featured.id]}{' '}
            <span>{trustLine(featured)} Basement measures its real speed after install.</span>
          </p>
          <div className="hero-score">
            <div className="cell"><div className="l">Speed</div><div className="v">~{REFERENCE_TPS[featured.id]}</div><div className="u">tok/s · typical</div></div>
            <div className="cell"><div className="l">Download</div><div className="v">{formatBytes(featured.artifact_bytes)}</div><div className="u">one time</div></div>
            <div className="cell"><div className="l">Licence</div><div className="v">{featured.artifacts[0]?.licence ?? 'n/a'}</div><div className="u">open weights</div></div>
            <div className="cell"><div className="l">Runtime</div><div className="v">{runtimeLabel(featured.runtime.kind)}</div><div className="u">pinned digest</div></div>
          </div>
        </section>
      )}

      {showFleet && (
        <FleetStrip
          sparks={sparks}
          power={powerRows}
          open={openSpark}
          onOpen={nodeID => setOpenSpark(openSpark === nodeID ? '' : nodeID)}
        />
      )}
      {showFleet && openRow && <NodeCard spark={openRow} />}
      {showMemberBanner && (
        <MemberBanner controllerConsoleURL={fleetRoleSummary?.controller_console_url ?? ''} />
      )}

      {/* The owner's split. Two tabs, not two groups on one page: with many
          models, the models you have and the models you can get are two
          different questions. The pill is the Redactor's own switch. */}
      {showTabs && (
        <div className="tabs-row">
          <div className="jobs" role="tablist" aria-label="Which models">
            <button
              role="tab"
              id="tab-held"
              aria-controls="pane-held"
              aria-current={tab === 'mine'}
              onClick={() => setTab('mine')}
            >
              {heldTabLabel(sparks.length)}<span className="n">{split.held.length}</span>
            </button>
            <button
              role="tab"
              id="tab-catalog"
              aria-controls="pane-catalog"
              aria-current={tab === 'catalog'}
              onClick={() => setTab('catalog')}
            >
              {CATALOG_TAB}<span className="n">{split.catalogCount}</span>
            </button>
          </div>
        </div>
      )}

      <div className="mtable">
        <div className="mthead" aria-hidden="true">
          <span>Model</span><span className="r">Speed</span><span className="r">Disk</span><span style={{ paddingLeft: 20 }}>Status</span><span />
        </div>
        {showTabs ? (
          <>
            <div className="mpane" id="pane-held" role="tabpanel" aria-labelledby="tab-held" hidden={tab !== 'mine'}>
              {split.held.map(rowFor)}
            </div>
            <div
              className="mpane"
              id="pane-catalog"
              role="tabpanel"
              aria-labelledby="tab-catalog"
              hidden={tab !== 'catalog'}
            >
              {split.catalogCount === 0 ? (
                <div className="empty">{CATALOG_EMPTY}</div>
              ) : (
                <>
                  {/* One divider for each lab, in the order the catalog sorts
                      them. The lab is what the owner reads a growing list by,
                      and the newest model of a lab sits at the top of its
                      group. */}
                  {split.labs.map(group => (
                    <Fragment key={group.label}>
                      <div className="mgroup">{group.label}</div>
                      {group.models.map(rowFor)}
                    </Fragment>
                  ))}
                </>
              )}
            </div>
          </>
        ) : (
          rows.map(rowFor)
        )}
      </div>
      {/* Where the recipes in that table came from, and how fresh they are.
          A feed that has never been fetched says nothing at all, and the
          button beside it asks the feed now rather than at the next check. */}
      {system?.recipe_feed && (() => {
        const note = feedNote(system.recipe_feed, Date.now())
        return (
          <div className="feed-line">
            {note && <p className={`table-note ${note.warn ? 'warn' : ''}`}>{note.text}</p>}
            <button type="button" className="ghost" disabled={checkingFeed} onClick={checkForRecipes}>
              {checkingFeed ? 'Checking' : 'Check for new recipes'}
            </button>
            {feedError && <p className="table-note warn">{feedError}</p>}
          </div>
        )
      })()}
      {/* Only shown once something has actually been counted. Basement
          counts a model's tokens while it serves it here, so an empty total
          means no serving has been sampled yet, not that nothing ran. */}
      {tokens && tokens.totals.prompt_tokens + tokens.totals.generation_tokens > 0 && (
        <div className="token-total">
          <span
            className="n"
            title={(tokens.totals.prompt_tokens + tokens.totals.generation_tokens).toLocaleString()}
          >
            {formatTokens(tokens.totals.prompt_tokens + tokens.totals.generation_tokens)}
          </span>
          <span>
            tokens served on this Spark since basement started counting
            <small title={tokens.totals.prompt_tokens.toLocaleString()}>
              {formatTokens(tokens.totals.prompt_tokens)} prompt
            </small>
            <small title={tokens.totals.generation_tokens.toLocaleString()}>
              {formatTokens(tokens.totals.generation_tokens)} generated
            </small>
          </span>
        </div>
      )}
      <p className="table-note">
        “Typical” speeds are community-reported. Basement measures the real number after install.
      </p>

      <dialog ref={dialogRef} onClose={() => setConfirm(null)} aria-label="Confirm installation">
        {confirm && (() => {
          const recipe = confirm.recipe
          const onPeer = placement === 'peer'
          // The fleet-wide list replaces the two-option picker whenever the
          // controller planned this install. Both cannot be on screen at
          // once: one names Sparks the fleet holds, the other names a Spark
          // added by address only. A model that needs more than one Spark is
          // never planned, and the gate is repeated here so the list cannot
          // appear on such a model even for one render.
          const fleetChoice = placementPlan !== null && recipe.topology.spark_count === 1
          const options = fleetChoice
            ? placementOptions(placementList, placementPlan.recommended_node_id)
            : []
          // The Spark this install runs on, while that is not this one. Every
          // number below then belongs to that machine, and it is the same row
          // the submit sends to.
          const remote = fleetChoice && placementTarget && !placementTarget.isSelf
            ? placementTarget
            : undefined
          // The picked row can only be sent if it names a Spark that can
          // really take the model.
          const placementReady = !fleetChoice ||
            installRoute(placementChoice, placementList, placementPlan.recommended_node_id).where !== 'none'
          // Every number in here belongs to the machine that will actually
          // run the install, which is why the other Spark answers its own
          // preflight before anything below is shown for it.
          const shown = onPeer ? peerPreflight : confirm.preflight
          const target = onPeer
            ? peerSummary?.system
            : remote
              ? {
                storage_available_bytes: remote.storageAvailableBytes ?? 0,
                memory_available_bytes: remote.memoryAvailableBytes ?? 0,
              }
              : system
          const machine = onPeer && peer ? peer.name : remote ? remote.name : 'This Spark'
          const verb = remote ? placementVerb(remote, recipe.version) : verbFor(recipe, placement)
          const switchFrom = remote ? placementSwitchFrom(remote) : switchFromFor(placement)
          const licences = licenceArtifacts(recipe)
          const territoryLabel = territoryEligibilityLabel(recipe)
          const confirmationsComplete = installConfirmationsComplete(recipe, licence, territoryEligibility)
          // An update has a version already installed on the target machine
          // to name. Only this Spark's disk is readable from this console, so
          // an update on any other machine states the versions and leaves
          // that Spark's files to that Spark.
          const installedVersion = onPeer
            ? peerInstalled.get(recipe.id)?.recipe_version
            : remote
              ? remote.installedVersion
              : installed.get(recipe.id)?.recipe_version
          const isUpdate = verb === 'Update' && installedVersion !== undefined
          const plan = isUpdate && !onPeer && !remote ? updatePlan(installedVersion, recipe, storage) : null
          // "container" only when the plan names no kind at all; with a kind,
          // runtimeLabel spells it the way its own project does and falls
          // back to the recipe's own word rather than renaming it.
          const runtimeWord = plan?.runtimeKind ? runtimeLabel(plan.runtimeKind) : 'container'
          // Nothing is fetched only when this Spark already has both the
          // weights and the runtime image this version pins. Anything less
          // than both, and the install does download something.
          const nothingToFetch = plan?.weightsPresent === true && plan?.imagePresent === true
          const weightsFormat =
            recipe.service.sglang?.quantization ??
            (recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant : undefined)
          const reclaim = shown ? reclaimFrom(shown) : undefined
          const close = () => dialogRef.current?.close()
          const foot = (confirmable: boolean) => (
            <div className="dialog-foot">
              <button type="button" className="ghost" onClick={close}>Cancel</button>
              <button type="button" className="primary" disabled={!confirmable} onClick={confirmInstall}>{verb}</button>
            </div>
          )
          return (
            <form method="dialog" className="dialog-pad" onSubmit={event => event.preventDefault()}>
              <div className="dialog-head">
                <div>
                  <p className="kicker">{verb === 'Install' ? 'Install model' : verb === 'Update' ? 'Update model' : verb}</p>
                  <h2>{recipe.display_name}</h2>
                </div>
                <button type="button" className="dialog-close" onClick={close} aria-label="Close">×</button>
              </div>
              {/* A model that needs two Sparks always runs across both, so
                  there is nothing to choose; a one-Spark model can run on any
                  Spark in the fleet. The controller's plan names them, with
                  what each machine has free, and states the reason beside a
                  Spark it refuses rather than dropping that Spark from the
                  list. */}
              {fleetChoice ? (
                <div className="install-choice" role="radiogroup" aria-label="Run on">
                  <p className="kicker">Run on</p>
                  {options.map(option => (
                    <label className="confirm-check" key={option.key}>
                      <input
                        type="radio"
                        name="install-placement"
                        disabled={!option.eligible}
                        checked={placementChoice === option.key}
                        onChange={() => setPlacementChoice(option.key)}
                      />
                      <span>
                        {option.name}
                        <small>{option.note}</small>
                      </span>
                      <span className="row-check" />
                    </label>
                  ))}
                  {!placementReady && (
                    <p className="muted" style={{ fontSize: 12.5 }}>{NO_PLACEMENT_LEFT}</p>
                  )}
                </div>
              ) : peer && recipe.topology.spark_count === 1 ? (
                <div className="install-choice" role="radiogroup" aria-label="Run on">
                  <p className="kicker">Run on</p>
                  <label className="confirm-check">
                    <input
                      type="radio"
                      name="install-placement"
                      checked={!onPeer}
                      onChange={() => choosePlacement('local')}
                    />
                    <span>
                      This Spark
                      <small>{system?.hostname ?? 'the machine this console runs on'}</small>
                    </span>
                    <span className="row-check" />
                  </label>
                  <label className="confirm-check">
                    <input
                      type="radio"
                      name="install-placement"
                      checked={onPeer}
                      onChange={() => choosePlacement('peer')}
                    />
                    <span>
                      {peer.name}
                      <small>{peerSummary?.system?.hostname ?? peer.base_url}</small>
                    </span>
                    <span className="row-check" />
                  </label>
                </div>
              ) : null}
              {onPeer && peerChecking ? (
                <>
                  <p className="muted" style={{ fontSize: 12.5 }}>Checking {machine} for space, memory and licence…</p>
                  {foot(false)}
                </>
              ) : onPeer && (peerError || !shown) ? (
                <>
                  <p className="error-text" role="alert">{peerError || `${machine} did not answer with a preflight.`}</p>
                  <p className="muted" style={{ fontSize: 12.5 }}>Pick this Spark to install here instead.</p>
                  {foot(false)}
                </>
              ) : remote || (shown && shown.ready) ? (
                // Every check in a preflight reads one machine's own disk,
                // memory and licences. This Spark's preflight says nothing
                // about an install that will not run here, so it does not
                // stand in the way of one. The Spark that will run it answers
                // for itself: the plan already refused the Sparks that cannot
                // take the model, and the controller runs that machine's own
                // preflight before it places anything, in its own words.
                <>
                  {isUpdate ? (
                    // Every line here is either a version number, something
                    // this Spark reported about its own disk, or a setting
                    // the new recipe pins. Nothing is a changelog, because
                    // recipes carry no notes field and the recipe of the
                    // installed version is not kept (see updatePlan).
                    <div className="update-plan">
                      <dl className="facts">
                        <dt>Version</dt>
                        <dd>v{installedVersion} → v{recipe.version}</dd>
                        {plan ? (
                          <>
                            <dt>Weights</dt>
                            <dd>
                              {plan.weightsPresent === null
                                ? 'Reading what is already on this Spark.'
                                : plan.weightsPresent
                                  ? 'Same weights already on this Spark are reused. No large download.'
                                  : `New weights needed. ${formatBytes(plan.bytesToFetch)} downloads.`}
                            </dd>
                            <dt>Runtime</dt>
                            <dd>
                              {plan.imagePresent === null
                                ? `Pinned ${runtimeWord} image, by digest.`
                                : plan.imagePresent
                                  ? `${runtimeWord} image already on this Spark.`
                                  : `${runtimeWord} image not on this Spark yet; it is pulled.`}
                            </dd>
                          </>
                        ) : (
                          <>
                            <dt>Files</dt>
                            <dd>{machine} works out what it already has.</dd>
                          </>
                        )}
                        {plan?.contextLength ? (
                          <>
                            <dt>Context length</dt>
                            <dd>{plan.contextLength.toLocaleString()} tokens</dd>
                          </>
                        ) : null}
                        <dt>Runs on</dt>
                        <dd>{recipe.topology.spark_count === 1 ? 'One Spark' : `${recipe.topology.spark_count} Sparks`}</dd>
                        {weightsFormat ? (
                          <>
                            <dt>Weights format</dt>
                            <dd>{weightsFormat}</dd>
                          </>
                        ) : null}
                      </dl>
                    </div>
                  ) : (
                    <dl className="model-facts">
                      <div>
                        <dt>Download</dt>
                        <dd>
                          {verb === 'Resume install'
                            ? `${formatBytes(Math.max(recipe.artifact_bytes - downloadedBytes(recipe), 0))} to go`
                            : formatBytes(recipe.artifact_bytes)}
                        </dd>
                      </div>
                      <div><dt>Space needed</dt><dd>{formatBytes(recipe.required_bytes)}</dd></div>
                      <div><dt>Typical speed</dt><dd>{REFERENCE_TPS[recipe.id] ? `~${REFERENCE_TPS[recipe.id]} tok/s` : 'n/a'}</dd></div>
                    </dl>
                  )}
                  {/* Read from whichever machine will run this, and only when
                      that machine actually reported both numbers. */}
                  {target && target.storage_available_bytes > 0 && target.memory_available_bytes > 0 && (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      {machine} has {formatBytes(target.storage_available_bytes)} free on disk and{' '}
                      {formatBytes(target.memory_available_bytes)} memory free right now.
                    </p>
                  )}
                  {onPeer ? (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      Progress shows on {machine}'s own console. First start can take up to {startTimeoutMinutes(recipe)} minutes.
                    </p>
                  ) : nothingToFetch ? (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      Nothing to download; starting the model can take up to {startTimeoutMinutes(recipe)} minutes.
                    </p>
                  ) : (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      After downloading, the first start can take up to {startTimeoutMinutes(recipe)} minutes. Cancelling is safe; downloads resume later.
                    </p>
                  )}
                  {/* Both downloads run on this Spark and share its
                      bandwidth. An install on another Spark shares nothing
                      with them. */}
                  {!onPeer && !remote && anotherInstallRunning && (
                    <p className="muted" style={{ fontSize: 12.5 }}>Another download is running. Both continue, sharing bandwidth.</p>
                  )}
                  {switchFrom && (
                    <div className="install-choice" role="radiogroup" aria-label="After the download finishes">
                      {(fleetChoice || (peer && recipe.topology.spark_count === 1)) &&
                        <p className="kicker">After the download finishes</p>}
                      <label className="confirm-check">
                        <input
                          type="radio"
                          name="install-activate"
                          checked={activate}
                          onChange={() => setActivate(true)}
                        />
                        <span>
                          {switchFrom === recipe.id ? 'Update and switch now' : 'Download and switch now'}
                          <small>
                            {switchFrom === recipe.id
                              ? `Restarts ${recipe.display_name} on the new version. Basement restores it if this fails.`
                              : `This stops ${nameOf(switchFrom)} while ${recipe.display_name} starts.`}
                          </small>
                        </span>
                        <span className="row-check" />
                      </label>
                      <label className="confirm-check">
                        <input
                          type="radio"
                          name="install-activate"
                          checked={!activate}
                          onChange={() => setActivate(false)}
                        />
                        <span>
                          {switchFrom === recipe.id ? 'Update only' : 'Download only'}
                          <small>
                            {switchFrom === recipe.id
                              ? `${recipe.display_name} keeps serving. Switch later from the Models tab.`
                              : `${nameOf(switchFrom)} keeps serving. Start ${recipe.display_name} later from the Models tab.`}
                          </small>
                        </span>
                        <span className="row-check" />
                      </label>
                    </div>
                  )}
                  {onPeer && (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      {switchFrom
                        ? `Switching happens on ${machine}, not here.`
                        : `${machine} downloads and serves this. This Spark is unaffected.`}
                    </p>
                  )}
                  {licences.length > 0 && (
                    <div className="licence-details">
                      {licences.map(artifact => (
                        <section className="licence-detail" key={`${artifact.role}:${artifact.repository}`}>
                          {artifact.licence && <p className="licence-name">{artifact.licence}</p>}
                          {(artifact.licence_territory_exclusions?.length ?? 0) > 0 && (
                            <>
                              <p className="licence-copy">This licence does not grant rights in these territories:</p>
                              <ul className="territory-list">
                                {artifact.licence_territory_exclusions?.map(territory => (
                                  <li key={territory}>{territory}</li>
                                ))}
                              </ul>
                            </>
                          )}
                          {artifact.licence_url && (
                            <div className="licence-links">
                              <a href={artifact.licence_url} target="_blank" rel="noreferrer">Read the licence ↗</a>
                            </div>
                          )}
                        </section>
                      ))}
                    </div>
                  )}
                  <div className="licence-consents">
                    {licences.length > 0 && (
                      <label className="confirm-check">
                        <input type="checkbox" checked={licence} onChange={event => setLicence(event.target.checked)} />
                        <span>I accept the model licence</span>
                      </label>
                    )}
                    {territoryLabel && (
                      <label className="confirm-check">
                        <input
                          type="checkbox"
                          checked={territoryEligibility}
                          onChange={event => setTerritoryEligibility(event.target.checked)}
                        />
                        <span>{territoryLabel}</span>
                      </label>
                    )}
                  </div>
                  {/* run() already refuses a second call for a recipe it is
                      still working on. Naming it here makes the guard one a
                      person can see rather than one they only feel. */}
                  {foot(confirmationsComplete && placementReady && !pending.has(recipe.id))}
                </>
              ) : shown ? (
                <>
                  <p className="error-text" role="alert">{machine} is not ready for {recipe.display_name} yet:</p>
                  <ul>{preflightBlockers(shown).map(item => <li key={item}>{item}</li>)}</ul>
                  {Array.isArray(reclaim) && reclaim.length > 0 && (
                    <div className="alert">
                      <strong>Free up space</strong>
                      <ul>
                        {reclaim.map(candidate => (
                          <li key={candidate.recipe_id}>
                            Removing {candidate.display_name} reclaims {formatBytes(candidate.bytes)}
                            {candidate.active ? ' (currently active)' : ''}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  <div className="dialog-foot">
                    {onPeer && <span className="note">Pick this Spark to install here instead.</span>}
                    <button type="button" className="ghost" onClick={close}>Close</button>
                  </div>
                </>
              ) : null}
            </form>
          )
        })()}
      </dialog>
    </div>
  )
}
