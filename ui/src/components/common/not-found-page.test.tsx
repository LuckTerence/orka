import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useLocation: () => ({ pathname: '/no-such-page' }),
  }
})

import { render, screen } from '@/test/test-utils'
import { NotFoundPage } from './not-found-page'

describe('NotFoundPage', () => {
  beforeEach(() => {
    document.title = 'Orka'
  })

  it('names the missing address and links back to the Dashboard', () => {
    render(<NotFoundPage />)
    expect(screen.getByRole('heading', { level: 1, name: 'Page not found' })).toBeInTheDocument()
    expect(screen.getByText('Nothing lives at /no-such-page')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to Dashboard' })).toHaveAttribute('href', '/')
    expect(document.title).toBe('Page not found · Orka')
  })
})
