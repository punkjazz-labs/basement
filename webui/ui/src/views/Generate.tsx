import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  api, apiBlob, apiUpload, copyText, idempotency,
  type GenerateResponse, type Generation, type MediaGenerationConfig, type Recipe, type StagedMedia,
} from '../api'
import {
  canvasShapes, canvasSizes, defaultCanvasTier, durationArithmetic, durationOptions, finishedTransitions,
  fitArithmetic, fitCanvasToImage, formatElapsed, generationActive, generationElapsedSeconds,
  generationRequest, generationState, generationStatusMap, generationTerminal, IMAGE_ACCEPT, markUnseen,
  MODE_IMAGE_TO_VIDEO, MODE_TEXT_TO_VIDEO, modeOptions, pickImage, playableVideoBlob, reuseValues,
  sizeWaitHint, sizeWaitLabel, sortGenerationsNewestFirst, stagedFrame, stagedFrameCaption,
  type CanvasShape, type GenerationStatusMap, type StagedFrame,
} from '../generation'
import { cachedPoster, capturePoster, captureFrameBlob, forgetPoster, runFrameBlob, storePoster } from '../posters'
import { readableWeights } from '../catalog'
import { confirmBox, noticeBox } from '../confirm'

interface GenerateProps {
  recipe: Recipe
  recipes: Recipe[]
}

const SOUND_KEY = 'basement.generate.sound'
const FLASH_TITLE = '● Done · basement'

// Captured once, at import time, so a run of title flashes always has the
// real title to return to even after several rounds of alternating it.
const ORIGINAL_TITLE = typeof document === 'undefined' ? '' : document.title

const modelGlyph = (name: string): string =>
  name.split(/\s+/).filter(Boolean).slice(0, 2).map(word => word[0]).join('').toUpperCase()

// The Generate button shows whichever chord this platform actually accepts.
// Both the Mac and the non-Mac key combination are still handled below
// regardless of what this reports, so a stale or unrecognized platform
// string only ever costs a mislabeled glyph, never a broken shortcut.
const shortcutGlyph = (): string => {
  const platform = typeof navigator === 'undefined' ? '' : navigator.platform
  return /Mac|iPhone|iPad|iPod/.test(platform) ? '⌘⏎' : 'Ctrl+⏎'
}

const generationFilePath = (generation: Generation): string =>
  generation.file_url ?? `/api/v1/generations/${encodeURIComponent(generation.id)}/file`

// The generations list spans every model this Spark has ever run, not just
// the recipe open right now, so a run's model name comes from its own
// model_id rather than from the page's current context.
function generationModelName(recipes: Recipe[], generation: Generation): string {
  return recipes.find(item => item.id === generation.model_id)?.display_name ?? generation.model_id
}

// The same arithmetic the old result card used: a generation's frame count
// only still means a round number of seconds if it still lands on the
// recipe's current frame grid. A recipe update can move that grid, so an
// older generation just reports its frame count instead of a stale duration.
function durationString(generation: Generation, config: MediaGenerationConfig): string {
  const matchesCurrentFrameGrid = generation.frames === config.frame_block * generation.blocks + config.frame_offset
  const seconds = matchesCurrentFrameGrid && config.frames_per_second > 0
    ? generation.frames / config.frames_per_second
    : null
  return seconds === null
    ? `${generation.frames} frames`
    : `${Math.round(seconds * 10) / 10}s · ${generation.frames} frames`
}

// Two short tones built from oscillators rather than shipped as an audio
// file, so the console has no binary asset to keep in sync with this code.
// offset staggers one run's chime after another's, so two runs finishing in
// the same tick play in sequence instead of both tones stacking into double
// the gain.
function playChime(context: AudioContext, offset = 0): void {
  const tone = (frequency: number, start: number, duration: number) => {
    const oscillator = context.createOscillator()
    const gain = context.createGain()
    gain.gain.value = 0.08
    oscillator.frequency.value = frequency
    oscillator.connect(gain)
    gain.connect(context.destination)
    oscillator.start(context.currentTime + offset + start)
    oscillator.stop(context.currentTime + offset + start + duration)
  }
  tone(880, 0, 0.09)
  tone(1175, 0.09, 0.09)
}

function RunningElapsed({ generation }: { generation: Generation }) {
  const [, setTick] = useState(0)
  useEffect(() => {
    const timer = setInterval(() => setTick(value => value + 1), 1000)
    return () => clearInterval(timer)
  }, [])
  const elapsed = generationElapsedSeconds(generation, Date.now())
  return <span>Elapsed {elapsed === null ? '0:00' : formatElapsed(elapsed)}</span>
}

