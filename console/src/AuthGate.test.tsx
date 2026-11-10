import { describe, it, expect } from 'vitest'
import { render, fireEvent, screen } from '@testing-library/react'
import { Login } from './AuthGate'

describe('Login', () => {
  it('toggles the forgot-password panel with the reset command', () => {
    render(<Login onDone={() => {}} />)
    expect(screen.queryByTestId('login-forgot-panel')).toBeNull()
    fireEvent.click(screen.getByTestId('login-forgot'))
    const panel = screen.getByTestId('login-forgot-panel')
    expect(panel.textContent).toContain('./console-login.sh --reset-password')
    expect(panel.textContent).toContain('Then reload this page and create a new password.')
    fireEvent.click(screen.getByTestId('login-forgot'))
    expect(screen.queryByTestId('login-forgot-panel')).toBeNull()
  })
})
