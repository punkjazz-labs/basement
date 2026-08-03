import { describe, expect, it } from 'vitest'
import { stepCopy, stepOperation } from './api'

describe('stepOperation', () => {
  it('reads a two-Spark step as the operation it is', () => {
    // A distributed job stores "download_artifact:head" and
    // "download_artifact:worker". Matching those literally is what left the
    // deployment dialog with no download detail on a two-Spark install.
    expect(stepOperation('download_artifact:head')).toBe('download_artifact')
    expect(stepOperation('download_artifact:worker')).toBe('download_artifact')
    expect(stepOperation('pull_image:worker')).toBe('pull_image')
    expect(stepOperation('rollback_stop_container:head')).toBe('stop_container')
  })

  it('leaves a single-Spark step alone', () => {
    expect(stepOperation('download_artifact')).toBe('download_artifact')
    expect(stepOperation('rollback_start_container')).toBe('start_container')
  })
})

describe('stepCopy', () => {
  it('names the Spark a two-Spark step ran on', () => {
    expect(stepCopy('download_artifact:worker')).toBe('Download model files (second Spark)')
    expect(stepCopy('download_artifact:head')).toBe('Download model files (this Spark)')
    expect(stepCopy('download_artifact')).toBe('Download model files')
  })
})
