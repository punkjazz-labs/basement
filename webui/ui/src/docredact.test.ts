import { describe, expect, it } from 'vitest'
import type { DocredactFinding, DocredactModelPass } from './api'
import {
  ACCEPT_ATTRIBUTE, ACCEPT_HINT, acceptedDocument, documentMeta, exportNames, findingByID,
  groupFindings, passReceipt, pickDocument, previewParagraphs, previewSegments, selectionLiteral,
  sourceClass, sourceLabel, toggledFindings, wordCount,
} from './docredact'

const finding = (overrides: Partial<DocredactFinding> = {}): DocredactFinding => ({
  id: 'EMAIL_1',
  token: '[EMAIL_1]',
  literal: 'jane.doe@example.com',
  category: 'email',
  source: 'pattern',
  occurrences: 2,
  enabled: true,
  ...overrides,
})

describe('opening a document', () => {
  it('accepts only what this build can read', () => {
    expect(acceptedDocument('notes.txt')).toBe(true)
    expect(acceptedDocument('agreement.MD')).toBe(true)
    expect(acceptedDocument('/tmp/deal.md')).toBe(true)
    expect(acceptedDocument('scan.pdf')).toBe(false)
    expect(acceptedDocument('contract.docx')).toBe(false)
    expect(acceptedDocument('README')).toBe(false)
    expect(acceptedDocument('.txt')).toBe(false)
  })

  it('offers the same list to the file picker and to the drop zone', () => {
    expect(ACCEPT_ATTRIBUTE).toBe('.txt,.md')
    expect(ACCEPT_HINT).toBe('.txt .md')
  })

  it('takes the first readable file out of a drop', () => {
    expect(pickDocument([{ name: 'photo.png' }, { name: 'deal.md' }, { name: 'notes.txt' }]))
      .toEqual({ name: 'deal.md' })
    expect(pickDocument([{ name: 'photo.png' }])).toBeNull()
    expect(pickDocument([])).toBeNull()
  })
})

describe('measuring the document', () => {
  it('counts runs of non-space as words', () => {
    expect(wordCount('')).toBe(0)
    expect(wordCount('   \n ')).toBe(0)
    expect(wordCount('one')).toBe(1)
    expect(wordCount(' two  words\nover lines ')).toBe(4)
  })

  it('writes the viewbar line the way the mockup reads', () => {
    expect(documentMeta(4210, 11)).toBe('4,210 words · 11 findings')
    expect(documentMeta(1, 1)).toBe('1 word · 1 finding')
    expect(documentMeta(0, 0)).toBe('0 words · 0 findings')
  })
})

describe('grouping findings', () => {
  it('files every category under its heading, in reading order', () => {
    const groups = groupFindings([
      finding(),
      finding({ id: 'IBAN_1', token: '[IBAN_1]', category: 'iban' }),
      finding({ id: 'PHRASE_1', token: '[PHRASE_1]', category: 'phrase', source: 'manual' }),
      finding({ id: 'DOB_1', token: '[DOB_1]', category: 'dob' }),
      finding({ id: 'PHONE_1', token: '[PHONE_1]', category: 'phone' }),
    ])
    expect(groups.map(group => group.heading)).toEqual([
      'People and companies', 'Contact', 'Money and accounts', 'Dates and IDs',
    ])
    expect(groups[1].findings.map(item => item.id)).toEqual(['EMAIL_1', 'PHONE_1'])
  })

  it('leaves out a heading nothing was found under', () => {
    expect(groupFindings([finding()]).map(group => group.heading)).toEqual(['Contact'])
    expect(groupFindings([])).toEqual([])
  })

  it('still shows a finding whose category this build does not know', () => {
    const groups = groupFindings([finding({ id: 'ROLE_1', category: 'role' })])
    expect(groups).toEqual([{ heading: 'Dates and IDs', findings: [finding({ id: 'ROLE_1', category: 'role' })] }])
  })
})

describe('groupFindings with model categories', () => {
  const finding = (category: string, source = 'pattern', literal = 'x'): DocredactFinding =>
    ({ id: literal, token: '[X_1]', literal, category, source, occurrences: 1, enabled: true })

  it('files person, org and job_title under People and companies', () => {
    const groups = groupFindings([finding('person'), finding('org'), finding('job_title')])
    expect(groups).toHaveLength(1)
    expect(groups[0].heading).toBe('People and companies')
    expect(groups[0].findings).toHaveLength(3)
  })

  it('files address under Contact and amount under Money and accounts', () => {
    const groups = groupFindings([finding('address'), finding('amount')])
    expect(groups.map(group => group.heading)).toEqual(['Contact', 'Money and accounts'])
  })

  it('files every national identifier under Dates and IDs', () => {
    const ids = ['fr_nir', 'es_dni', 'pt_nif', 'de_steuer_id', 'nl_bsn', 'uk_nino', 'br_cpf', 'cn_resident_id', 'jp_my_number']
    const groups = groupFindings(ids.map(category => finding(category, 'pattern', category)))
    expect(groups).toHaveLength(1)
    expect(groups[0].heading).toBe('Dates and IDs')
    expect(groups[0].findings).toHaveLength(ids.length)
  })

  it('still files a category nobody named under the last heading', () => {
    const groups = groupFindings([finding('never_heard_of_it')])
    expect(groups).toHaveLength(1)
    expect(groups[0].heading).toBe('Dates and IDs')
  })

  it('sorts model rows after pattern and manual rows inside a group, stably', () => {
    const groups = groupFindings([
      finding('person', 'model', 'model-first'),
      finding('person', 'pattern', 'rule-a'),
      finding('person', 'model', 'model-second'),
      finding('person', 'manual', 'by-hand'),
    ])
    expect(groups[0].findings.map(item => item.literal))
      .toEqual(['rule-a', 'by-hand', 'model-first', 'model-second'])
  })
})

