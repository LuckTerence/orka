import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { render, screen } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { FindingDetail } from './finding-detail'

describe('FindingDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('renders a titled not-found page and sends no patch request for a missing finding', async () => {
    const requests: string[] = []
    server.use(http.all('/api/v1/security/*', ({ request }) => {
      requests.push(new URL(request.url).pathname)
      return HttpResponse.json({ error: { code: 404, message: 'finding not found' } }, { status: 404 })
    }))

    render(<FindingDetail findingId="f-404" />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Finding not found' })).toBeInTheDocument()
    expect(screen.getByText('No finding with ID "f-404" in default')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to Security' })).toHaveAttribute('href', '/security')
    expect(document.title).toBe('Finding not found · Orka')
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(requests).toEqual(['/api/v1/security/findings/f-404'])
  })
})
