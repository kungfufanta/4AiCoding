import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import { Notes, parseNotes } from './notes'

describe('parseNotes', () => {
  it('splits headings, lists and paragraphs', () => {
    const md = "## What's Changed\n* one\n- two\n\nSome text\nmore text\n\n### Sub ###\n"
    expect(parseNotes(md)).toEqual([
      { kind: 'heading', level: 2, text: "What's Changed" },
      { kind: 'list', items: ['one', 'two'] },
      { kind: 'para', text: 'Some text more text' },
      { kind: 'heading', level: 3, text: 'Sub' },
    ])
  })
  it('handles CRLF and empty input', () => {
    expect(parseNotes('')).toEqual([])
    expect(parseNotes('# A\r\n* b\r\n')).toEqual([{ kind: 'heading', level: 1, text: 'A' }, { kind: 'list', items: ['b'] }])
  })
})

describe('Notes', () => {
  it('renders bold, inline code and links', () => {
    const { container } = render(<Notes md="* **Bold** and `code` and [docs](https://example.com/x) by https://github.com/o/r/pull/12" />)
    expect(container.querySelector('b')?.textContent).toBe('Bold')
    expect(container.querySelector('code')?.textContent).toBe('code')
    const as = container.querySelectorAll('a')
    expect(as[0].getAttribute('href')).toBe('https://example.com/x')
    expect(as[0].textContent).toBe('docs')
    expect(as[1].getAttribute('href')).toBe('https://github.com/o/r/pull/12')
    expect(as[1].textContent).toBe('#12')
    expect(as[0].getAttribute('rel')).toBe('noreferrer')
  })
  it('escapes html and refuses non-http links', () => {
    const md = '## <script>alert(1)</script>\n* [x](javascript:alert(1)) <img src=x onerror=alert(1)>'
    const { container } = render(<Notes md={md} />)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('a')).toBeNull()
    expect(container.textContent).toContain('<script>alert(1)</script>')
    expect(container.textContent).toContain('[x](javascript:alert(1))')
  })
  it('renders headings and list items', () => {
    const { container } = render(<Notes md={'## Breaking changes\n* a\n* b\n\npara'} testid="n" />)
    expect(container.querySelectorAll('li').length).toBe(2)
    expect(container.querySelector('p')?.textContent).toBe('para')
    expect(container.querySelector('[data-testid="n"]')).not.toBeNull()
  })
})