describe('passReceipt', () => {
  const pass = (accepted: number, hallucinated: number): DocredactModelPass =>
    ({ accepted, duplicates: 0, hallucinated, chunks_total: 1, chunks_failed: 0, degraded: false, model: 'Qwen 3.6 27B' })

  it('counts new findings in words', () => {
    expect(passReceipt(pass(4, 0))).toEqual({ model: 'Qwen 3.6 27B', findings: '4 new findings', claims: '' })
    expect(passReceipt(pass(1, 0)).findings).toBe('1 new finding')
    expect(passReceipt(pass(0, 0)).findings).toBe('no new findings')
  })

  it('names claims only when the model made one that was not in the document', () => {
    expect(passReceipt(pass(2, 1)).claims).toBe('1 claim not in the document')
    expect(passReceipt(pass(2, 3)).claims).toBe('3 claims not in the document')
  })
})

describe('the source pill', () => {
  it('calls the manager pattern pass what the console calls it', () => {
    expect(sourceLabel('pattern')).toBe('rule')
    expect(sourceClass('pattern')).toBe('pill')
  })

  it('shows any other source exactly as it arrived', () => {
    expect(sourceLabel('manual')).toBe('manual')
    expect(sourceClass('manual')).toBe('pill manual')
    expect(sourceLabel('model')).toBe('model')
    expect(sourceClass('model')).toBe('pill model')
    expect(sourceLabel('something-new')).toBe('something-new')
    expect(sourceClass('something-new')).toBe('pill')
    expect(sourceLabel('')).toBe('n/a')
  })
})

describe('toggling', () => {
  it('flips one finding and leaves the rest alone', () => {
    const findings = [finding(), finding({ id: 'PHONE_1', category: 'phone' })]
    const next = toggledFindings(findings, 'EMAIL_1')
    expect(next[0].enabled).toBe(false)
    expect(next[1].enabled).toBe(true)
    expect(findings[0].enabled).toBe(true)
    expect(toggledFindings(next, 'EMAIL_1')[0].enabled).toBe(true)
  })

  it('does nothing for an id that is not there', () => {
    const findings = [finding()]
    expect(toggledFindings(findings, 'NOPE_1')).toEqual(findings)
    expect(findingByID(findings, 'EMAIL_1')?.literal).toBe('jane.doe@example.com')
    expect(findingByID(findings, 'NOPE_1')).toBeUndefined()
  })
})

describe('the preview', () => {
  it('splits on blank lines and drops the empties', () => {
    expect(previewParagraphs('one\n\ntwo\n\n\n\nthree')).toEqual(['one', 'two', 'three'])
    expect(previewParagraphs('a single line')).toEqual(['a single line'])
    expect(previewParagraphs('  ')).toEqual([])
  })

  it('turns a finding pseudonym into a chip and leaves lookalikes as text', () => {
    expect(previewSegments('Write to [EMAIL_1] today.', [finding()])).toEqual([
      { kind: 'text', text: 'Write to ' },
      { kind: 'token', text: '[EMAIL_1]' },
      { kind: 'text', text: ' today.' },
    ])
    // [EMAIL_2] belongs to no finding, so it is whatever the document said.
    expect(previewSegments('[EMAIL_2] wrote', [finding()])).toEqual([
      { kind: 'text', text: '[EMAIL_2] wrote' },
    ])
    expect(previewSegments('nothing hidden here', [finding()])).toEqual([
      { kind: 'text', text: 'nothing hidden here' },
    ])
  })

  it('handles a paragraph that is only a pseudonym', () => {
    expect(previewSegments('[EMAIL_1]', [finding()])).toEqual([{ kind: 'token', text: '[EMAIL_1]' }])
  })
})

describe('selecting text to hide it', () => {
  it('takes the selection without the whitespace a drag picks up', () => {
    expect(selectionLiteral('  the Basel laboratory incident ')).toBe('the Basel laboratory incident')
    expect(selectionLiteral('Marta')).toBe('Marta')
  })

  it('refuses a selection that cannot be found in the document', () => {
    expect(selectionLiteral('')).toBe('')
    expect(selectionLiteral('   ')).toBe('')
    // Already hidden: the pseudonym is not in the original text.
    expect(selectionLiteral('paid to [IBAN_1] in two parts')).toBe('')
    expect(selectionLiteral('[EMAIL_1]')).toBe('')
    // Across a line: the halves are not adjacent in the source.
    expect(selectionLiteral('first line\nsecond line')).toBe('')
  })
})

describe('naming the downloads', () => {
  it('derives both names from the file that was opened', () => {
    expect(exportNames('severance-agreement.md')).toEqual({
      redacted: 'severance-agreement.redacted.md',
      mapping: 'severance-agreement.mapping.json',
    })
    expect(exportNames('letter.txt')).toEqual({
      redacted: 'letter.redacted.txt',
      mapping: 'letter.mapping.json',
    })
  })

  it('treats anything that is not markdown as plain text', () => {
    expect(exportNames('notes').redacted).toBe('notes.redacted.txt')
    expect(exportNames('report.rtf').redacted).toBe('report.redacted.txt')
    expect(exportNames('/home/notes/deal.md').redacted).toBe('deal.redacted.md')
  })

  it('falls back to a name when there is nothing to derive one from', () => {
    expect(exportNames('')).toEqual({ redacted: 'document.redacted.txt', mapping: 'document.mapping.json' })
    expect(exportNames('   ')).toEqual({ redacted: 'document.redacted.txt', mapping: 'document.mapping.json' })
  })
})
