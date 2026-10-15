import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
const mockUseAgentList = vi.fn()
const mockMutateAsync = vi.fn()
const mockUseSecretNames = vi.fn()

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    useNavigate: () => mockNavigate,
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@/hooks/use-security', () => ({
  useCreateRepositoryScan: () => ({ mutateAsync: mockMutateAsync, isPending: false }),
}))

vi.mock('@/hooks/use-secrets', () => ({
  useSecretNames: () => mockUseSecretNames(),
}))

vi.mock('@/hooks/use-agents', () => ({
  useAgentList: (...args: unknown[]) => mockUseAgentList(...args),
}))

if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as any
}

import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { render, screen } from '@/test/test-utils'
import { ApiError } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'
import { RepositoryCreateForm } from './repository-create-form'

function installPointerPolyfills() {
  if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false
  if (!HTMLElement.prototype.setPointerCapture) HTMLElement.prototype.setPointerCapture = () => {}
  if (!HTMLElement.prototype.releasePointerCapture) HTMLElement.prototype.releasePointerCapture = () => {}
  if (!HTMLElement.prototype.scrollIntoView) HTMLElement.prototype.scrollIntoView = () => {}
}

function withDefaultNamespaceAgents() {
  mockUseAgentList.mockImplementation(() => ({
    data: { items: [{ metadata: { name: 'security-agent' } }] },
    isLoading: false,
  }))
}

async function fillRequiredFields(user: ReturnType<typeof userEvent.setup>, repoURL: string) {
  await user.type(screen.getByLabelText('Name'), 'payments-api')
  await user.type(screen.getByLabelText('GitHub Repository URL'), repoURL)
  await user.click(screen.getByRole('combobox', { name: 'Analysis Agent' }))
  await user.click(await screen.findByRole('option', { name: 'security-agent' }))
}

describe('RepositoryCreateForm', () => {
  beforeEach(() => {
    installPointerPolyfills()
    mockNavigate.mockReset()
    mockMutateAsync.mockReset()
    mockMutateAsync.mockResolvedValue({})
    mockUseSecretNames.mockReset()
    mockUseSecretNames.mockReturnValue({ data: { items: [] }, error: null, isLoading: false })
    vi.mocked(toast.error).mockClear()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    mockUseAgentList.mockImplementation((options?: { namespace?: string; enabled?: boolean }) => {
      if (options?.enabled === false) {
        return { data: undefined, isLoading: false }
      }

      const namespace = options?.namespace ?? useUIStore.getState().namespace
      if (namespace === 'orka-system') {
        return {
          data: { items: [{ metadata: { name: 'gpt54-assistant' } }] },
          isLoading: false,
        }
      }

      return {
        data: { items: [] },
        isLoading: false,
      }
    })
  })

  it('shows a namespace hint and lets the user switch to orka-system when agents only exist there', async () => {
    const user = userEvent.setup()
    render(<RepositoryCreateForm />)

    expect(screen.getByText(/Showing agents from the/i)).toBeInTheDocument()
    expect(screen.getByText(/No agents are available in this namespace/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Register Repository' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: 'Switch to orka-system' }))

    expect(useUIStore.getState().namespace).toBe('orka-system')
  })

  it('names every field for assistive technology', () => {
    withDefaultNamespaceAgents()
    render(<RepositoryCreateForm />)

    for (const label of ['Name', 'Branch', 'GitHub Repository URL', 'Schedule', 'Sub-path']) {
      expect(screen.getByRole('textbox', { name: label })).toBeInTheDocument()
    }
    expect(screen.getByRole('spinbutton', { name: 'History Window (days)' })).toBeInTheDocument()
    for (const label of ['Analysis Agent', 'Patch Agent', 'Git Secret', 'Validation Mode']) {
      expect(screen.getByRole('combobox', { name: label })).toBeInTheDocument()
    }
  })

  it.each([
    'https://gitlab.com/org/repo',
    'https://github.com/org/repo/pull/7',
    'https://token@github.com/org/repo',
  ])('refuses to register %s', async (repoURL) => {
    withDefaultNamespaceAgents()
    const user = userEvent.setup()
    render(<RepositoryCreateForm />)

    await fillRequiredFields(user, repoURL)
    await user.click(screen.getByRole('button', { name: 'Register Repository' }))

    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('credential-free GitHub repository root'))
    expect(mockMutateAsync).not.toHaveBeenCalled()
  })

  it('refuses a schedule that is not a five-field cron expression', async () => {
    withDefaultNamespaceAgents()
    const user = userEvent.setup()
    render(<RepositoryCreateForm />)

    await fillRequiredFields(user, 'https://github.com/org/repo')
    const schedule = screen.getByLabelText('Schedule')
    await user.clear(schedule)
    await user.type(schedule, 'every 6 hours')
    await user.click(screen.getByRole('button', { name: 'Register Repository' }))

    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('five-field cron expression'))
    expect(mockMutateAsync).not.toHaveBeenCalled()
  })

  it('refuses a schedule with an out-of-range field', async () => {
    withDefaultNamespaceAgents()
    const user = userEvent.setup()
    render(<RepositoryCreateForm />)

    await fillRequiredFields(user, 'https://github.com/org/repo')
    const schedule = screen.getByLabelText('Schedule')
    await user.clear(schedule)
    await user.type(schedule, '0 */6 * * 7')
    await user.click(screen.getByRole('button', { name: 'Register Repository' }))

    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining('in-range values'))
    expect(mockMutateAsync).not.toHaveBeenCalled()
  })

  it('registers a GitHub repository with a typed Secret name when Secrets cannot be listed', async () => {
    withDefaultNamespaceAgents()
    mockUseSecretNames.mockReturnValue({ data: undefined, error: new ApiError(403, 'forbidden'), isLoading: false })
    const user = userEvent.setup()
    render(<RepositoryCreateForm />)

    await fillRequiredFields(user, '  https://github.com/org/repo  ')
    expect(screen.getByText("You can't list Secrets; type the Secret name.")).toBeInTheDocument()
    await user.type(screen.getByRole('textbox', { name: 'Git Secret' }), 'repo-token')
    await user.click(screen.getByRole('button', { name: 'Register Repository' }))

    expect(toast.error).not.toHaveBeenCalled()
    expect(mockMutateAsync).toHaveBeenCalledWith(expect.objectContaining({
      name: 'payments-api',
      spec: expect.objectContaining({
        repoURL: 'https://github.com/org/repo',
        schedule: '0 */6 * * *',
        analysisAgentRef: { name: 'security-agent' },
        gitSecretRef: { name: 'repo-token' },
      }),
    }))
  })
})
