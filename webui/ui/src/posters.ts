// Poster capture and cache for the Generate screen's run strip. Each
// finished run gets one thumbnail frame captured client-side from the video
// the console already downloaded, cached in localStorage so the strip does
// not re-decode the video on every reload. The Generate screen wires these
// functions in later; this module only owns the capture and the cache.

const ENTRY_PREFIX = 'basement.generate.poster.'
const INDEX_KEY = 'basement.generate.posters'
const MAX_ENTRIES = 200

const entryKey = (id: string): string => `${ENTRY_PREFIX}${id}`

function readIndex(store: Storage): string[] {
  const raw = store.getItem(INDEX_KEY)
  if (!raw) return []
  try {
    const parsed: unknown = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed.filter((entry): entry is string => typeof entry === 'string') : []
  } catch {
    return []
  }
}

// Every write to the store goes through here so a quota error anywhere
// (the poster entry itself, or the index once it has been trimmed) never
// escapes as an uncaught exception. A missing poster is a cosmetic loss,
// not a failure worth surfacing to the caller.
function safeSet(store: Storage, key: string, value: string): boolean {
  try {
    store.setItem(key, value)
    return true
  } catch {
    return false
  }
}

export function cachedPoster(id: string): string | null {
  return window.localStorage.getItem(entryKey(id))
}

export function storePoster(id: string, dataURI: string, store: Storage = window.localStorage): void {
  const index = readIndex(store).filter(existing => existing !== id)

  const evictOldest = (): boolean => {
    const oldest = index.shift()
    if (oldest === undefined) return false
    store.removeItem(entryKey(oldest))
    return true
  }

  // The cap applies whether or not the store is near quota, so a long
  // session does not grow the index without bound just because there is
  // still room to write.
  while (index.length >= MAX_ENTRIES) {
    if (!evictOldest()) break
  }

  for (;;) {
    const entryWritten = safeSet(store, entryKey(id), dataURI)
    const indexWritten = entryWritten && safeSet(store, INDEX_KEY, JSON.stringify([...index, id]))
    if (indexWritten) return

    // Either write hit quota. An entry with no matching index membership is
    // an orphan that no future eviction could ever reach (nothing else ever
    // looks at raw keys), so undo it before making room for another try.
    if (entryWritten) store.removeItem(entryKey(id))
    if (!evictOldest()) {
      // Nothing left to evict and the write still fails. Persist whatever
      // eviction already happened and give up on this poster silently.
      safeSet(store, INDEX_KEY, JSON.stringify(index))
      return
    }
  }
}

export function forgetPoster(id: string, store: Storage = window.localStorage): void {
  store.removeItem(entryKey(id))
  const index = readIndex(store).filter(existing => existing !== id)
  safeSet(store, INDEX_KEY, JSON.stringify(index))
}

// Grabs one representative frame from a finished run's video and returns it
// as a small JPEG data URI, for the run strip's poster thumbnail. Kept free
// of module state and excluded from the unit tests above: jsdom cannot
// decode media, so this is exercised by hand in the browser once the
// Generate screen wires it in.
export function capturePoster(videoURL: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const video = document.createElement('video')
    let frameHandle: number | null = null

    const cleanup = () => {
      video.removeEventListener('loadeddata', onFrame)
      video.removeEventListener('error', onError)
      if (frameHandle !== null) video.cancelVideoFrameCallback(frameHandle)
      video.removeAttribute('src')
      video.load()
    }

    const onFrame = () => {
      const width = 128
      const ratio = video.videoWidth > 0 ? video.videoHeight / video.videoWidth : 1
      const canvas = document.createElement('canvas')
      canvas.width = width
      canvas.height = Math.round(width * ratio)
      const context = canvas.getContext('2d')
      if (!context) {
        cleanup()
        reject(new Error('canvas 2d context unavailable for poster capture'))
        return
      }
      context.drawImage(video, 0, 0, canvas.width, canvas.height)
      const dataURI = canvas.toDataURL('image/jpeg', 0.7)
      cleanup()
      resolve(dataURI)
    }

    const onError = () => {
      cleanup()
      reject(new Error('video failed to load while capturing a poster'))
    }

    video.addEventListener('error', onError, { once: true })

    if (typeof video.requestVideoFrameCallback === 'function') {
      frameHandle = video.requestVideoFrameCallback(onFrame)
    } else {
      video.addEventListener('loadeddata', onFrame, { once: true })
    }

    video.muted = true
    video.playsInline = true
    video.preload = 'auto'
    video.src = videoURL
    video.currentTime = 0
  })
}
