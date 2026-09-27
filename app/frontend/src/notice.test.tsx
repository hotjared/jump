// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import Notice, { type NoticeMessage } from './Notice'

afterEach(() => { cleanup(); vi.useRealTimers() })

it('shows and dismisses successful notices after five seconds', () => {
  vi.useFakeTimers()
  function Host() {
    const [notice, setNotice] = useState<NoticeMessage | null>({ text: 'RDP session started', severity: 'success' })
    return <Notice notice={notice} dismiss={() => setNotice(null)} />
  }
  render(<Host />)
  expect(screen.getByRole('status').textContent).toContain('RDP session started')
  act(() => { vi.advanceTimersByTime(4999) })
  expect(screen.getByRole('status')).toBeTruthy()
  act(() => { vi.advanceTimersByTime(1) })
  expect(screen.queryByRole('status')).toBeNull()
})

it('allows immediate manual dismissal', () => {
  vi.useFakeTimers()
  const dismiss = vi.fn()
  const view = render(<Notice notice={{ text: 'Connected', severity: 'info' }} dismiss={dismiss} />)
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss notification' }))
  expect(dismiss).toHaveBeenCalledOnce()
  view.rerender(<Notice notice={null} dismiss={dismiss} />)
  expect(screen.queryByRole('status')).toBeNull()
  act(() => { vi.advanceTimersByTime(5000) })
  expect(dismiss).toHaveBeenCalledOnce()
})

it('keeps errors and warnings until dismissed', () => {
  vi.useFakeTimers()
  const dismiss = vi.fn()
  const view = render(<Notice notice={{ text: 'Connection failed', severity: 'error' }} dismiss={dismiss} />)
  act(() => { vi.advanceTimersByTime(6000) })
  expect(screen.getByRole('alert').textContent).toContain('Connection failed')
  view.rerender(<Notice notice={{ text: 'Permission denied', severity: 'warning' }} dismiss={dismiss} />)
  act(() => { vi.advanceTimersByTime(6000) })
  expect(screen.getByRole('alert').textContent).toContain('Permission denied')
  expect(dismiss).not.toHaveBeenCalled()
})

it('restarts the timer when another success replaces the first and clears it on unmount', () => {
  vi.useFakeTimers()
  const dismiss = vi.fn()
  const first: NoticeMessage = { text: 'SSH session started', severity: 'success' }
  const second: NoticeMessage = { text: 'RDP session started', severity: 'success' }
  const view = render(<Notice notice={first} dismiss={dismiss} />)
  act(() => { vi.advanceTimersByTime(4000) })
  view.rerender(<Notice notice={second} dismiss={dismiss} />)
  expect(screen.getByRole('status').textContent).toContain('RDP session started')
  act(() => { vi.advanceTimersByTime(1000) })
  expect(dismiss).not.toHaveBeenCalled()
  act(() => { vi.advanceTimersByTime(4000) })
  expect(dismiss).toHaveBeenCalledOnce()
  view.unmount()
  act(() => { vi.advanceTimersByTime(5000) })
  expect(dismiss).toHaveBeenCalledOnce()
})
