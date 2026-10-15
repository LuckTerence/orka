import { describe, it, expect, beforeEach, vi } from 'vitest'

const mockMutateAsync = vi.fn()

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
    useRepositoryMonitorItems: empty,
    useRepositoryMonitorActions: empty,
    useRepositoryMonitorCommands: empty,
    useRepositoryMonitorWorkActions: empty,
    useRepositoryMonitorImplementationJobs: empty,
    useRepositoryMonitorMutations: empty,
    useRunRepositoryMonitor: () => ({ mutateAsync: mockMutateAsync, isPending: false }),
    useCreateRepositoryMonitorCommand: () => ({ mutate: vi.fn(), isPending: false }),
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
})
