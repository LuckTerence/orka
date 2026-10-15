import { describe, it, expect } from 'vitest'
import { isCredentialFreeGitHubRepositoryURL, isCronScheduleShape } from './repository-form-validation'

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

describe('isCronScheduleShape', () => {
  it.each([
    '0 */6 * * *',
    '*/30 * * * *',
    '0 9 * * MON-FRI',
    ' 15 2 1,15 * ? ',
    '@daily',
    '@every 1h30m',
    'CRON_TZ=Europe/Berlin 0 6 * * *',
  ])('accepts %s', (schedule) => {
    expect(isCronScheduleShape(schedule)).toBe(true)
  })

  it.each([
    '',
    'every hour',
    '0 */6 * *',
    '0 */6 * * * *',
    '@sometimes',
    '0 6 * * * ; rm',
  ])('rejects %s', (schedule) => {
    expect(isCronScheduleShape(schedule)).toBe(false)
  })
})
