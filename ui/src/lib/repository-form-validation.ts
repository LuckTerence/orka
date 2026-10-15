const GITHUB_PATH_SEGMENT = /^[A-Za-z0-9._-]+(?:\.git)?$/

/**
 * Accepts only a credential-free GitHub repository root: `https://github.com/org/repo`
 * (optionally `.git`) or `git@github.com:org/repo`. Pull request, issue, branch,
 * file, query, fragment, plain-HTTP, other-host, and token-bearing URLs are rejected.
 */
export function isCredentialFreeGitHubRepositoryURL(value: string) {
  const repoURL = value.trim()
  if (!repoURL) return false

  if (/^git@github\.com:[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+(?:\.git)?$/.test(repoURL)) {
    return true
  }

  if (!repoURL.startsWith('https://')) {
    return false
  }

  try {
    const parsed = new URL(repoURL)
    if (parsed.protocol !== 'https:' || parsed.hostname !== 'github.com') return false
    if (parsed.username || parsed.password || parsed.search || parsed.hash) return false

    const pathParts = parsed.pathname.split('/').filter(Boolean)
    if (pathParts.length !== 2) return false

    return pathParts.every((part) => GITHUB_PATH_SEGMENT.test(part))
  } catch {
    return false
  }
}

const CRON_DESCRIPTOR = /^@(?:yearly|annually|monthly|weekly|daily|midnight|hourly|every\s+\S+)$/
const CRON_FIELD = /^[A-Za-z0-9*/,?-]+$/

/**
 * Basic shape check for a cron schedule: five space-separated fields
 * (minute hour day-of-month month day-of-week) or one of the @-descriptors the
 * controller's standard cron parser also accepts, optionally after a
 * `CRON_TZ=`/`TZ=` prefix. Field values stay the server's call.
 */
export function isCronScheduleShape(value: string) {
  const schedule = value.trim().replace(/^(?:CRON_)?TZ=\S+\s+/, '')
  if (CRON_DESCRIPTOR.test(schedule)) return true
  const fields = schedule.split(/\s+/)
  return fields.length === 5 && fields.every((field) => CRON_FIELD.test(field))
}
