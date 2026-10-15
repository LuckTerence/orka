import { describe, it, expect } from 'vitest'
import { isCredentialFreeGitHubRepositoryURL, isCronSchedule } from './repository-form-validation'

describe('isCredentialFreeGitHubRepositoryURL', () => {
  it.each([
    'https://github.com/org/repo',
    'https://github.com/org/repo.git',
    '  https://github.com/org/repo  ',
    'git@github.com:org/repo',
    'git@github.com:org/repo.git',
  ])('accepts %s', (url) => {
    expect(isCredentialFreeGitHubRepositoryURL(url)).toBe(true)
  })

  it.each([
    '',
    'not a url',
    'http://github.com/org/repo',
    'https://gitlab.com/org/repo',
    'https://token@github.com/org/repo',
    'https://user:pass@github.com/org/repo',
    'https://github.com/org',
    'https://github.com/org/repo/pull/1',
    'https://github.com/org/repo/tree/main',
    'https://github.com/org/repo?tab=readme',
    'https://github.com/org/repo#readme',
  ])('rejects %s', (url) => {
    expect(isCredentialFreeGitHubRepositoryURL(url)).toBe(false)
  })
})

describe('isCronSchedule', () => {
  it.each([
    '0 */6 * * *',
    '*/30 * * * *',
    '0 9 * * MON-FRI',
    ' 15 2 1,15 * ? ',
    '59 23 31 12 6',
    '0 0 1 1 0',
    '0-30/10 8-18 1-7,15 jan-JUN sun,Sat',
    '5/15 * * * *',
    '0 6 * DEC *',
    '@daily',
    '@every 1h30m',
    'CRON_TZ=Europe/Berlin 0 6 * * *',
    'TZ=UTC 0 6 * * *',
  ])('accepts %s', (schedule) => {
    expect(isCronSchedule(schedule)).toBe(true)
  })

  it.each([
    '',
    'every hour',
    '0 */6 * *',
    '0 */6 * * * *',
    '@sometimes',
    '0 6 * * * ; rm',
    '60 * * * *',
    '0 24 * * *',
    '0 0 0 * *',
    '0 0 32 * *',
    '0 0 * 0 *',
    '0 0 * 13 *',
    // robfig/cron's standard parser has no weekday 7.
    '0 0 * * 7',
    '0 0 * * 1-7',
    '30-10 * * * *',
    '*/0 * * * *',
    '1/2/3 * * * *',
    '1-2-3 * * * *',
    // robfig/cron skips empty list items; an empty item is a typo here.
    '1,,2 * * * *',
    '-1 * * * *',
    '0 MON * * *',
    '0 0 * * JAN',
    '0 0 * * monday',
  ])('rejects %s', (schedule) => {
    expect(isCronSchedule(schedule)).toBe(false)
  })
})
