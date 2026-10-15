import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import type { Task } from '@/schemas/task'

const terminalPhases = new Set(['Succeeded', 'Failed', 'Cancelled'])

// Derived from the Task itself so list rows never need a per-row approvals
// request. The controller declares a WaitingForApproval condition, and an
// autonomous task parked on an approval reports "waiting for approval ..." as
// its status message.
export function isWaitingForApproval(task: Task): boolean {
  if (terminalPhases.has(task.status?.phase ?? '')) return false
  const condition = task.status?.conditions?.find((c) => c.type === 'WaitingForApproval')
  if (condition) return condition.status === 'True'
  return task.status?.message?.startsWith('waiting for approval ') ?? false
}

/** A delete was accepted and finalizers are still running. */
export function isTaskDeleting(task: Task): boolean {
  return Boolean(task.metadata.deletionTimestamp)
}

/** A scheduled (cron) Task whose future runs are suspended. */
export function isScheduleSuspended(task: Task): boolean {
  return Boolean(task.spec.schedule) && task.spec.suspend === true
}

/**
 * Compact state flags shown next to a task's phase wherever tasks are
 * listed: Deleting, Waiting for approval, and Suspended (schedule).
 */
export function TaskStateBadges({ task, className }: { task: Task; className?: string }) {
  const deleting = isTaskDeleting(task)
  const waiting = isWaitingForApproval(task)
  const suspended = isScheduleSuspended(task)
  if (!deleting && !waiting && !suspended) return null
  return (
    <span className={cn('inline-flex flex-wrap items-center gap-1', className)}>
      {deleting && (
        <Badge variant="outline" className="text-status-failed">Deleting</Badge>
      )}
      {waiting && (
        <Badge variant="outline" className="gap-1.5 text-status-running">
          <span aria-hidden="true" className="inline-block size-1.5 rounded-full bg-current motion-safe:animate-pulse-live" />
          Waiting for approval
        </Badge>
      )}
      {suspended && (
        <Badge variant="outline" className="text-muted-foreground">Suspended</Badge>
      )}
    </span>
  )
}
