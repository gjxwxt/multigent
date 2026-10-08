import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { App } from '../App'
import { HomePage } from '../pages/HomePage'
import { NotFoundPage } from '../pages/NotFoundPage'
import { api } from '../services/api'

const mockHealth = { status: 'UP', timestamp: '2026-09-09T12:00:00Z', service: 'server' }
const mockItem = {
  id: 'item-1',
  title: 'Task 1',
  description: 'Desc 1',
  status: 'PENDING',
  createdAt: '2026-09-09T12:00:00Z',
}

function renderWithRouter(ui: React.ReactElement, initialEntries: string[] = ['/']) {
  return render(<MemoryRouter initialEntries={initialEntries}>{ui}</MemoryRouter>)
}

describe('API service unit tests', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('fetches health status from /api/health', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => mockHealth,
    })

    const result = await api.getHealth()
    expect(result.status).toBe('UP')
    expect(result.service).toBe('server')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/health', expect.anything())
  })

  it('fetches item list from /api/v1/items', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => [mockItem],
    })

    const result = await api.getItems()
    expect(result).toHaveLength(1)
    expect(result[0].id).toBe('item-1')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/items', expect.anything())
  })

  it('fetches a single item from /api/v1/items/:id', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => mockItem,
    })

    const result = await api.getItem('item-1')
    expect(result.id).toBe('item-1')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/items/item-1', expect.anything())
  })

  it('creates an item with POST to /api/v1/items', async () => {
    const newItem = { ...mockItem, id: 'item-2', title: 'New' }
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => newItem,
    })

    const result = await api.createItem({ title: 'New', description: 'Desc' })
    expect(result.id).toBe('item-2')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/items', expect.objectContaining({
      method: 'POST',
    }))
  })
})

describe('App routing integration tests', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('renders home route with backend status and initial items', async () => {
    vi.spyOn(api, 'getHealth').mockResolvedValue(mockHealth)
    vi.spyOn(api, 'getItems').mockResolvedValue([
      { ...mockItem, title: 'Existing Task', description: 'Sample description' },
    ])

    renderWithRouter(<App />)

    expect(await screen.findByText('Backend Connected (Java 21)')).toBeInTheDocument()
    expect(screen.getByText('Spring Boot 3.3 + React 18')).toBeInTheDocument()
    expect(screen.getByText('Existing Task')).toBeInTheDocument()
  })

  it('handles backend offline error gracefully', async () => {
    vi.spyOn(api, 'getHealth').mockRejectedValue(new Error('Network error'))
    vi.spyOn(api, 'getItems').mockResolvedValue([])

    renderWithRouter(<App />)

    expect(await screen.findByText('Backend Offline')).toBeInTheDocument()
  })

  it('renders the unknown-route fallback', () => {
    renderWithRouter(<NotFoundPage />)
    expect(screen.getByText('Page not found')).toBeInTheDocument()
  })

  it('navigates from the list to the item detail page', async () => {
    const user = userEvent.setup()
    vi.spyOn(api, 'getHealth').mockResolvedValue(mockHealth)
    const getItemsMock = vi.spyOn(api, 'getItems')
    getItemsMock.mockResolvedValue([{ ...mockItem, title: 'Task to Open' }])
    const getItemMock = vi.spyOn(api, 'getItem')
    getItemMock.mockResolvedValue({ ...mockItem, title: 'Task to Open' })

    renderWithRouter(<App />)

    const detailLink = await screen.findByRole('link', { name: 'Task to Open' })
    await user.click(detailLink)

    expect(await screen.findByText('Item Detail')).toBeInTheDocument()
    expect(screen.getByText('Task to Open')).toBeInTheDocument()
    expect(getItemMock).toHaveBeenCalledWith('item-1')
  })

  it('shows the not-found state when the detail id does not exist', async () => {
    vi.spyOn(api, 'getItem').mockRejectedValue({
      code: 'RESOURCE_NOT_FOUND',
      message: 'nope',
      details: [],
      timestamp: '2026-09-09T12:00:00Z',
    })

    renderWithRouter(<App />, ['/items/missing-id'])

    expect(await screen.findByText('Item not found')).toBeInTheDocument()
  })
})

describe('HomePage CRUD interaction tests', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('opens modal, validates empty title, and creates an item', async () => {
    const user = userEvent.setup()
    vi.spyOn(api, 'getHealth').mockResolvedValue(mockHealth)
    const getItemsMock = vi.spyOn(api, 'getItems')
    getItemsMock.mockResolvedValueOnce([])
    const createMock = vi.spyOn(api, 'createItem').mockResolvedValue({
      ...mockItem,
      id: 'item-new',
      title: 'New Integration Test Task',
    })
    getItemsMock.mockResolvedValueOnce([{ ...mockItem, id: 'item-new', title: 'New Integration Test Task' }])

    renderWithRouter(<HomePage />)

    const addButton = await screen.findByRole('button', { name: /\+ Add Item/i })
    await user.click(addButton)

    expect(screen.getByText('Create New Item')).toBeInTheDocument()

    // Test form validation: submit empty title
    const submitButton = screen.getByRole('button', { name: /^Create$/i })
    await user.click(submitButton)
    expect(screen.getByText('Title cannot be empty')).toBeInTheDocument()
    expect(createMock).not.toHaveBeenCalled()

    // Fill in valid title and description
    const titleInput = screen.getByPlaceholderText(/e\.g\. Implement User Authentication/i)
    const descInput = screen.getByPlaceholderText(/Optional notes or details\.\.\./i)
    await user.type(titleInput, 'New Integration Test Task')
    await user.type(descInput, 'Created via test')

    await user.click(submitButton)

    await waitFor(() => {
      expect(createMock).toHaveBeenCalledWith({
        title: 'New Integration Test Task',
        description: 'Created via test',
      })
    })

    expect(await screen.findByText('New Integration Test Task')).toBeInTheDocument()
  })

  it('deletes an item when delete button is clicked', async () => {
    const user = userEvent.setup()
    vi.spyOn(api, 'getHealth').mockResolvedValue(mockHealth)
    const deleteMock = vi.spyOn(api, 'deleteItem').mockResolvedValue()
    const getItemsMock = vi.spyOn(api, 'getItems')
    getItemsMock.mockResolvedValue([{ ...mockItem, title: 'Task to Delete' }])

    renderWithRouter(<HomePage />)

    const deleteBtn = await screen.findByRole('button', { name: /Delete Task to Delete/i })
    await user.click(deleteBtn)

    await waitFor(() => {
      expect(deleteMock).toHaveBeenCalledWith('item-1')
    })
  })
})