export function GenerationProgress({ generation }: { generation: Generation }) {
  const value = generation.progress_value
  const max = generation.progress_max
  // The runtime reports which graph node is working, as its id. That is an
  // internal number with no meaning to anyone reading this screen, so it is
  // recorded but not shown; the bar is what says how far along this is.
  const phase = 'Generating'
  const determinate = typeof value === 'number' && typeof max === 'number'
    && Number.isFinite(value) && Number.isFinite(max) && value >= 0 && max > 0 && value <= max
  if (!determinate) {
    return (
      <div className="gen-working" role="status">
        <span className="sdot busy" aria-hidden="true" />
        <span>{phase}</span>
        <RunningElapsed generation={generation} />
      </div>
    )
  }
  const percent = value / max * 100
  return (
    <div className="gen-progress" role="status">
      <div className="gen-progress-head">
        <span>{phase}</span>
        <RunningElapsed generation={generation} />
      </div>
      <div
        className="gen-progress-track"
        role="progressbar"
        aria-label="Generation progress"
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={value}
      >
        <span style={{ width: `${percent}%` }} />
      </div>
      <div className="gen-progress-count">
        <span>{value} of {max}</span>
        <span>{Math.round(percent)}%</span>
      </div>
    </div>
  )
}

// The file endpoint deliberately says attachment. Fetching it through the
// authenticated helper and giving the resulting Blob its own URL is what
// makes local playback possible without exposing the runtime or a host path.
function GenerationVideo({ generation, videoRef }: {
  generation: Generation
  // Held by the view so a frame can be taken from the run being watched, at
  // the point it is being watched at.
  videoRef?: React.RefObject<HTMLVideoElement | null>
}) {
  const [url, setURL] = useState('')
  const [error, setError] = useState('')
  const [retry, setRetry] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    let objectURL = ''
    let mounted = true
    setURL('')
    setError('')
    apiBlob(generationFilePath(generation), {
      signal: controller.signal,
    }).then(blob => {
      objectURL = URL.createObjectURL(playableVideoBlob(blob))
      if (mounted) setURL(objectURL)
      else URL.revokeObjectURL(objectURL)
    }).catch(problem => {
      if (mounted && (problem as { name?: string })?.name !== 'AbortError') {
        setError(problem instanceof Error ? problem.message : 'Could not load this video')
      }
    })
    // A refresh that removes or changes this generation unmounts this view;
    // leaving Generate does the same. Both paths release the Blob URL.
    return () => {
      mounted = false
      controller.abort()
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
  }, [generation.id, generation.file_url, generation.finished_at, generation.bytes, retry])

  if (error) {
    return (
      <div className="gen-video-state error-text" role="alert">
        <span>{error}</span>
        <button className="quiet" onClick={() => setRetry(value => value + 1)}>Try again</button>
      </div>
    )
  }
  if (!url) {
    return (
      <div className="gen-video-state" role="status">
        <span className="sdot busy" aria-hidden="true" />
        Loading video
      </div>
    )
  }
  return (
    <div className="gen-video-wrap">
      <video
        ref={videoRef}
        controls
        preload="metadata"
        src={url}
        style={{ aspectRatio: `${generation.width} / ${generation.height}` }}
      >
        This browser cannot play the generated video.
      </video>
    </div>
  )
}

function Thumb({ generation, modelName, poster, selected, onSelect }: {
  generation: Generation
  modelName: string
  poster: string | null
  selected: boolean
  onSelect: () => void
}) {
  const value = generation.progress_value
  const max = generation.progress_max
  const determinate = generation.status === 'running'
    && typeof value === 'number' && typeof max === 'number' && max > 0 && value >= 0 && value <= max
  const finishedElapsed = generation.status === 'completed'
    ? generationElapsedSeconds(generation, Date.now())
    : null

  return (
    <button
      type="button"
      className={`thumb${selected ? ' sel' : ''}`}
      title={`${modelName}: ${generation.prompt}`}
      onClick={onSelect}
    >
      {poster
        ? <img className="pic" src={poster} alt="" />
        : (
          <div className="thumb-blank">
            <span className={`gen-state ${generation.status}`}>{generationState(generation.status)}</span>
          </div>
        )}
      {generation.status === 'running' && (
        <div className="prog">
          {determinate
            ? <span style={{ width: `${(value / max) * 100}%` }} />
            : <span className="indeterminate" />}
        </div>
      )}
      <div className="cap">
        <span className={`gen-state ${generation.status}`}>{generationState(generation.status)}</span>
        <b>{generation.prompt}</b>
        {finishedElapsed !== null && <span>{formatElapsed(finishedElapsed)}</span>}
      </div>
    </button>
  )
}

// A staged frame plus the local preview of the exact bytes that were staged.
// The preview is a Blob URL rather than a read back from the Spark: the
// staging endpoint stores an image, it does not serve one.
interface ComposerFrame extends StagedFrame {
  preview: string
}

