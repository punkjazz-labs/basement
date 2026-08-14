import type { DocredactFinding, DocredactModelPass } from './api'

// The console half of the document redactor. Everything here is pure: which
// heading a finding is filed under, how the document is measured, what a
// selection in the preview means, and what the two downloads are called.
// Redactor.tsx owns the requests, the file input and the selection events.

// v1 reads what the browser can hand over as text. PDF and Word extraction
// are not built, so they are not offered: an accept list is a promise.
export const ACCEPTED_EXTENSIONS = ['.txt', '.md'] as const
export const ACCEPT_ATTRIBUTE = ACCEPTED_EXTENSIONS.join(',')
export const ACCEPT_HINT = ACCEPTED_EXTENSIONS.join(' ')

const extensionOf = (name: string): string => {
  const base = name.trim().split(/[\\/]/).pop() ?? ''
  const dot = base.lastIndexOf('.')
  return dot <= 0 ? '' : base.slice(dot).toLowerCase()
}

export const acceptedDocument = (name: string): boolean =>
  (ACCEPTED_EXTENSIONS as readonly string[]).includes(extensionOf(name))

// The first file in a drop or a picker this build can actually read. A drop
// carrying nothing readable is not an error to explain, it is simply nothing
// to open.
export function pickDocument<T extends { name: string }>(files: readonly T[]): T | null {
  return files.find(file => acceptedDocument(file.name)) ?? null
}

// ---- What the viewbar says --------------------------------------------------

// Words as a person counts them: runs of non-space, nothing else. Markdown
// punctuation is left in, because a heading marker is a word to the model
// that will read this file.
export const wordCount = (text: string): number => {
  const trimmed = text.trim()
  return trimmed === '' ? 0 : trimmed.split(/\s+/).length
}

const counted = (value: number, one: string, many: string): string =>
  `${value.toLocaleString('en-US')} ${value === 1 ? one : many}`

// The line beside the file name. Findings are counted whole, switched off
// ones included: they are still things the redactor found.
export const documentMeta = (words: number, findings: number): string =>
  `${counted(words, 'word', 'words')} · ${counted(findings, 'finding', 'findings')}`

// ---- Findings, grouped for reading ------------------------------------------

// The headings, in the order they are shown. Categories are the manager's own
// names (internal/docredact), so this list is the one place a new detector
// has to be filed. A category no group names is filed under the last heading
// rather than dropped: a finding a person cannot see is a finding they cannot
// switch off.
const GROUPS: Array<{ heading: string; categories: string[] }> = [
  { heading: 'People and companies', categories: ['phrase', 'person', 'org', 'job_title'] },
  { heading: 'Contact', categories: ['email', 'phone', 'address'] },
  { heading: 'Money and accounts', categories: ['iban', 'card', 'amount'] },
  {
    heading: 'Dates and IDs',
    categories: [
      'dob', 'it_codice_fiscale', 'us_ssn', 'ipv4', 'ipv6',
      'fr_nir', 'es_dni', 'pt_nif', 'de_steuer_id', 'nl_bsn',
      'uk_nino', 'br_cpf', 'cn_resident_id', 'jp_my_number',
    ],
  },
]

export interface FindingGroup {
  heading: string
  findings: DocredactFinding[]
}

// Inside a group, whatever a rule or the owner found comes before what the
// model claimed: a rule row is exact by construction, so it is the first
// thing read. The sort is stable, so rows keep the document order they
// arrived in otherwise.
export const orderFindings = (findings: DocredactFinding[]): DocredactFinding[] =>
  [...findings.filter(finding => finding.source !== 'model'),
   ...findings.filter(finding => finding.source === 'model')]

export function groupFindings(findings: DocredactFinding[]): FindingGroup[] {
  const known = new Set(GROUPS.flatMap(group => group.categories))
  const last = GROUPS.length - 1
  return GROUPS.map((group, index) => ({
    heading: group.heading,
    findings: orderFindings(findings.filter(finding =>
      group.categories.includes(finding.category) ||
      (index === last && !known.has(finding.category)))),
  })).filter(group => group.findings.length > 0)
}

// The pill on a row. "pattern" is what the manager calls its own detectors;
// on screen those are rules. Anything else is shown exactly as it arrived,
// including a source this build has never heard of.
export const sourceLabel = (source: string): string =>
  source === 'pattern' ? 'rule' : source || 'n/a'

export const sourceClass = (source: string): string => {
  const label = sourceLabel(source)
  return label === 'model' || label === 'manual' ? `pill ${label}` : 'pill'
}

