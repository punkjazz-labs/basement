import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  api, apiBlob, idempotency,
  type GenerateResponse, type Generation, type Recipe,
} from '../api'
import {
  canvasOptions, durationOptions, formatElapsed, generationActive, generationElapsedSeconds,
  generationMode, generationState, generationTerminal,
} from '../generation'
import { readableWeights } from '../catalog'
import { confirmBox, noticeBox } from '../confirm'

interface GenerateProps {
  recipe: Recipe
  recipes: Recipe[]
}

const modelGlyph = (name: string): string =>
  name.split(/\s+/).filter(Boolean).slice(0, 2).map(word => word[0]).join('').toUpperCase()

const generationFilePath = (generation: Generation): string =>
  generation.file_url ?? `/api/v1/generations/${encodeURIComponent(generation.id)}/file`

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
function GenerationVideo({ generation }: { generation: Generation }) {
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
      objectURL = URL.createObjectURL(blob)
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

function GenerationCard({ generation, recipe, busy, selected, onSelect, onCancel, onDelete }: {
  generation: Generation
  recipe?: Recipe
  busy: boolean
  selected: boolean
  onSelect: () => void
  onCancel: () => void
  onDelete: () => void
}) {
  const active = generationActive(generation.status)
  const terminal = generationTerminal(generation.status)
  const config = recipe?.media_generation
  const matchesCurrentFrameGrid = config
    && generation.frames === config.frame_block * generation.blocks + config.frame_offset
  const seconds = matchesCurrentFrameGrid && config.frames_per_second > 0
    ? generation.frames / config.frames_per_second
    : null
  const duration = seconds === null
    ? `${generation.frames} frames`
    : `${Math.round(seconds * 10) / 10}s · ${generation.frames} frames`
  const finalElapsed = generation.finished_at
    ? generationElapsedSeconds(generation, Date.now())
    : null

  return (
    <article className={`gen-result ${generation.status}`}>
      <div className="gen-result-head">
        <span className={`gen-state ${generation.status}`}>{generationState(generation.status)}</span>
        <span className="gen-result-model">{recipe?.display_name ?? generation.model_id}</span>
      </div>
      <p className="gen-result-prompt" title={generation.prompt}>{generation.prompt}</p>
      <div className="gen-result-meta">
        <span>{generationMode(generation.mode)}</span>
        <span>{generation.width} × {generation.height}</span>
        <span>{duration}</span>
        <span>Seed {generation.seed}</span>
      </div>

      {generation.status === 'running' && (
        <GenerationProgress generation={generation} />
      )}
      {generation.status === 'queued' && (
        <div className="gen-working" role="status">
          <span className="sdot wait" aria-hidden="true" />
          <span>{generation.queue_position ? `Queue position ${generation.queue_position}` : 'Waiting in the queue'}</span>
        </div>
      )}
      {generation.status === 'completed' && selected && <GenerationVideo generation={generation} />}
      {generation.status === 'completed' && finalElapsed !== null && (
        <span className="gen-finished-time">Generated in {formatElapsed(finalElapsed)}</span>
      )}
      {generation.error && generation.status !== 'completed' && (
        <p className="gen-result-error" role={generation.status === 'failed' ? 'alert' : undefined}>{generation.error}</p>
      )}

      <div className="gen-result-actions">
        {generation.status === 'completed' && !selected && (
          <button className="quiet" onClick={onSelect}>Play video</button>
        )}
        {generation.status === 'completed' && (
          <a className="gen-download" href={generationFilePath(generation)}>Download</a>
        )}
        {active && <button className="quiet" disabled={busy} onClick={onCancel}>Cancel</button>}
        {terminal && <button className="quiet" disabled={busy} onClick={onDelete}>Delete</button>}
      </div>
    </article>
  )
}

export default function Generate({ recipe, recipes }: GenerateProps) {
  const config = recipe.media_generation
  const sizes = useMemo(() => config ? canvasOptions(config) : [], [config])
  const durations = useMemo(() => config ? durationOptions(config) : [], [config])
  const [prompt, setPrompt] = useState('')
  const [size, setSize] = useState('')
  const [blocks, setBlocks] = useState(0)
  const [seed, setSeed] = useState('')
  const [generations, setGenerations] = useState<Generation[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [formError, setFormError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [busyID, setBusyID] = useState('')
  const [selectedVideoID, setSelectedVideoID] = useState('')
  const [streamAvailable, setStreamAvailable] = useState(true)

  useEffect(() => {
    const preferred = sizes[1] ?? sizes[0]
    setSize(current => sizes.some(option => option.value === current) ? current : preferred?.value ?? '')
  }, [sizes])

  useEffect(() => {
    const preferred = durations.find(option => option.blocks === config?.default_blocks) ?? durations[0]
    setBlocks(current => durations.some(option => option.blocks === current) ? current : preferred?.blocks ?? 0)
  }, [durations, config?.default_blocks])

  const loadGenerations = useCallback(async (showLoading = false) => {
    if (showLoading) setLoading(true)
    try {
      const next = await api<Generation[]>('/api/v1/generations')
      setGenerations([...next].sort((left, right) => right.created_at.localeCompare(left.created_at)))
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
        setGenerations([...payload.generations].sort((left, right) => right.created_at.localeCompare(left.created_at)))
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

  useEffect(() => {
    setSelectedVideoID(current => {
      if (generations.some(generation => generation.id === current && generation.status === 'completed')) {
        return current
      }
      return generations.find(generation => generation.status === 'completed')?.id ?? ''
    })
  }, [generations])

  const hasActive = generations.some(generation => generationActive(generation.status))
  useEffect(() => {
    if (!hasActive && streamAvailable) return
    let cancelled = false
    const poll = () => {
      if (document.hidden || cancelled) return
      loadGenerations()
    }
    const timer = setInterval(poll, 2000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [hasActive, loadGenerations, streamAvailable])

  if (!config) return null

  const selectedSize = sizes.find(option => option.value === size)
  const textMode = config.modes.includes('text_to_video')
  const imageMode = config.modes.includes('image_to_video')
  const canSubmit = textMode && Boolean(prompt.trim()) && Boolean(selectedSize) && blocks > 0 && !submitting
  const quantization = recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant : undefined

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    setFormError('')
    if (!selectedSize || !textMode) return
    let parsedSeed: number | undefined
    if (seed.trim()) {
      parsedSeed = Number(seed)
      if (!/^\d+$/.test(seed.trim()) || !Number.isSafeInteger(parsedSeed) || parsedSeed < 0) {
        setFormError('Seed must be a non-negative whole number.')
        return
      }
    }
    setSubmitting(true)
    try {
      await api<GenerateResponse>('/api/v1/generate', {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({
          model_id: recipe.id,
          mode: 'text_to_video',
          prompt: prompt.trim(),
          blocks,
          width: selectedSize.width,
          height: selectedSize.height,
          ...(parsedSeed === undefined ? {} : { seed: parsedSeed }),
        }),
      })
      setPrompt('')
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
        ? 'Deletes the result file and its generation record from this Spark.'
        : 'Deletes this generation record from this Spark.',
      confirmLabel: 'Delete generation',
      danger: true,
    })
    if (!ok) return
    setBusyID(generation.id)
    try {
      await api(`/api/v1/generations/${encodeURIComponent(generation.id)}`, { method: 'DELETE', body: '{}' })
      await loadGenerations()
    } catch (problem) {
      noticeBox('Could not delete this generation', problem instanceof Error ? problem.message : undefined)
    } finally {
      setBusyID('')
    }
  }

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
          <form className="gen-form" onSubmit={submit}>
            <div>
              <div className="pill-group" role="radiogroup" aria-label="Mode">
                <label className={!textMode ? 'disabled' : ''}>
                  <input type="radio" name="gen-mode" checked={textMode} disabled={!textMode} readOnly />
                  Text to video
                </label>
                {imageMode && (
                  <label className="disabled" title="Image to video is not available yet">
                    <input type="radio" name="gen-mode" disabled />
                    Image to video
                  </label>
                )}
              </div>
              {imageMode && <p className="gen-mode-note">Image to video is not available yet.</p>}
            </div>

            <div className="composer">
              <textarea
                rows={3}
                maxLength={2000}
                aria-label="Prompt"
                placeholder="Describe the clip"
                value={prompt}
                onChange={event => setPrompt(event.target.value)}
              />
            </div>

            <div className="gen-controls">
              <div className="field">
                <span>Size</span>
                <div className="pill-group" role="radiogroup" aria-label="Size">
                  {sizes.map(option => (
                    <label key={option.value}>
                      <input
                        type="radio"
                        name="gen-size"
                        value={option.value}
                        checked={size === option.value}
                        onChange={() => setSize(option.value)}
                      />
                      {option.label}
                    </label>
                  ))}
                </div>
                <span className="hint">
                  Short edge up to {config.max_short_edge}px, long edge up to {config.max_long_edge}px. Width and height must be multiples of {config.canvas_multiple}.
                </span>
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
                        onChange={() => setBlocks(option.blocks)}
                      />
                      {option.label}
                    </label>
                  ))}
                </div>
                <span className="hint">
                  {config.frame_block} frames per block, plus {config.frame_offset}, at {config.frames_per_second} frames per second.
                </span>
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

            {sizes.length === 0 && <p className="error-text">This recipe does not provide a canvas that fits the approved size presets.</p>}
            {!textMode && <p className="error-text">Text to video is not available for this model.</p>}
            {formError && <div className="error-note" role="alert"><p>{formError}</p></div>}

            <div className="gen-foot">
              <p className="faint">
                {config.concurrent_generations === 1
                  ? 'Generations run one at a time. New ones wait in the queue. '
                  : `Up to ${config.concurrent_generations} generations run at a time. New ones wait in the queue. `}
                A generation keeps running if you leave this view.
              </p>
              <button className="primary" type="submit" disabled={!canSubmit}>
                {submitting ? 'Starting' : 'Generate'}
              </button>
            </div>
          </form>
        </section>
      </div>

      <section className="card gen-right" aria-labelledby="generation-results-title">
        <div className="section-head">
          <h2 id="generation-results-title">Results</h2>
          <span className="spacer" />
          <span className="muted gen-results-note">Newest first · played from local disk only</span>
        </div>
        {loading && <div className="gen-results-empty">Reading generations…</div>}
        {!loading && loadError && <div className="error-note" role="alert"><p>{loadError}</p></div>}
        {!loading && !loadError && generations.length === 0 && (
          <div className="gen-results-empty">No generations yet. Describe a clip and generate it.</div>
        )}
        {!loading && generations.length > 0 && (
          <div className="gen-results-list">
            {generations.map(generation => (
              <GenerationCard
                key={generation.id}
                generation={generation}
                recipe={recipes.find(item => item.id === generation.model_id)}
                busy={busyID === generation.id}
                selected={selectedVideoID === generation.id}
                onSelect={() => setSelectedVideoID(generation.id)}
                onCancel={() => cancel(generation)}
                onDelete={() => remove(generation)}
              />
            ))}
          </div>
        )}
      </section>
    </div>
  )
}