export default function Generate({ recipe, recipes }: GenerateProps) {
  const config = recipe.media_generation
  const [shape, setShape] = useState<CanvasShape>('horizontal')
  const sizes = useMemo(() => config ? canvasSizes(config, shape) : [], [config, shape])
  const durations = useMemo(() => config ? durationOptions(config) : [], [config])
  const modes = useMemo(() => config ? modeOptions(config) : [], [config])
  const [mode, setMode] = useState<string>(MODE_TEXT_TO_VIDEO)
  const [frame, setFrame] = useState<ComposerFrame | null>(null)
  const [frameError, setFrameError] = useState('')
  const [staging, setStaging] = useState(false)
  const [copiedSeed, setCopiedSeed] = useState(false)
  const [useMenu, setUseMenu] = useState<{ id: string; top: number; right: number } | null>(null)
  const [prompt, setPrompt] = useState('')
  const [shortEdge, setShortEdge] = useState(0)
  const [blocks, setBlocks] = useState(0)
  const [seed, setSeed] = useState('')
  const [filledNote, setFilledNote] = useState(false)
  const [generations, setGenerations] = useState<Generation[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [formError, setFormError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [busyID, setBusyID] = useState('')
  const [stagedID, setStagedID] = useState('')
  const [streamAvailable, setStreamAvailable] = useState(true)
  const [soundOn, setSoundOn] = useState(() => window.localStorage.getItem(SOUND_KEY) === 'on')
  const [unseen, setUnseen] = useState<ReadonlySet<string>>(new Set())
  // Has no meaning of its own; incrementing it just forces the poster effect
  // below to re-run and look for the next uncaptured poster once the
  // current capture finishes, without generations itself having changed.
  const [posterTick, setPosterTick] = useState(0)
  const formRef = useRef<HTMLFormElement>(null)
  const frameInputRef = useRef<HTMLInputElement>(null)
  const stageVideoRef = useRef<HTMLVideoElement>(null)
  const useMenuRef = useRef<HTMLDivElement>(null)
  // The Blob URL the slot is showing, kept outside state so replacing or
  // clearing the frame can release the previous one without a render.
  const framePreviewRef = useRef('')
  const audioCtxRef = useRef<AudioContext | null>(null)
  const statusRef = useRef<GenerationStatusMap>({})
  const inFlightPosterRef = useRef<{ id: string; controller: AbortController } | null>(null)
  const posterFailedRef = useRef<Set<string>>(new Set())

  // The tier survives a change of shape, because turning a clip on its side is
  // not a decision about how big it is.
  useEffect(() => {
    if (!config) return
    const preferred = defaultCanvasTier(config)
    setShortEdge(current => sizes.some(option => option.shortEdge === current) ? current : preferred)
  }, [sizes, config])

  useEffect(() => {
    const preferred = durations.find(option => option.blocks === config?.default_blocks) ?? durations[0]
    setBlocks(current => durations.some(option => option.blocks === current) ? current : preferred?.blocks ?? 0)
  }, [durations, config?.default_blocks])

  // A recipe update can retire the mode this screen is standing in, and the
  // first offered mode is text to video wherever a recipe offers it.
  useEffect(() => {
    if (modes.length === 0) return
    setMode(current => modes.some(option => option.mode === current) ? current : modes[0].mode)
  }, [modes])

  // The last preview outlives every render, so only a real unmount releases
  // it. Every other release happens where the frame itself is replaced.
  useEffect(() => () => {
    if (framePreviewRef.current) URL.revokeObjectURL(framePreviewRef.current)
  }, [])

  // The Use menu closes on anything that is not a click inside it. It is
  // positioned against the viewport, so a scroll or a resize would leave it
  // pointing at a card that has moved; both close it instead.
  useEffect(() => {
    if (!useMenu) return
    const close = () => setUseMenu(null)
    const outside = (event: Event) => {
      if (!(event.target instanceof Node) || !useMenuRef.current?.contains(event.target)) close()
    }
    const escape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') close()
    }
    document.addEventListener('pointerdown', outside)
    document.addEventListener('keydown', escape)
    window.addEventListener('resize', close)
    window.addEventListener('scroll', close, true)
    return () => {
      document.removeEventListener('pointerdown', outside)
      document.removeEventListener('keydown', escape)
      window.removeEventListener('resize', close)
      window.removeEventListener('scroll', close, true)
    }
  }, [useMenu])

  const loadGenerations = useCallback(async (showLoading = false) => {
    if (showLoading) setLoading(true)
    try {
      const next = await api<Generation[]>('/api/v1/generations')
      setGenerations(sortGenerationsNewestFirst(next))
      setLoadError('')
    } catch (problem) {
      setLoadError(problem instanceof Error ? problem.message : 'Could not read generations')
    } finally {
      if (showLoading) setLoading(false)
    }
  }, [])

  useEffect(() => {
    loadGenerations(true)
  }, [loadGenerations])

  useEffect(() => {
    if (typeof EventSource === 'undefined') {
      setStreamAvailable(false)
      return
    }
    const stream = new EventSource('/api/v1/generations/events')
    stream.onopen = () => setStreamAvailable(true)
    stream.onerror = () => setStreamAvailable(false)
    const receive = (event: Event) => {
      try {
        const payload = JSON.parse((event as MessageEvent).data) as { generations?: Generation[] }
        if (!Array.isArray(payload.generations)) return
        setGenerations(sortGenerationsNewestFirst(payload.generations))
        setLoadError('')
      } catch {
        // A later valid event or the polling fallback repairs the view.
      }
    }
    stream.addEventListener('generation', receive)
    return () => {
      stream.removeEventListener('generation', receive)
      stream.close()
    }
  }, [])

  // stagedID can hold any run, not only a completed one, once the strip has
  // been clicked. It is only re-picked here when the id it already holds no
  // longer names a run at all (deleted, or nothing staged yet). The newest
  // completed run is still the preference, matching what this screen always
  // opened to before the strip existed; but when nothing has completed yet,
  // the newest run of any status is staged instead, so the stage shows its
  // progress or queue state rather than sitting empty.
  useEffect(() => {
    setStagedID(current => {
      if (generations.some(generation => generation.id === current)) return current
      return generations.find(generation => generation.status === 'completed')?.id ?? generations[0]?.id ?? current
    })
  }, [generations])

  const hasActive = generations.some(generation => generationActive(generation.status))
  useEffect(() => {
    if (!hasActive && streamAvailable) return
    let cancelled = false
    const poll = () => {
      if (cancelled) return
      // A hidden tab with nothing active can wait for its next visit; a
      // hidden tab with something running or queued cannot, because the
      // completion sound and the title flash depend on this poll noticing
      // the transition while nothing else is watching (SSE included, since
      // this branch only runs at all when SSE is down or something is active).
      if (document.hidden && !hasActive) return
      loadGenerations()
    }
    const timer = setInterval(poll, 2000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [hasActive, loadGenerations, streamAvailable])

  // Both react to a run crossing into completed or failed, but not on the
  // same terms: the chime is the toggle's own promise ("sound when a run
  // finishes"), so it plays whether this tab is the one being looked at or
  // not; the title flash exists only to draw a look back to a tab nobody is
  // watching, so it stays hidden-only. The SSE handler and the poller both
  // flow through setGenerations above, so this effect is the single place
  // that notices a transition regardless of which path delivered it.
  useEffect(() => {
    const current = generationStatusMap(generations)
    const transitioned = finishedTransitions(statusRef.current, current)
    statusRef.current = current
    if (transitioned.length === 0) return
    if (soundOn && audioCtxRef.current) {
      const ctx = audioCtxRef.current
      transitioned.forEach((_id, index) => playChime(ctx, index * 0.2))
    }
    if (typeof document === 'undefined' || !document.hidden) return
    setUnseen(previous => markUnseen(previous, transitioned))
  }, [generations, soundOn])

  useEffect(() => {
    if (typeof document === 'undefined') return
    const onVisibility = () => {
      if (document.hidden) return
      setUnseen(new Set())
      document.title = ORIGINAL_TITLE
    }
    document.addEventListener('visibilitychange', onVisibility)
    return () => document.removeEventListener('visibilitychange', onVisibility)
  }, [])

  useEffect(() => {
    if (typeof document === 'undefined') return
    if (unseen.size === 0 || !document.hidden) return
    const timer = setInterval(() => {
      document.title = document.title === ORIGINAL_TITLE ? FLASH_TITLE : ORIGINAL_TITLE
    }, 1000)
    return () => {
      clearInterval(timer)
      document.title = ORIGINAL_TITLE
    }
  }, [unseen])

  // Posters are read from localStorage here, once per list change, rather
  // than inside Thumb's render: cachedPoster is a synchronous storage read,
  // and the strip can hold a lot of cards.
  const posters = useMemo(() => {
    const map = new Map<string, string>()
    for (const generation of generations) {
      const poster = cachedPoster(generation.id)
      if (poster) map.set(generation.id, poster)
    }
    return map
  }, [generations, posterTick])

  // At most one poster capture runs at a time. Each attempt either stores a
  // poster or marks the id as failed for this session, so a run that cannot
  // be captured (a corrupt file, a browser that refuses the codec) is tried
  // once and then left alone instead of retried on every render.
  //
  // generations gets a new array identity on every SSE tick and every poll,
  // including ticks that have nothing to do with the run being captured, so
  // this effect re-runs far more often than the capture itself changes. A
  // capture in progress is tracked by id in inFlightPosterRef rather than by
  // a plain busy flag: when the effect re-runs and that id is still present
  // in the list, the running capture is left alone instead of being aborted
  // and restarted, which is what used to make a large video's poster never
  // finish under continuous polling. It is only actually stopped here if its
  // target left the list (deleted); a true unmount is handled by the effect
  // below, so this one only ever aborts a capture that is no longer wanted.
  //
  // cancelled still gates only the final setPosterTick call, which exists
  // purely to look for the next target; it never gates storePoster, so a
  // capture that finishes despite being superseded is still kept.
  useEffect(() => {
    const inFlight = inFlightPosterRef.current
    if (inFlight) {
      // An aborted controller means the unmount cleanup below already ran.
      // Under StrictMode that happens on a synthetic unmount too, so the
      // entry no longer represents live work and must not block a restart.
      if (inFlight.controller.signal.aborted) {
        inFlightPosterRef.current = null
      } else if (generations.some(generation => generation.id === inFlight.id)) {
        return
      } else {
        inFlight.controller.abort()
        inFlightPosterRef.current = null
      }
    }
    const target = generations.find(generation => (
      generation.status === 'completed'
      && !posters.has(generation.id)
      && !posterFailedRef.current.has(generation.id)
    ))
    if (!target) return
    let cancelled = false
    let objectURL = ''
    const controller = new AbortController()
    inFlightPosterRef.current = { id: target.id, controller }
    apiBlob(generationFilePath(target), { signal: controller.signal }).then(blob => {
      objectURL = URL.createObjectURL(playableVideoBlob(blob))
      return capturePoster(objectURL)
    }).then(dataURI => {
      storePoster(target.id, dataURI)
    }).catch(problem => {
      if ((problem as { name?: string })?.name !== 'AbortError') posterFailedRef.current.add(target.id)
    }).finally(() => {
      if (objectURL) URL.revokeObjectURL(objectURL)
      if (inFlightPosterRef.current?.controller === controller) inFlightPosterRef.current = null
      if (!cancelled) setPosterTick(value => value + 1)
    })
    return () => { cancelled = true }
  }, [generations, posters])

  // The one place an in-flight capture is actually torn down mid-flight: a
  // real unmount. Empty deps means this cleanup runs only then, not on every
  // re-run of the effect above.
  useEffect(() => () => {
    inFlightPosterRef.current?.controller.abort()
  }, [])

  if (!config) return null

  const selectedSize = sizes.find(option => option.shortEdge === shortEdge)
  const imageMode = mode === MODE_IMAGE_TO_VIDEO
  const modeOffered = modes.some(option => option.mode === mode)
  // Counted in code points so this agrees with the server, which counts
  // runes. prompt.length would count UTF-16 units and disagree the moment
  // anyone writes an emoji.
  const promptLength = [...prompt.trim()].length
  const promptTooLong = promptLength > config.max_prompt_length
  const canSubmit = modeOffered && (!imageMode || Boolean(frame)) && Boolean(prompt.trim()) && !promptTooLong
    && Boolean(selectedSize) && blocks > 0 && !submitting && !staging
  const quantization = recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant : undefined
  const staged = generations.find(generation => generation.id === stagedID)

  const editPrompt = (value: string) => { setPrompt(value); setFilledNote(false) }
  const chooseShape = (value: CanvasShape) => { setShape(value); setFilledNote(false) }
  const chooseSize = (value: number) => { setShortEdge(value); setFilledNote(false) }
  const chooseDuration = (value: number) => { setBlocks(value); setFilledNote(false) }
  // The staged frame survives a trip through text mode: the image is already
  // on this Spark, and coming back to find the slot empty would mean staging
  // it again for nothing.
  const chooseMode = (value: string) => { setMode(value); setFrameError('') }

  const holdFrame = (next: ComposerFrame | null) => {
    if (framePreviewRef.current && framePreviewRef.current !== next?.preview) {
      URL.revokeObjectURL(framePreviewRef.current)
    }
    framePreviewRef.current = next?.preview ?? ''
    setFrame(next)
  }

  // One path for both ways an image reaches the slot: a file that was chosen
  // here, and a frame taken from a finished run. Either way it is staged on
  // this Spark first, and the slot fills only once the Spark has answered
  // with the id a generation can name.
  const addFrame = async (name: string, produce: () => Promise<Blob>) => {
    setFrameError('')
    setStaging(true)
    try {
      const blob = await produce()
      const response = await apiUpload<StagedMedia>('/api/v1/generations/media', blob, name)
      const image = stagedFrame(name, response)
      if (!image) throw new Error('Could not read the image size.')
      holdFrame({ ...image, preview: URL.createObjectURL(blob) })
      setFilledNote(false)
      const fit = fitCanvasToImage(config, image.width, image.height)
      if (fit) {
        setShape(fit.shape)
        setShortEdge(fit.shortEdge)
      }
    } catch (problem) {
      setFrameError(problem instanceof Error ? problem.message : 'Could not add this image')
    } finally {
      setStaging(false)
    }
  }

  const chooseFrameFile = (files: FileList | null) => {
    const file = pickImage([...(files ?? [])])
    if (file) void addFrame(file.name, async () => file)
  }

  // The run on the stage hands over its own element, so the frame taken is
  // the one on screen at its playhead. Any other run has no playhead to read,
  // so it is decoded off screen and gives up its first frame instead.
  const runFrame = async (source: Generation): Promise<Blob> => {
    const element = stageVideoRef.current
    if (element && source.id === stagedID && element.videoWidth > 0) return captureFrameBlob(element)
    const file = playableVideoBlob(await apiBlob(generationFilePath(source)))
    const url = URL.createObjectURL(file)
    try {
      return await runFrameBlob(url)
    } finally {
      URL.revokeObjectURL(url)
    }
  }

  const useAsFirstFrame = (source: Generation) => {
    setUseMenu(null)
    setMode(MODE_IMAGE_TO_VIDEO)
    void addFrame(`${source.id}.png`, () => runFrame(source))
  }

  const reuseThisPrompt = (source: Generation) => {
    const values = reuseValues(source, config)
    setPrompt(values.prompt)
    setShape(values.shape)
    setShortEdge(values.shortEdge)
    setBlocks(values.blocks)
    setSeed('')
    setFilledNote(true)
  }

  const copySeed = async (value: number) => {
    await copyText(String(value))
    setCopiedSeed(true)
    setTimeout(() => setCopiedSeed(false), 1600)
  }

  const toggleUseMenu = (source: Generation, event: React.MouseEvent<HTMLButtonElement>) => {
    const rect = event.currentTarget.getBoundingClientRect()
    setUseMenu(current => current?.id === source.id ? null : {
      id: source.id,
      top: rect.bottom + 4,
      right: Math.max(window.innerWidth - rect.right, 8),
    })
  }

  // AudioContext creation stays behind an actual user gesture (browsers
  // refuse to let it make sound otherwise), but a reload with the toggle
  // already on from a previous session never fires that toggle click, so
  // Generate is the other gesture this screen can lean on to arm it.
  const ensureAudioContext = () => {
    if (audioCtxRef.current) return
    const Ctor = window.AudioContext
    if (Ctor) audioCtxRef.current = new Ctor()
  }

  const toggleSound = () => {
    setSoundOn(current => {
      const next = !current
      window.localStorage.setItem(SOUND_KEY, next ? 'on' : 'off')
      if (next) ensureAudioContext()
      return next
    })
  }

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    setFormError('')
    if (!selectedSize || !modeOffered || (imageMode && !frame)) return
    let parsedSeed: number | undefined
    if (seed.trim()) {
      parsedSeed = Number(seed)
      if (!/^\d+$/.test(seed.trim()) || !Number.isSafeInteger(parsedSeed) || parsedSeed < 0) {
        setFormError('Seed must be a non-negative whole number.')
        return
      }
    }
    if (soundOn) ensureAudioContext()
    setSubmitting(true)
    try {
      await api<GenerateResponse>('/api/v1/generate', {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify(generationRequest({
          recipeID: recipe.id,
          mode,
          prompt,
          blocks,
          width: selectedSize.width,
          height: selectedSize.height,
          seed: parsedSeed,
          firstFrame: frame?.id,
        })),
      })
      setPrompt('')
      setFilledNote(false)
      await loadGenerations()
    } catch (problem) {
      setFormError(problem instanceof Error ? problem.message : 'Could not start this generation')
    } finally {
      setSubmitting(false)
    }
  }

  const cancel = async (generation: Generation) => {
    setBusyID(generation.id)
    try {
      await api(`/api/v1/generations/${encodeURIComponent(generation.id)}/cancel`, { method: 'POST', body: '{}' })
      await loadGenerations()
    } catch (problem) {
      noticeBox('Could not cancel this generation', problem instanceof Error ? problem.message : undefined)
    } finally {
      setBusyID('')
    }
  }

  const remove = async (generation: Generation) => {
    const { ok } = await confirmBox({
      title: 'Delete this generation?',
      body: generation.status === 'completed'
        ? 'Deletes the result file and generation record.'
        : 'Deletes this generation record from this Spark.',
      confirmLabel: 'Delete generation',
      danger: true,
    })
    if (!ok) return
    setBusyID(generation.id)
    try {
      await api(`/api/v1/generations/${encodeURIComponent(generation.id)}`, { method: 'DELETE', body: '{}' })
      forgetPoster(generation.id)
      posterFailedRef.current.delete(generation.id)
      await loadGenerations()
    } catch (problem) {
      noticeBox('Could not delete this generation', problem instanceof Error ? problem.message : undefined)
    } finally {
      setBusyID('')
    }
  }

  const finalElapsed = staged?.status === 'completed' && staged.finished_at
    ? generationElapsedSeconds(staged, Date.now())
    : null

  return (
    <div className="gen-layout">
      <div className="gen-left stack">
        <div className="section-head gen-context">
          <div className="glyph" aria-hidden="true">{modelGlyph(recipe.display_name)}</div>
          <div>
            <strong>{recipe.display_name}</strong>
            <div className="faint">
              Serving on this Spark{quantization ? ` · ${quantization} weights` : ''}
            </div>
          </div>
          <span className="spacer" />
          <span className="tag quiet">Video</span>
        </div>

        <section className="card">
          <form ref={formRef} className="gen-form" onSubmit={submit}>
            {/*
              One mode is not a choice, so the row is not drawn for it: the
              screen keeps exactly the layout it had before modes existed.
            */}
            {modes.length > 1 && (
              <div className="pill-group" role="radiogroup" aria-label="Mode">
                {modes.map(option => (
                  <label key={option.mode}>
                    <input
                      type="radio"
                      name="gen-mode"
                      value={option.mode}
                      checked={mode === option.mode}
                      onChange={() => chooseMode(option.mode)}
                    />
                    {option.label}
                  </label>
                ))}
              </div>
            )}

            <div className="composer gen-composer">
              {/*
                No maxLength. The browser enforces it by silently dropping
                whatever does not fit, so pasting a long shot-by-shot prompt
                left one that looked complete, ended mid-sentence, and
                generated from the half that survived. Over the limit the
                text stays exactly as written and Generate is what refuses.
              */}
              <textarea
                rows={3}
                aria-label="Prompt"
                aria-invalid={promptTooLong}
                placeholder="Describe the clip"
                value={prompt}
                onChange={event => editPrompt(event.target.value)}
                onKeyDown={event => {
                  if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
                    event.preventDefault()
                    if (canSubmit) formRef.current?.requestSubmit()
                  }
                }}
              />
            </div>
            {imageMode && <p className="gen-prompt-hint">The image sets the look. The prompt sets the motion.</p>}
            {/*
              Shown from three quarters of the way, not always: a counter on
              an empty field is noise, and one that appears only at the
              moment of refusal is a surprise.
            */}
            {promptLength > config.max_prompt_length * 0.75 && (
              <p className={promptTooLong ? 'error-text prompt-count' : 'faint prompt-count'}>
                {promptTooLong
                  ? `${promptLength.toLocaleString()} of ${config.max_prompt_length.toLocaleString()} characters. Remove ${(promptLength - config.max_prompt_length).toLocaleString()} to generate.`
                  : `${promptLength.toLocaleString()} of ${config.max_prompt_length.toLocaleString()} characters`}
              </p>
            )}
            {filledNote && (
              <div className="filled-note" role="status">
                <p>Filled from the staged result. Seed cleared for a new take.</p>
              </div>
            )}

            {imageMode && (
              <div className="field">
                <span>First frame</span>
                {/*
                  The whole slot takes a drop; the button inside it is what
                  opens the picker, so choosing an image never depends on a
                  pointer. An empty slot is the only reason Generate is off in
                  this mode, which is why it carries no explaining line.
                */}
                <div
                  className={`frame-slot${frame ? ' filled' : ''}`}
                  onDragOver={event => event.preventDefault()}
                  onDrop={event => {
                    event.preventDefault()
                    chooseFrameFile(event.dataTransfer.files)
                  }}
                >
                  {frame ? (
                    <>
                      <img className="frame-pic" src={frame.preview} alt="" />
                      <span className="frame-cap">{stagedFrameCaption(frame)}</span>
                      <button
                        type="button"
                        className="frame-clear"
                        aria-label="Remove the first frame"
                        onClick={() => { holdFrame(null); setFrameError('') }}
                      >
                        ×
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      className="frame-pick"
                      disabled={staging}
                      onClick={() => frameInputRef.current?.click()}
                    >
                      {staging ? 'Adding the image' : 'Drop an image or click to choose'}
                    </button>
                  )}
                  <input
                    ref={frameInputRef}
                    type="file"
                    accept={IMAGE_ACCEPT}
                    hidden
                    onChange={event => {
                      chooseFrameFile(event.target.files)
                      event.target.value = ''
                    }}
                  />
                </div>
                {frameError && <div className="error-note" role="alert"><p>{frameError}</p></div>}
              </div>
            )}

            <div className="gen-controls">
              <div className="gen-canvas">
                <div className="field">
                  <span>Shape</span>
                  <div className="pill-group gen-shape-pills" role="radiogroup" aria-label="Shape">
                    {canvasShapes().map(option => (
                      <label key={option.shape}>
                        <input
                          type="radio"
                          name="gen-shape"
                          value={option.shape}
                          checked={shape === option.shape}
                          onChange={() => chooseShape(option.shape)}
                        />
                        <span className={`shape-glyph ${option.shape}`} aria-hidden="true" />
                        {option.label}
                        <span className="shape-ratio">{option.ratio}</span>
                      </label>
                    ))}
                  </div>
                </div>
                <div className="field">
                  <span>Size</span>
                  <div className="size-group" role="radiogroup" aria-label="Size">
                    {sizes.map(option => {
                      const waitLabel = sizeWaitLabel(config, option.shortEdge)
                      return (
                        <label key={option.value} className="size-option">
                          <input
                            type="radio"
                            name="gen-size"
                            value={option.value}
                            checked={shortEdge === option.shortEdge}
                            onChange={() => chooseSize(option.shortEdge)}
                          />
                          <span className="size-px">{option.label}</span>
                          {waitLabel && <span className="size-wait">{waitLabel}</span>}
                        </label>
                      )
                    })}
                  </div>
                  {imageMode && frame && selectedSize && (
                    <span className="hint fit-line">
                      {fitArithmetic(frame.width, frame.height, selectedSize.width, selectedSize.height)}
                    </span>
                  )}
                  <span className="hint">{sizeWaitHint(config)}</span>
                </div>
              </div>
              <div className="field">
                <span>Duration</span>
                <div className="pill-group gen-duration-pills" role="radiogroup" aria-label="Duration">
                  {durations.map(option => (
                    <label key={option.blocks} title={`${option.frames} frames`}>
                      <input
                        type="radio"
                        name="gen-duration"
                        value={option.blocks}
                        checked={blocks === option.blocks}
                        onChange={() => chooseDuration(option.blocks)}
                      />
                      {option.label}
                    </label>
                  ))}
                </div>
                {blocks > 0 && <span className="hint">{durationArithmetic(config, blocks)}</span>}
              </div>
              <label className="field">
                <span>Seed (optional)</span>
                <input
                  type="text"
                  inputMode="numeric"
                  placeholder="Generated automatically"
                  value={seed}
                  onChange={event => setSeed(event.target.value)}
                />
              </label>
            </div>

            {sizes.length === 0 && <p className="error-text">No canvas size fits this model&apos;s grid.</p>}
            {modes.length === 0 && <p className="error-text">This model offers no generation mode.</p>}
            {formError && <div className="error-note" role="alert"><p>{formError}</p></div>}

            <div className="gen-foot">
              <button className="primary" type="submit" disabled={!canSubmit}>
                {submitting ? 'Starting' : <>Generate <span className="kbd" aria-hidden="true">{shortcutGlyph()}</span></>}
              </button>
              <p className="faint">
                Up to {config.concurrent_generations} run at a time; extra runs queue. Keeps generating if you leave.
              </p>
            </div>
          </form>
        </section>
      </div>

      <section className="card gen-right" aria-labelledby="generation-results-title">
        <div className="section-head">
          <h2 id="generation-results-title">Results</h2>
          <button type="button" className="sound-toggle" aria-pressed={soundOn} onClick={toggleSound}>
            <span className="box" aria-hidden="true">{soundOn ? '✓' : ''}</span>
            Sound when a run finishes
          </button>
          <span className="spacer" />
          <span className="muted gen-results-note">Played from local disk only</span>
        </div>

        {loading && <div className="stage-empty">Reading generations…</div>}
        {!loading && loadError && <div className="error-note" role="alert"><p>{loadError}</p></div>}

        {!loading && !loadError && (
          <>
            <div className="stage">
              {staged?.status === 'completed' ? (
                <div className="stage-player">
                  <GenerationVideo generation={staged} videoRef={stageVideoRef} />
                  {/* The two numbers a run has to be repeated from, on the
                      run itself, so reproducing it is not archaeology. */}
                  <div className="stage-chips">
                    <span className="chip"><span className="k">SIZE</span>{staged.width}×{staged.height}</span>
                    <span className="chip"><span className="k">SEED</span>{staged.seed}</span>
                    <button type="button" className="chip copy" onClick={() => void copySeed(staged.seed)}>
                      {copiedSeed ? 'Copied' : 'Copy seed'}
                    </button>
                  </div>
                </div>
              ) : staged && generationActive(staged.status) ? (
                <div className="stage-empty">
                  {staged.status === 'running'
                    ? <GenerationProgress generation={staged} />
                    : (
                      <div className="gen-working" role="status">
                        <span className="sdot wait" aria-hidden="true" />
                        <span>{staged.queue_position ? `Queue position ${staged.queue_position}` : 'Waiting in the queue'}</span>
                      </div>
                    )}
                </div>
              ) : staged ? (
                <div className="stage-empty">
                  <span className={`gen-state ${staged.status}`}>{generationState(staged.status)}</span>
                </div>
              ) : generations.length === 0 ? (
                <div className="stage-empty">No generations yet. Describe a clip and generate it.</div>
              ) : null}

              {staged && (
                <>
                  <div className="stage-meta">
                    <span className={`gen-state ${staged.status}`}>{generationState(staged.status)}</span>
                    <span className="stage-model">{generationModelName(recipes, staged)}</span>
                    {/* On a completed run the player's chips carry size and
                        seed; repeating them here would say the same numbers
                        twice on one screen. */}
                    {staged.status !== 'completed' && <span>{staged.width} × {staged.height}</span>}
                    <span>{durationString(staged, config)}</span>
                    {staged.status !== 'completed' && <span>Seed {staged.seed}</span>}
                    {finalElapsed !== null && <span>Generated in {formatElapsed(finalElapsed)}</span>}
                  </div>
                  <p className="stage-prompt">{staged.prompt}</p>
                  {staged.error && staged.status !== 'completed' && (
                    <p className="gen-result-error" role={staged.status === 'failed' ? 'alert' : undefined}>{staged.error}</p>
                  )}
                  <div className="actions">
                    {staged.status === 'completed' && <a className="gen-download" href={generationFilePath(staged)}>Download</a>}
                    {(staged.status === 'completed' || staged.status === 'failed') && (
                      <button type="button" className="quiet reuse" onClick={() => reuseThisPrompt(staged)}>Use this prompt</button>
                    )}
                    {generationActive(staged.status) && (
                      <button type="button" className="quiet" disabled={busyID === staged.id} onClick={() => cancel(staged)}>Cancel</button>
                    )}
                    {generationTerminal(staged.status) && (
                      <button type="button" className="quiet" disabled={busyID === staged.id} onClick={() => remove(staged)}>Delete</button>
                    )}
                  </div>
                </>
              )}
            </div>

            {generations.length > 0 && (
              <>
                <div className="section-head strip-head">
                  <span className="muted">All runs · newest first</span>
                </div>
                <div className="strip">
                  {generations.map(generation => (
                    <div className="run" key={generation.id}>
                      <Thumb
                        generation={generation}
                        modelName={generationModelName(recipes, generation)}
                        poster={posters.get(generation.id) ?? null}
                        selected={generation.id === stagedID}
                        onSelect={() => setStagedID(generation.id)}
                      />
                      {generation.status === 'completed' && (
                        <div className="use-wrap" ref={useMenu?.id === generation.id ? useMenuRef : undefined}>
                          <button
                            type="button"
                            className="use"
                            aria-haspopup="menu"
                            aria-expanded={useMenu?.id === generation.id}
                            onClick={event => toggleUseMenu(generation, event)}
                          >
                            Use <span aria-hidden="true">▾</span>
                          </button>
                          {useMenu?.id === generation.id && (
                            // Placed against the viewport: the strip scrolls
                            // sideways and would otherwise cut the menu off.
                            <div
                              className="usemenu"
                              role="menu"
                              style={{ top: useMenu.top, right: useMenu.right }}
                            >
                              <button
                                type="button"
                                role="menuitem"
                                onClick={() => { setUseMenu(null); reuseThisPrompt(generation) }}
                              >
                                Use the prompt
                              </button>
                              <button
                                type="button"
                                role="menuitem"
                                onClick={() => useAsFirstFrame(generation)}
                              >
                                Use as first frame
                              </button>
                            </div>
                          )}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              </>
            )}
          </>
        )}
      </section>
    </div>
  )
}
