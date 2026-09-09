import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { App } from '../App'
import { api } from '../services/api'

describe('API service unit tests', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('fetches health status from /api/health', async () => {
    const mockHealth = { status: 'UP', timestamp: '2026-09-09T12:00:00Z', service: 'server' }
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => mockHealth,
    })

    const result = await api.getHealth()
    expect(result.status).toBe('UP')
    expect(result.service).toBe('server')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/health')
  })

  it('fetches item list from /api/v1/items', async () => {
    const mockItems = [
      { id: 'item-1', title: 'Task 1', description: 'Desc 1', status: 'PENDING', createdAt: '2026-09-09T12:00:00Z' },
    ]
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => mockItems,
    })

    const result = await api.getItems()
    expect(result).toHaveLength(1)
    expect(result[0].id).toBe('item-1')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/items')
  })

  it('creates an item with POST to /api/v1/items', async () => {
    const newItem = { id: 'item-2', title: 'New', description: 'Desc', status: 'PENDING', createdAt: '2026-09-09T12:00:00Z' }
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

describe('App Component RTL Integration Tests', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('renders application with backend status and initial items', async () => {
    vi.spyOn(api, 'getHealth').mockResolvedValue({
      status: 'UP',
      timestamp: '2026-09-09T12:00:00Z',
      service: 'server',
    })
    vi.spyOn(api, 'getItems').mockResolvedValue([
      { id: 'item-1', title: 'Existing Task', description: 'Sample description', status: 'COMPLETED', createdAt: '2026-09-09T12:00:00Z' },
    ])

    render(<App />)

    expect(await screen.findByText('Backend Connected (Java 21)')).toBeInTheDocument()
    expect(screen.getByText('Spring Boot 3.3 + React 18')).toBeInTheDocument()
    expect(screen.getByText('Existing Task')).toBeInTheDocument()
    expect(screen.getByText('Sample description')).toBeInTheDocument()
  })

  it('handles backend offline error gracefully', async () => {
    vi.spyOn(api, 'getHealth').mockRejectedValue(new Error('Network error'))
    vi.spyOn(api, 'getItems').mockResolvedValue([])

    render(<App />)

    expect(await screen.findByText('Backend Offline')).toBeInTheDocument()
  })

  it('opens modal, validates empty title, and creates an item', async () => {
    const user = userEvent.setup()
    vi.spyOn(api, 'getHealth').mockResolvedValue({
      status: 'UP',
      timestamp: '2026-09-09T12:00:00Z',
      service: 'server',
    })
    const getItemsMock = vi.spyOn(api, 'getItems')
    getItemsMock.mockResolvedValueOnce([])
    const createMock = vi.spyOn(api, 'createItem').mockResolvedValue({
      id: 'item-new',
      title: 'New Integration Test Task',
      description: 'Created via test',
      status: 'PENDING',
      createdAt: '2026-09-09T12:00:00Z',
    })
    getItemsMock.mockResolvedValueOnce([
      { id: 'item-new', title: 'New Integration Test Task', description: 'Created via test', status: 'PENDING', createdAt: '2026-09-09T12:00:00Z' },
    ])

    render(<App />)

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
    vi.spyOn(api, 'getHealth').mockResolvedValue({
      status: 'UP',
      timestamp: '2026-09-09T12:00:00Z',
      service: 'server',
    })
    const deleteMock = vi.spyOn(api, 'deleteItem').mockResolvedValue()
    const getItemsMock = vi.spyOn(api, 'getItems')
    getItemsMock.mockResolvedValueOnce([
      { id: 'item-1', title: 'Task to Delete', description: 'Will be removed', status: 'PENDING', createdAt: '2026-09-09T12:00:00Z' },
    ])
    getItemsMock.mockResolvedValueOnce([])

    render(<App />)

    const deleteBtn = await screen.findByRole('button', { name: /Delete Task to Delete/i })
    await user.click(deleteBtn)

    await waitFor(() => {
      expect(deleteMock).toHaveBeenCalledWith('item-1')
    })
  })
})
