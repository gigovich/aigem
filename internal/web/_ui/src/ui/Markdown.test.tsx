import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'
import { Markdown } from './Markdown'

test('renders the subset the skill bodies use', () => {
  const { container } = render(
    <Markdown source={'# Title\n\nSome **bold** and `code`.\n\n- one\n- two\n\n```\nrun me\n```'} />,
  )
  expect(screen.getByRole('heading', { name: 'Title' })).toBeInTheDocument()
  expect(container.querySelector('strong')).toHaveTextContent('bold')
  expect(container.querySelectorAll('li')).toHaveLength(2)
  expect(container.querySelector('pre')).toHaveTextContent('run me')
})

/*
 * This page draws model output and the text of skills the agent may have been
 * pointed at by a page an attacker wrote. The parser produces React elements
 * and never HTML, so a tag it does not know how to build cannot be built - and
 * these are the cases that would matter if it did.
 */

test('never builds an element out of the text it is given', () => {
  const { container } = render(
    <Markdown source={'<img src=x onerror="alert(1)"> and <script>alert(2)</script>'} />,
  )
  expect(container.querySelector('img')).toBeNull()
  expect(container.querySelector('script')).toBeNull()
  // It is shown as what it said, because that is what the document contained.
  expect(container.textContent).toContain('<img src=x')
})

test('follows only a scheme this page will navigate to', () => {
  const { container } = render(
    <Markdown
      source={
        '[safe](https://example.test/x)\n\n[local](/models)\n\n' +
        '[bad](javascript:alert(1))\n\n[worse](data:text/html,<script>)'
      }
    />,
  )
  const links = [...container.querySelectorAll('a')].map((a) => a.getAttribute('href'))
  expect(links).toEqual(['https://example.test/x', '/models'])
  // The refused ones are still visible, so a reader can see there was a link
  // and that it was not made live.
  expect(container.textContent).toContain('javascript:alert(1)')
})

test('an external link cannot reach back into this page', () => {
  const { container } = render(<Markdown source="[out](https://example.test/)" />)
  const link = container.querySelector('a')
  expect(link).toHaveAttribute('rel', 'noreferrer noopener')
  expect(link).toHaveAttribute('target', '_blank')
})

test('an empty document renders nothing rather than failing', () => {
  const { container } = render(<Markdown source="" />)
  expect(container.textContent).toBe('')
})

// A fence the writer never closed must not swallow the parser.
test('an unterminated code fence still terminates', () => {
  const { container } = render(<Markdown source={'```\nnever closed'} />)
  expect(container.querySelector('pre')).toHaveTextContent('never closed')
})
