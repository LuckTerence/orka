import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockMutateAsync = vi.fn()
const mockCommandMutate = vi.fn()
const mockUseRepositoryMonitor = vi.fn()

const readyMonitor = {
  metadata: { name: 'repo-monitor', namespace: 'default' },
  spec: { repoURL: 'https://github.com/orka-agents/orka' },
  status: { phase: 'Ready' },
}

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
  }
})

const mockDetailQuery = vi.fn()

vi.mock('@/hooks/use-monitors', () => {
  const empty = (name: string) => {
    mockDetailQuery(name)
    return { data: { items: [] } }
  }
  return {
    useRepositoryMonitor: () => mockUseRepositoryMonitor(),
    useRepositoryMonitorRuns: empty,
    useRepositoryMonitorItems: (name: string, kind?: string) =>
      kind === 'issue' ? { data: { items: [{ itemKey: 'issue-7', number: 7, title: 'Flaky test' }] } } : empty(name),
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
import { useUIStore } from '@/stores/ui'
import { RepositoryMonitorDetail } from './repository-monitor-detail'

describe('RepositoryMonitorDetail', () => {
  beforeEach(() => {
    mockMutateAsync.mockReset()
    mockCommandMutate.mockReset()
    mockDetailQuery.mockReset()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    mockUseRepositoryMonitor.mockReturnValue({ isLoading: false, data: readyMonitor })
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  it('alerts with the Ready=False reason and message of an errored monitor', () => {
    mockUseRepositoryMonitor.mockReturnValue({
      isLoading: false,
      data: {
        ...readyMonitor,
        status: {
          phase: 'Error',
          conditions: [{ type: 'Ready', status: 'False', reason: 'InvalidRepositoryURL', message: 'repoURL must be a GitHub repository root' }],
        },
      },
    })
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('InvalidRepositoryURL')
    expect(alert).toHaveTextContent('repoURL must be a GitHub repository root')
  })

  it('titles the page and browser tab with the monitor name, with the repository below', () => {
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    expect(screen.getByRole('heading', { level: 1, name: 'repo-monitor' })).toBeInTheDocument()
    expect(screen.getByText('orka-agents/orka')).toBeInTheDocument()
    expect(document.title).toBe('repo-monitor · Orka')
  })

  it('renders a titled not-found page and runs no detail queries for a missing monitor', () => {
    mockUseRepositoryMonitor.mockReturnValue({
      isLoading: false,
      data: undefined,
      error: new ApiError(404, '{"error":{"code":404,"message":"repository monitor not found"}}'),
    })
    render(<RepositoryMonitorDetail monitorName="gone-monitor" />)

    expect(screen.getByRole('heading', { level: 1, name: 'Repository monitor not found' })).toBeInTheDocument()
    expect(screen.getByText('No repository monitor named "gone-monitor" in default')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to Monitors' })).toHaveAttribute('href', '/monitors')
    expect(document.title).toBe('Repository monitor not found · Orka')
    expect(mockDetailQuery).not.toHaveBeenCalled()
  })

  it('prefers the not-found page over cached data once the monitor 404s', () => {
    mockUseRepositoryMonitor.mockReturnValue({
      isLoading: false,
      data: readyMonitor,
      error: new ApiError(404, '{"error":{"code":404,"message":"repository monitor not found"}}'),
    })
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    expect(screen.getByRole('heading', { level: 1, name: 'Repository monitor not found' })).toBeInTheDocument()
  })

  it('shows no alert for a ready monitor', () => {
    render(<RepositoryMonitorDetail monitorName="repo-monitor" />)

    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
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
