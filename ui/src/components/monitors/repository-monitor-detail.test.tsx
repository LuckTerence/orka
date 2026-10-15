import { describe, it, expect, beforeEach, vi } from 'vitest'

const mockMutateAsync = vi.fn()
const mockCommandMutate = vi.fn()

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@/hooks/use-monitors', () => {
  const empty = () => ({ data: { items: [] } })
  return {
    useRepositoryMonitor: () => ({
      isLoading: false,
      data: {
        metadata: { name: 'repo-monitor', namespace: 'default' },
        spec: { repoURL: 'https://github.com/orka-agents/orka' },
        status: { phase: 'Ready' },
      },
    }),
    useRepositoryMonitorRuns: empty,
    useRepositoryMonitorItems: (_name: string, kind?: string) =>
      kind === 'issue' ? { data: { items: [{ itemKey: 'issue-7', number: 7, title: 'Flaky test' }] } } : empty(),
    useRepositoryMonitorActions: empty,
    useRepositoryMonitorCommands: empty,
    useRepositoryMonitorWorkActions: empty,
    useRepositoryMonitorImplementationJobs: empty,
    useRepositoryMonitorMutations: empty,
    useRunRepositoryMonitor: () => ({ mutateAsync: mockMutateAsync, isPending: false }),
    useCreateRepositoryMonitorCommand: () => ({ mutate: mockCommandMutate, isPending: false }),
  }
})

import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { render, screen, waitFor } from '@/test/test-utils'
import { ApiError } from '@/lib/api-client'
import { RepositoryMonitorDetail } from './repository-monitor-detail'

describe('RepositoryMonitorDetail', () => {
  beforeEach(() => {
    mockMutateAsync.mockReset()
    mockCommandMutate.mockReset()
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  it('confirms a started run', async () => {
    mockMutateAsync.mockResolvedValue({})
    const user = userEvent.setup()
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    await user.click(screen.getByRole('button', { name: 'Run' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Monitor run started'))
  })

  it('reports a refused run with the server message', async () => {
    mockMutateAsync.mockRejectedValue(new ApiError(403, '{"error":{"code":403,"message":"not authorized to run monitors"}}'))
    const user = userEvent.setup()
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    await user.click(screen.getByRole('button', { name: 'Run' }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to start monitor run: not authorized to run monitors'))
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('confirms a queued item command', async () => {
    mockCommandMutate.mockImplementation((_command, options) => options.onSuccess())
    const user = userEvent.setup()
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    await user.click(screen.getByRole('button', { name: 'Triage' }))

    expect(mockCommandMutate).toHaveBeenCalledWith({ kind: 'issue', number: 7, intent: 'triage' }, expect.any(Object))
    expect(toast.success).toHaveBeenCalledWith('Queued triage for #7')
  })

  it('reports a refused item command with the server message', async () => {
    mockCommandMutate.mockImplementation((_command, options) =>
      options.onError(new Error('not authorized to command monitors')))
    const user = userEvent.setup()
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    await user.click(screen.getByRole('button', { name: 'Research' }))

    expect(toast.error).toHaveBeenCalledWith('Failed to queue research: not authorized to command monitors')
  })
})
