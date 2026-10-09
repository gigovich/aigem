import { describe, expect, test } from 'vitest'
import type { Ticket } from '@/lib/wire'
import { blocks, personMoves, treeRows, waitsFor } from './tickets'

const t = (id: string, status: Ticket['status'], extra: Partial<Ticket> = {}): Ticket => ({
  id, repo: '', title: id, body: '', status, dependsOn: [], by: 'you', comments: [], runs: [],
  runnable: false, ...extra,
})

const all = [
  t('TCK-1', 'running', { progress: { done: 1, total: 2 } }),
  t('TCK-2', 'done', { parent: 'TCK-1' }),
  t('TCK-3', 'ready', { parent: 'TCK-1', dependsOn: ['TCK-2', 'TCK-4'] }),
  t('TCK-4', 'running'),
  t('TCK-5', 'closed', { title: 'old thing' }),
]

describe('treeRows', () => {
  test('subtickets follow their parent, one level deeper', () => {
    expect(treeRows(all, 'all', '', new Set()).map((r) => [r.ticket.id, r.depth])).toEqual([
      ['TCK-1', 0], ['TCK-2', 1], ['TCK-3', 1], ['TCK-4', 0], ['TCK-5', 0],
    ])
  })
  test('active hides done and closed, but keeps a parent whose subticket is shown', () => {
    expect(treeRows(all, 'active', '', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-1', 'TCK-3', 'TCK-4'])
  })
  test('a collapsed parent hides its subtickets', () => {
    expect(treeRows(all, 'all', '', new Set(['TCK-1'])).map((r) => r.ticket.id)).toEqual(['TCK-1', 'TCK-4', 'TCK-5'])
  })
  test('the filter matches id and title', () => {
    expect(treeRows(all, 'all', 'old', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-5'])
    expect(treeRows(all, 'all', 'tck-4', new Set()).map((r) => r.ticket.id)).toEqual(['TCK-4'])
  })
})

test('waitsFor lists the dependencies not done yet, blocks lists who waits', () => {
  expect(waitsFor(all[2]!, all)).toEqual(['TCK-4'])
  expect(blocks(all[3]!, all).map((x) => x.id)).toEqual(['TCK-3'])
})

test('personMoves mirrors the daemon rules', () => {
  expect(personMoves('open')).toEqual(['ready', 'closed'])
  expect(personMoves('ready')).toEqual(['open', 'done', 'closed'])
  expect(personMoves('blocked')).toEqual(['open', 'ready', 'done', 'closed'])
  expect(personMoves('closed')).toEqual(['open'])
  expect(personMoves('running')).toEqual([])
})
