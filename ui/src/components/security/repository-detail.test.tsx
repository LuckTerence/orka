import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => {
      const { params, ...anchorProps } = props
      void params
      return <a href={to} {...anchorProps}>{children}</a>
    },
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { focusManager } from '@tanstack/react-query'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { act, render, screen } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import type { RepositoryScan } from '@/schemas/security'
import { RepositoryDetail } from './repository-detail'

const repositoryPath = '/api/v1/security/repositories/payments-main'

function repository(status: RepositoryScan['status']): RepositoryScan {
  return {
    metadata: { name: 'payments-main', namespace: 'default' },
    spec: {
      repoURL: 'https://github.com/example/payments',
      branch: 'main',
      analysisAgentRef: { name: 'security-agent' },
    },
    status,
  }
}

function serveRepository(repo: RepositoryScan) {
  server.use(
    http.get(repositoryPath, () => HttpResponse.json(repo)),
    http.get(`${repositoryPath}/threat-model`, () =>
      HttpResponse.json({ error: { code: 404, message: 'threat model not found' } }, { status: 404 })),
    http.get(`${repositoryPath}/:collection`, () => HttpResponse.json({ items: [] })),
  )
}

describe('RepositoryDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('alerts with the Ready=False reason and message of an errored repository', async () => {
    serveRepository(repository({
      phase: 'Error',
      conditions: [{
        type: 'Ready',
        status: 'False',
        reason: 'InvalidRepository',
        message: 'repoURL must be a credential-free GitHub repository root',
      }],
    }))

    render(<RepositoryDetail repositoryName="payments-main" />)

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('InvalidRepository')
    expect(alert).toHaveTextContent('repoURL must be a credential-free GitHub repository root')
  })

  it('titles the page and browser tab with the RepositoryScan name, with the repository below', async () => {
    serveRepository(repository({ phase: 'Ready' }))

    render(<RepositoryDetail repositoryName="payments-main" />)

    expect(await screen.findByRole('heading', { level: 1, name: 'payments-main' })).toBeInTheDocument()
    expect(screen.getByText('example/payments')).toBeInTheDocument()
    expect(document.title).toBe('payments-main · Orka')
  })

  it('shows no alert for a ready repository', async () => {
    serveRepository(repository({
      phase: 'Ready',
      conditions: [{ type: 'Ready', status: 'True', reason: 'ScanSucceeded', message: 'scan completed successfully' }],
    }))

    render(<RepositoryDetail repositoryName="payments-main" />)

    expect(await screen.findByText('Ready')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('renders a titled not-found page and sends no detail requests for a missing repository', async () => {
    const requests: string[] = []
    server.use(http.all('/api/v1/security/*', ({ request }) => {
      requests.push(new URL(request.url).pathname)
      return HttpResponse.json({ error: { code: 404, message: 'repository scan not found' } }, { status: 404 })
    }))

    render(<RepositoryDetail repositoryName="findings" />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Repository not found' })).toBeInTheDocument()
    expect(screen.getByText('No repository named "findings" in default')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to Security' })).toHaveAttribute('href', '/security')
    expect(document.title).toBe('Repository not found · Orka')
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(requests).toEqual(['/api/v1/security/repositories/findings'])
  })

  it('says the token cannot read the repository instead of "not found" on 403', async () => {
    server.use(http.get(repositoryPath, () =>
      HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })))

    render(<RepositoryDetail repositoryName="payments-main" />)

    expect(await screen.findByText('Not authorized to view repository payments-main')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Repository not found' })).not.toBeInTheDocument()
    // The access error keeps the page chrome: a heading, tab title, and a way back.
    expect(screen.getByRole('heading', { level: 1, name: 'Could not load repository' })).toBeInTheDocument()
    expect(document.title).toBe('Could not load repository · Orka')
    expect(screen.getByRole('link', { name: 'Back to Security' })).toHaveAttribute('href', '/security')
  })

  it('switches to the not-found page when a loaded repository is deleted', async () => {
    let deleted = false
    serveRepository(repository({ phase: 'Ready' }))
    server.use(http.get(repositoryPath, () => deleted
      ? HttpResponse.json({ error: { code: 404, message: 'repository scan not found' } }, { status: 404 })
      : HttpResponse.json(repository({ phase: 'Ready' }))))

    render(<RepositoryDetail repositoryName="payments-main" />)
    expect(await screen.findByRole('heading', { level: 1, name: 'payments-main' })).toBeInTheDocument()

    deleted = true
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })

    expect(await screen.findByRole('heading', { level: 1, name: 'Repository not found' })).toBeInTheDocument()
    act(() => focusManager.setFocused(undefined))
  })
})
