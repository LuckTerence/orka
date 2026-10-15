import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

const navigateMock = vi.hoisted(() => vi.fn())

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => navigateMock,
    useLocation: () => ({ pathname: '/tasks' }),
  }
})

import { toast } from 'sonner'

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskList } from './task-list'

describe('TaskList', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    vi.clearAllMocks()
  })

  it('loading state shows skeleton rows', () => {
    // Default handler returns empty list, but first render is loading
    server.use(
      http.get('/api/v1/tasks', async () => {
        await new Promise((r) => setTimeout(r, 5000))
        return HttpResponse.json({ items: [], metadata: {} })
      }),
    )
    const { container } = render(<TaskList />)
    const skeletons = container.querySelectorAll('[data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('empty state shows "No tasks found"', async () => {
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText(/No tasks found/)).toBeInTheDocument()
    })
  })

  it('populated table shows task rows', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'task-1', namespace: 'default', uid: 'uid-1', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 'task-2', namespace: 'prod', uid: 'uid-2', creationTimestamp: new Date().toISOString() },
              spec: { type: 'ai' },
              status: { phase: 'Succeeded' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText('task-1')).toBeInTheDocument()
    })
    expect(screen.getByText('task-2')).toBeInTheDocument()
    expect(screen.getByText('Container')).toBeInTheDocument()
    expect(screen.getByText('AI')).toBeInTheDocument()
    expect(screen.queryByText('Ai')).not.toBeInTheDocument()
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('Succeeded')).toBeInTheDocument()
    expect(screen.getByText('default')).toBeInTheDocument()
    expect(screen.getByText('prod')).toBeInTheDocument()
  })

  it('New Task button links to /tasks/new', async () => {
    render(<TaskList />)
    const link = screen.getByText('New Task').closest('a')
    expect(link).toHaveAttribute('href', '/tasks/new')
  })

  function serveDeletableTask() {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'del-task', namespace: 'default', uid: 'uid-del', creationTimestamp: new Date().toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Succeeded' },
            },
          ],
          metadata: {},
        }),
      ),
    )
  }

  it('asks for confirmation before deleting and deletes once on confirm', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    const deletes: string[] = []
    server.use(
      http.delete('/api/v1/tasks/:id', ({ params }) => {
        deletes.push(String(params.id))
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<TaskList />)
    await user.click(await screen.findByRole('button', { name: 'Delete task del-task' }))
    // The trash icon only arms the delete; nothing is sent yet.
    expect(screen.getByRole('dialog')).toHaveTextContent('del-task')
    expect(deletes).toEqual([])

    await user.dblClick(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Task del-task deleted'))
    expect(deletes).toEqual(['del-task'])
    expect(navigateMock).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('cancelling the confirmation sends no delete', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    const deletes: string[] = []
    server.use(
      http.delete('/api/v1/tasks/:id', ({ params }) => {
        deletes.push(String(params.id))
        return new HttpResponse(null, { status: 204 })
      }),
    )
    render(<TaskList />)
    await user.click(await screen.findByRole('button', { name: 'Delete task del-task' }))
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(deletes).toEqual([])
  })

  it('shows the server message without a repeated prefix when the delete fails', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    server.use(
      http.delete('/api/v1/tasks/:id', () =>
        HttpResponse.json({ error: { code: 500, message: 'failed to delete task: etcd unavailable' } }, { status: 500 }),
      ),
    )
    render(<TaskList />)
    await user.click(await screen.findByRole('button', { name: 'Delete task del-task' }))
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete task: etcd unavailable'))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('navigates when any part of a row is clicked', async () => {
    const user = userEvent.setup()
    serveDeletableTask()
    render(<TaskList />)
    await user.click(await screen.findByText('Succeeded'))
    expect(navigateMock).toHaveBeenCalledWith({ to: '/tasks/$taskId', params: { taskId: 'del-task' } })
  })

  it('timeAgo covers minutes, hours, and days', async () => {
    const now = Date.now()
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 't-min', namespace: 'default', uid: 'u1', creationTimestamp: new Date(now - 120_000).toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 't-hr', namespace: 'default', uid: 'u2', creationTimestamp: new Date(now - 7200_000).toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
            {
              metadata: { name: 't-day', namespace: 'default', uid: 'u3', creationTimestamp: new Date(now - 172800_000).toISOString() },
              spec: { type: 'container' },
              status: { phase: 'Running' },
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => {
      expect(screen.getByText('t-min')).toBeInTheDocument()
    })
    expect(screen.getByText('2m')).toBeInTheDocument()
    expect(screen.getByText('2h')).toBeInTheDocument()
    expect(screen.getByText('2d')).toBeInTheDocument()
  })
})

describe('TaskList paging and access', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('follows continue tokens through a Load more control and filters loaded rows', async () => {
    server.use(
      http.get('/api/v1/tasks', ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('continue') === 'page-2') {
          return HttpResponse.json({
            items: [{ metadata: { name: 'zeta-task', namespace: 'default' }, spec: { type: 'ai' }, status: { phase: 'Succeeded' } }],
            metadata: {},
          })
        }
        return HttpResponse.json({
          items: [{ metadata: { name: 'alpha-task', namespace: 'default' }, spec: { type: 'container' }, status: { phase: 'Running' } }],
          metadata: { continue: 'page-2', remainingItemCount: 1 },
        })
      }),
    )
    const user = userEvent.setup()
    render(<TaskList />)
    await waitFor(() => expect(screen.getByText('alpha-task')).toBeInTheDocument())
    expect(screen.queryByText('zeta-task')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /Load more/ }))
    await waitFor(() => expect(screen.getByText('zeta-task')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: /Load more/ })).not.toBeInTheDocument()

    await user.type(screen.getByLabelText('Filter tasks'), 'succeeded')
    expect(screen.getByText('zeta-task')).toBeInTheDocument()
    expect(screen.queryByText('alpha-task')).not.toBeInTheDocument()
  })

  it('shows a not-authorized panel instead of the empty-state CTA on 403', async () => {
    server.use(
      http.get('/api/v1/tasks', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<TaskList />)
    await waitFor(() => expect(screen.getByText('Not authorized to view tasks')).toBeInTheDocument())
    expect(screen.queryByText(/No tasks found/)).not.toBeInTheDocument()
  })
})
