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

interface CronField {
  min: number
  max: number
  /** Case-insensitive names; the first one stands for `min`. */
  names?: readonly string[]
}

// Bounds and names of the controller's standard cron parser (robfig/cron
// ParseStandard), which rejects anything outside them, including weekday 7.
const CRON_FIELDS: readonly CronField[] = [
  { min: 0, max: 59 }, // minute
  { min: 0, max: 23 }, // hour
  { min: 1, max: 31 }, // day of month
  { min: 1, max: 12, names: ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'] },
  { min: 0, max: 6, names: ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'] }, // day of week
]

function cronValue(token: string, field: CronField) {
  if (/^\d+$/.test(token)) return Number(token)
  const index = field.names?.indexOf(token.toLowerCase()) ?? -1
  return index < 0 ? undefined : field.min + index
}

/** One comma-separated item: `*` or `?`, a value, or a range, each optionally `/step`. */
function isCronFieldItem(item: string, field: CronField) {
  const [range, step, ...extraSteps] = item.split('/')
  if (extraSteps.length > 0) return false
  if (step !== undefined && !(/^\d+$/.test(step) && Number(step) > 0)) return false
  if (range === '*' || range === '?') return true
  const [low, high, ...extraBounds] = range.split('-')
  if (extraBounds.length > 0) return false
  const start = cronValue(low, field)
  const end = high === undefined ? start : cronValue(high, field)
  return start !== undefined && end !== undefined && field.min <= start && start <= end && end <= field.max
}

/**
 * Checks a cron schedule the way the controller parses it: five
 * space-separated fields (minute 0-59, hour 0-23, day of month 1-31, month
 * 1-12 or JAN-DEC, day of week 0-6 or SUN-SAT) built from `*`, `?`, values,
 * ranges, steps, and lists, or one of the @-descriptors, optionally after a
 * `CRON_TZ=`/`TZ=` prefix. Empty list items are refused as typos. The time
 * zone and @every duration stay the server's call.
 */
export function isCronSchedule(value: string) {
  const schedule = value.trim().replace(/^(?:CRON_)?TZ=\S+\s+/, '')
  if (CRON_DESCRIPTOR.test(schedule)) return true
  const fields = schedule.split(/\s+/)
  return fields.length === CRON_FIELDS.length &&
    fields.every((field, index) => field.split(',').every((item) => isCronFieldItem(item, CRON_FIELDS[index])))
}