// Switching a finding off in the list before the manager has answered. The
// answer replaces the whole list, so this only has to be right for the moment
// in between.
export const toggledFindings = (findings: DocredactFinding[], id: string): DocredactFinding[] =>
  findings.map(finding => (finding.id === id ? { ...finding, enabled: !finding.enabled } : finding))

export const findingByID = (findings: DocredactFinding[], id: string): DocredactFinding | undefined =>
  findings.find(finding => finding.id === id)

// ---- The preview ------------------------------------------------------------

// A pseudonym as it appears in the redacted text, e.g. [EMAIL_1]. Its Go
// twin is tokenShape in internal/docredact/restore.go; the two must stay
// in lockstep.
const TOKEN = /\[[A-Z][A-Z0-9]*_\d+\]/g

export type PreviewSegment =
  | { kind: 'text'; text: string }
  | { kind: 'token'; text: string }

export const previewParagraphs = (text: string): string[] =>
  text.split(/\n{2,}/).filter(paragraph => paragraph.trim() !== '')

// One paragraph split into what a person typed and what now stands in for
// something. Only a token that belongs to a finding becomes a chip: text that
// merely looks like one is text.
export function previewSegments(paragraph: string, findings: DocredactFinding[]): PreviewSegment[] {
  const tokens = new Set(findings.map(finding => finding.token))
  const segments: PreviewSegment[] = []
  let last = 0
  for (const match of paragraph.matchAll(TOKEN)) {
    const index = match.index ?? 0
    if (!tokens.has(match[0])) continue
    if (index > last) segments.push({ kind: 'text', text: paragraph.slice(last, index) })
    segments.push({ kind: 'token', text: match[0] })
    last = index + match[0].length
  }
  if (last < paragraph.length) segments.push({ kind: 'text', text: paragraph.slice(last) })
  return segments
}

// What a selection in the preview can be asked to hide. The preview is the
// redacted text, so a selection that includes a pseudonym does not exist in
// the original document at all and could never be found there; the same goes
// for one that runs across a line, since the two halves are not adjacent in
// the source the way they look on screen. Both are refused rather than sent
// and refused by the manager.
export function selectionLiteral(raw: string): string {
  const literal = raw.trim()
  if (literal === '') return ''
  if (/[\n\r]/.test(literal)) return ''
  if (new RegExp(TOKEN.source).test(literal)) return ''
  return literal
}

// ---- The two downloads ------------------------------------------------------

export interface ExportNames {
  redacted: string
  mapping: string
}

// The names the manager suggests, derived the same way it derives them so the
// console and the Content-Disposition header never disagree: the base name of
// the file that was opened, markdown keeping its extension and everything
// else treated as plain text.
export function exportNames(fileName: string): ExportNames {
  const base = fileName.trim().split(/[\\/]/).pop() ?? ''
  const extension = extensionOf(base)
  const name = (extension === '' ? base : base.slice(0, base.length - extension.length)).trim() || 'document'
  return {
    redacted: `${name}.redacted${extension === '.md' ? '.md' : '.txt'}`,
    mapping: `${name}.mapping.json`,
  }
}

// ---- The pass receipt -------------------------------------------------------

export interface PassReceipt {
  model: string
  findings: string
  claims: string
  parts: string
}

// The one line a finished pass leaves behind. Claims are the literals the
// model named that were not in the document: the engine already dropped
// them, so the receipt only has to say it happened, and only when it did.
//
// The server's `accepted` is every model-source finding in the document,
// cumulative across passes, not just this pass's contribution. What is new
// this pass is the growth since the last one, so the receipt subtracts the
// accepted count captured before this pass was asked.
export function passReceipt(pass: DocredactModelPass, previousAccepted = 0): PassReceipt {
  const gained = Math.max(0, pass.accepted - previousAccepted)
  return {
    model: pass.model,
    findings: gained === 0 ? 'no new findings' : counted(gained, 'new finding', 'new findings'),
    claims: pass.hallucinated === 0 ? '' : counted(pass.hallucinated, 'claim not in the document', 'claims not in the document'),
    // A pass that got no answer for some chunks read less of the document
    // than the receipt implies. The receipt must say so: on a redaction
    // tool, a silent partial read looks like a full read.
    parts: pass.chunks_failed === 0 ? '' :
      `the model did not answer ${pass.chunks_failed} of ${pass.chunks_total} parts`,
  }
}
