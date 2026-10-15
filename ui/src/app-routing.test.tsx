import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

// The app wires the router and the sign-in capture at module load, so each
// test loads it fresh against the address it starts from.
async function loadApp(url: string) {
  window.history.replaceState(null, '', url)
  vi.resetModules()
  const [{ App }, { useAuthStore }] = await Promise.all([import('./app'), import('./stores/auth')])
  return { App, useAuthStore }
}

describe('App routing', () => {
  beforeEach(() => {
    sessionStorage.clear()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    })
  })

  it('signs in from a #token= deep link and lands on the deep link', async () => {
    const { App, useAuthStore } = await loadApp('/security?view=all#token=deep-link-token')
    render(<App />)

    await waitFor(() => expect(useAuthStore.getState().token).toBe('deep-link-token'))
    await waitFor(() => expect(window.location.pathname).toBe('/security'))
    expect(window.location.search).toBe('?view=all')
    expect(window.location.hash).toBe('')
    expect(await screen.findByRole('heading', { level: 1, name: 'Security' })).toBeInTheDocument()
  })
})
