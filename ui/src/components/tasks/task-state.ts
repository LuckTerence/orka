import type { Task } from '@/schemas/task'

const terminalPhases = new Set(['Succeeded', 'Failed', 'Cancelled'])

// Derived from the Task itself so list rows never need a per-row approvals
// request. The controller declares a WaitingForApproval condition, and an
// autonomous task parked on an approval reports "waiting for approval ..." as
// its status message.
export function isWaitingForApproval(task: Task): boolean {
  if (terminalPhases.has(task.status?.phase ?? '')) return false
  const condition = task.status?.conditions?.find((c) => c.type === 'WaitingForApproval')
  return condition?.status === 'True' || (task.status?.message?.startsWith('waiting for approval ') ?? false)
}

/** A delete was accepted and finalizers are still running. */
export function isTaskDeleting(task: Task): boolean {
  return Boolean(task.metadata.deletionTimestamp)
}

/** A scheduled (cron) Task whose future runs are suspended. */
export function isScheduleSuspended(task: Task): boolean {
  return Boolean(task.spec.schedule) && task.spec.suspend === true
}
