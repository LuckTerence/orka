import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { toast } from 'sonner'
import { server } from '@/test/mocks/server'
import { render, screen, waitFor } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { ThreatModelEditor } from './threat-model-editor'

const threatModelPath = '/api/v1/security/repositories/payments/threat-model'

function threatModel(version: number, content: string) {
  return {
    namespace: 'default',
    repositoryScan: 'payments',
    version,
    content,
    source: 'generated',
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
  }
}

describe('ThreatModelEditor', () => {
  let stored: ReturnType<typeof threatModel>
  let puts: Array<{ content: string; expectedVersion?: number }>

  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
    stored = threatModel(3, 'Generated model v3')
    puts = []
    server.use(
      http.get(threatModelPath, () => HttpResponse.json(stored)),
      http.put(threatModelPath, async ({ request }) => {
        const body = await request.json() as { content: string; expectedVersion?: number }
        puts.push(body)
        if (body.expectedVersion && body.expectedVersion !== stored.version) {
          return HttpResponse.json({ error: { code: 409, message: 'threat model changed since it was loaded' } }, { status: 409 })
        }
        stored = { ...threatModel(stored.version + 1, body.content), source: 'edited' }
        return HttpResponse.json(stored)
      }),
    )
  })

  it('saves against the loaded version and builds the next save on the saved one', async () => {
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    const editor = await screen.findByDisplayValue('Generated model v3')
    await user.type(editor, ' + auth notes')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Threat model saved'))

    await user.type(editor, ' + more')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(puts).toHaveLength(2))

    expect(puts[0]).toMatchObject({ content: 'Generated model v3 + auth notes', expectedVersion: 3 })
    expect(puts[1]).toMatchObject({ content: 'Generated model v3 + auth notes + more', expectedVersion: 4 })
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('keeps the edits on a conflict and reloads the latest version on request', async () => {
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    const editor = await screen.findByDisplayValue('Generated model v3')
    // Someone else saves while this editor still holds v3.
    stored = threatModel(4, 'Teammate model v4')
    await user.clear(editor)
    await user.type(editor, 'My local edits')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('The threat model changed since you loaded it')
    expect(editor).toHaveValue('My local edits')
    expect(puts[0].expectedVersion).toBe(3)
    expect(toast.success).not.toHaveBeenCalled()
    expect(toast.error).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Reload latest' }))
    await waitFor(() => expect(editor).toHaveValue('Teammate model v4'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()

    await user.type(editor, ' + mine')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Threat model saved'))
    expect(puts[1]).toMatchObject({ content: 'Teammate model v4 + mine', expectedVersion: 4 })
  })

  it('reports other save failures with the server message', async () => {
    server.use(http.put(threatModelPath, () => HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })))
    const user = userEvent.setup()
    render(<ThreatModelEditor repositoryName="payments" />)

    await screen.findByDisplayValue('Generated model v3')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to save threat model: not authorized'))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
