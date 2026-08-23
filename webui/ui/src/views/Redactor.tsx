import { Fragment, useRef, useState } from 'react'
import {
  api, ApiError, type DocredactFinding, type DocredactModelPass, type DocredactModelPassResponse,
  type DocredactRestoreResponse, type DocredactSession,
} from '../api'
import { noticeBox } from '../confirm'
import {
  ACCEPT_ATTRIBUTE, ACCEPT_HINT, documentMeta, exportNames, groupFindings, parseMappingFile, passReceipt,
  pickDocument, previewParagraphs, previewSegments, restoreEntriesFromFindings, type RestoreEntry,
  restoredHint, restoreSegments, segmentsText, selectionLiteral, sourceClass, sourceLabel, strayLine,
  toggledFindings, wordCount,
} from '../docredact'

// The document never reaches this manager's disk and the redacted copy never
// leaves it as a file: the browser reads the text, the manager holds one
// analyzed session in memory, and both exports arrive as downloads.

interface OpenDocument {
  sessionID: string
  name: string
  words: number
}

interface HideOffer {
  literal: string
  top: number
  left: number
}

const message = (problem: unknown): string | undefined =>
  problem instanceof Error ? problem.message : undefined

export default function Redactor() {
  const [doc, setDoc] = useState<OpenDocument | null>(null)
  const [findings, setFindings] = useState<DocredactFinding[]>([])
  const [preview, setPreview] = useState('')
  const [busy, setBusy] = useState(false)
  const [offer, setOffer] = useState<HideOffer | null>(null)
  const [pass, setPass] = useState<DocredactModelPass | null>(null)
  // The `accepted` count captured right before this pass was asked, so the
  // receipt can report the growth since then rather than the server's
  // cumulative total.
  const [passBefore, setPassBefore] = useState(0)
  const [asking, setAsking] = useState(false)
  const [passProblem, setPassProblem] = useState<string | null>(null)
  const [mode, setMode] = useState<'redact' | 'restore'>('redact')
  const [reply, setReply] = useState('')
  const [mappingFile, setMappingFile] = useState<{ name: string; raw: string; entries: RestoreEntry[] } | null>(null)
  const [mappingProblem, setMappingProblem] = useState(false)
  const [restored, setRestored] = useState<DocredactRestoreResponse | null>(null)
  const [restoring, setRestoring] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const previewPane = useRef<HTMLElement>(null)
  // Bumped whenever the restore inputs change, so a response for an answer
  // nobody is waiting for anymore can be told apart from the current one.
  const restoreSeq = useRef(0)

  const sessionPath = (suffix: string) =>
    `/api/v1/docredact/sessions/${encodeURIComponent(doc?.sessionID ?? '')}${suffix}`

  const loadPreview = async (sessionID: string) => {
    const body = await api<{ text: string }>(
      `/api/v1/docredact/sessions/${encodeURIComponent(sessionID)}/preview`)
    setPreview(body.text)
  }

  const open = async (file: File) => {
    setBusy(true)
    setOffer(null)
    setPass(null)
    setPassBefore(0)
    setPassProblem(null)
    setRestored(null)
    restoreSeq.current++
    try {
      const text = await file.text()
      const session = await api<DocredactSession>('/api/v1/docredact/analyze', {
        method: 'POST',
        body: JSON.stringify({ text, name: file.name }),
      })
      setDoc({ sessionID: session.session_id, name: file.name, words: wordCount(text) })
      setFindings(session.findings ?? [])
      await loadPreview(session.session_id)
    } catch (problem) {
      noticeBox('Could not read that document', message(problem))
    } finally {
      setBusy(false)
    }
  }

  const openFrom = (files: FileList | null) => {
    const file = pickDocument([...(files ?? [])])
    if (file) void open(file)
  }

  const toggle = async (finding: DocredactFinding) => {
    const previous = findings
    setFindings(toggledFindings(findings, finding.id))
    setOffer(null)
    try {
      const body = await api<{ findings: DocredactFinding[] }>(
        sessionPath(`/findings/${encodeURIComponent(finding.id)}/toggle`),
        { method: 'POST', body: JSON.stringify({ enabled: !finding.enabled }) },
      )
      setFindings(body.findings ?? [])
      if (doc) await loadPreview(doc.sessionID)
    } catch (problem) {
      setFindings(previous)
      noticeBox('Could not change that finding', message(problem))
    }
  }

  // A selection in the preview is only an offer to hide something: nothing is
  // added until the owner presses Hide.
  const readSelection = () => {
    const selection = window.getSelection()
    const pane = previewPane.current
    if (!selection || selection.rangeCount === 0 || !pane) {
      setOffer(null)
      return
    }
    const literal = selectionLiteral(selection.toString())
    if (literal === '') {
      setOffer(null)
      return
    }
    // Beside the selection, on its own line, the way the design puts it: near
    // enough to be about that text, and never over the text it is about. The
    // last selection on a line is pulled back inside the pane instead of
    // disappearing off its edge.
    const box = selection.getRangeAt(0).getBoundingClientRect()
    const paneBox = pane.getBoundingClientRect()
    const left = box.right - paneBox.left + pane.scrollLeft + 6
    setOffer({
      literal,
      top: box.top - paneBox.top + pane.scrollTop - 3,
      left: Math.max(Math.min(left, pane.clientWidth - 74), 0),
    })
  }

  const hide = async (literal: string) => {
    setOffer(null)
    window.getSelection()?.removeAllRanges()
    try {
      const body = await api<{ findings: DocredactFinding[] }>(sessionPath('/findings'), {
        method: 'POST',
        body: JSON.stringify({ literal }),
      })
      setFindings(body.findings ?? [])
      if (doc) await loadPreview(doc.sessionID)
    } catch (problem) {
      noticeBox('Could not hide that text', message(problem))
    }
  }

  // The model pass is one POST: the manager picks the model (redactor role
  // first), runs every chunk, and answers with the findings as they now
  // stand. A degraded pass is an answer, not an error; only "no text model
  // is serving" refuses, and that sentence is shown as the manager wrote it.
  const askModel = async () => {
    if (!doc || asking) return
    const before = pass?.accepted ?? 0
    setAsking(true)
    setPassProblem(null)
    try {
      const body = await api<DocredactModelPassResponse>(sessionPath('/modelpass'), { method: 'POST' })
      setPass(body.model_pass)
      setPassBefore(before)
      setFindings(body.findings ?? [])
      await loadPreview(doc.sessionID)
    } catch (problem) {
      if (problem instanceof ApiError && problem.status === 503) setPassProblem(problem.message)
      else noticeBox('Could not ask the model', message(problem))
    } finally {
      setAsking(false)
    }
  }

  // Both files, one press: the redacted copy is what leaves this machine, the
  // mapping is what turns it back, and neither is useful without the other.
  const exportSafeCopy = () => {
    if (!doc) return
    const names = exportNames(doc.name)
    const save = (suffix: string, filename: string) => {
      const link = document.createElement('a')
      link.href = sessionPath(suffix)
      link.download = filename
      document.body.appendChild(link)
      link.click()
      link.remove()
    }
    save('/export/redacted', names.redacted)
    // Browsers drop a second download fired in the same tick as the first.
    window.setTimeout(() => save('/export/mapping', names.mapping), 150)
  }

  // The server does the restoring; this screen only shows the answer. With a
  // session open its live mapping is used. Without one, the saved mapping
  // file's own bytes go along, parsed again server-side by the code that
  // wrote them.
  const runRestore = async () => {
    if (restoring || reply.trim() === '') return
    // A reply edited, a new document, or a new mapping makes an in-flight
    // answer about the old inputs; it must be dropped, not shown.
    const seq = restoreSeq.current
    setRestoring(true)
    try {
      const body = doc
        ? await api<DocredactRestoreResponse>(sessionPath('/restore'), {
            method: 'POST', body: JSON.stringify({ text: reply }),
          })
        : await api<DocredactRestoreResponse>('/api/v1/docredact/restore', {
            method: 'POST', body: JSON.stringify({ text: reply, mapping: mappingFile?.raw ?? '' }),
          })
      if (seq === restoreSeq.current) setRestored(body)
    } catch (problem) {
      noticeBox('Could not restore that reply', message(problem))
    } finally {
      setRestoring(false)
    }
  }

  const openMapping = async (file: File | null) => {
    if (!file) return
    const raw = await file.text()
    const entries = parseMappingFile(raw)
    if (entries === null) {
      setMappingFile(null)
      setMappingProblem(true)
      return
    }
    setMappingProblem(false)
    setMappingFile({ name: file.name, raw, entries })
    setRestored(null)
    restoreSeq.current++
  }

  // The restored text stays on screen; copy is the only way it leaves. The
  // clipboard API is absent when the console is reached over plain http on
  // the LAN, so a hidden textarea is the fallback there.
  const copyRestored = () => {
    if (restored === null) return
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(restored.text)
      return
    }
    const area = document.createElement('textarea')
    area.value = restored.text
    area.style.position = 'fixed'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    document.execCommand('copy')
    area.remove()
  }

  const jobs = (
    <div className="jobs">
      <button aria-current={mode === 'redact'} onClick={() => setMode('redact')}>Redact</button>
      <button aria-current={mode === 'restore'} onClick={() => setMode('restore')}>Restore</button>
    </div>
  )

  const picker = (
    <input
      ref={fileInput}
      type="file"
      accept={ACCEPT_ATTRIBUTE}
      hidden
      onChange={event => {
        openFrom(event.target.files)
        event.target.value = ''
      }}
    />
  )

  if (mode === 'restore') {
    const entries = doc ? restoreEntriesFromFindings(findings) : (mappingFile?.entries ?? [])
    const segments = restored === null ? null : restoreSegments(reply, entries)
    // The server's text is the truth. Colors appear only when the client's
    // segmentation reproduces it exactly.
    const colorable = restored !== null && segments !== null && segmentsText(segments) === restored.text
    const canRestore = !restoring && reply.trim() !== '' && (doc !== null || mappingFile !== null)

    return (
      <div className="stack">
        <div className="viewbar">
          {jobs}
          <span className="meta">
            {doc ? `using this session's mapping · ${doc.name}`
              : mappingFile ? `using ${mappingFile.name}` : 'no document open'}
          </span>
          <div className="right">
            <button className="ghost" disabled={restored === null} onClick={copyRestored}>Copy restored text</button>
            <button className="primary" disabled={!canRestore} onClick={() => void runRestore()}>
              {restoring ? 'Restoring' : 'Restore'}
            </button>
          </div>
        </div>

        {!doc && !mappingFile && (
          <>
            {mappingProblem && (
              <div className="error-note" role="alert">
                <strong>That file is not a mapping</strong>
                <p>Use the .mapping.json file the export saved.</p>
              </div>
            )}
            <label
              className="drop"
              onDragOver={event => event.preventDefault()}
              onDrop={event => {
                event.preventDefault()
                void openMapping(event.dataTransfer.files?.[0] ?? null)
              }}
            >
              <b>Drop the mapping file here</b>
              <span className="sub">the .mapping.json next to your redacted copy · never leaves this machine</span>
              <input
                type="file"
                accept=".json"
                hidden
                onChange={event => {
                  void openMapping(event.target.files?.[0] ?? null)
                  event.target.value = ''
                }}
              />
            </label>
          </>
        )}

        <div className="restore-cols">
          <section className="restore-col">
            <div className="restore-col-head"><h2>The cloud&apos;s reply</h2><span className="hint">Paste it here</span></div>
            <textarea
              className="restore-in"
              value={reply}
              placeholder="Paste the reply that quotes [PERSON_1]-style pseudonyms."
              onChange={event => { setReply(event.target.value); setRestored(null); restoreSeq.current++ }}
            />
          </section>
          <section className="restore-col">
            <div className="restore-col-head">
              <h2>What it really says</h2>
              {restored !== null && <span className="hint">{restoredHint(restored.tokens)}</span>}
            </div>
            {restored !== null && restored.unknown.length > 0 && (
              <div className="passline"><span className="claims">{strayLine(restored.unknown.length)}</span></div>
            )}
            {restored === null
              ? <div className="restore-out faint">Restored text stays on this screen. It is never saved to a file.</div>
              : colorable
                ? <div className="restore-out">{segments.map((segment, index) =>
                    segment.kind === 'restored' ? <span key={index} className="back">{segment.text}</span>
                    : segment.kind === 'stray' ? <span key={index} className="stray">{segment.text}</span>
                    : <Fragment key={index}>{segment.text}</Fragment>)}</div>
                : <div className="restore-out">{restored.text}</div>}
          </section>
        </div>
      </div>
    )
  }

  if (!doc) {
    return (
      <div className="stack">
        <div className="viewbar">
          {jobs}
          <span className="meta">no document open</span>
        </div>
        <label
          className="drop"
          onDragOver={event => event.preventDefault()}
          onDrop={event => {
            event.preventDefault()
            openFrom(event.dataTransfer.files)
          }}
        >
          <b>Drop a document here</b>
          <span className="sub">{ACCEPT_HINT}</span>
          {picker}
        </label>
      </div>
    )
  }

  return (
    <div className="stack">
      {picker}
      <div className="viewbar">
        {jobs}
        <span className="name">{doc.name}</span>
        <span className="meta">{documentMeta(doc.words, findings.length)}</span>
        <div className="right">
          <button className="ghost" disabled={busy || asking} onClick={() => fileInput.current?.click()}>Open file</button>
          <button className="ghost" disabled={busy || asking} onClick={() => void askModel()}>
            {asking ? 'Asking the model' : 'Ask the model'}
          </button>
          <button className="primary" onClick={exportSafeCopy}>Export safe copy</button>
        </div>
      </div>

      {passProblem && (
        <div className="error-note" role="alert">
          <strong>Could not ask the model</strong>
          <p>{passProblem}</p>
          <p className="faint">Start a text model on the Models tab.</p>
        </div>
      )}

      <div className="cols">
        <section className="findings">
          <div className="findings-head"><h2>Findings</h2></div>
          {asking && (
            <div className="passline working"><span className="dot" />asking the model</div>
          )}
          {!asking && pass && pass.degraded && (
            <div className="passline degraded">
              <span className="dot" />the model&apos;s answers were unusable; findings are from patterns only
            </div>
          )}
          {!asking && pass && !pass.degraded && (() => {
            const receipt = passReceipt(pass, passBefore)
            return (
              <div className="passline">
                <span>asked <span className="who">{receipt.model}</span>: {receipt.findings}{receipt.claims !== '' && <span className="claims">, {receipt.claims}</span>}{receipt.parts !== '' && <span className="claims">, {receipt.parts}</span>}</span>
              </div>
            )
          })()}
          <div className="findings-body">
            {groupFindings(findings).map(group => (
              <Fragment key={group.heading}>
                <div className="cat">{group.heading}</div>
                {group.findings.map(finding => (
                  <div key={finding.id} className={finding.enabled ? 'frow' : 'frow off'}>
                    <button
                      className={finding.enabled ? 'tgl' : 'tgl off'}
                      aria-label={finding.literal}
                      aria-pressed={finding.enabled}
                      onClick={() => toggle(finding)}
                    />
                    <span className="lit">{finding.literal}</span>
                    <span className="n">{finding.occurrences}x</span>
                    <span className="pseud">{finding.token}</span>
                    <span className={sourceClass(finding.source)}>{sourceLabel(finding.source)}</span>
                  </div>
                ))}
              </Fragment>
            ))}
          </div>
        </section>

        <section className="preview" ref={previewPane} onMouseUp={readSelection}>
          <div className="preview-head"><h2>Preview</h2><span className="hint">Select text to hide it</span></div>
          <div className="doc">
            {previewParagraphs(preview).map((paragraph, index) => (
              <p key={index}>
                {previewSegments(paragraph, findings).map((segment, position) =>
                  segment.kind === 'token'
                    ? <span key={position} className="rep">{segment.text}</span>
                    : <Fragment key={position}>{segment.text}</Fragment>)}
              </p>
            ))}
          </div>
          {offer && (
            <span className="addtip" style={{ top: offer.top, left: offer.left }}>
              <button onMouseDown={event => event.preventDefault()} onClick={() => hide(offer.literal)}>Hide</button>
            </span>
          )}
        </section>
      </div>
    </div>
  )
}
