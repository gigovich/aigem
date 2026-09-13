/**
 * The slash commands the page carries out itself.
 *
 * The daemon runs the ones that are a turn - a compaction, a skill, an MCP
 * prompt. The rest of its catalogue name things a browser does with a
 * navigation or an HTTP call, and sending them up the socket would only come
 * back refused as unknown.
 */

import { navigate } from '@/lib/route'
import type { RunOp } from '@/lib/wire'
import { flash, openSession, setLogin, store } from './app'

/** Commands the daemon lists that have no browser shape at all. */
export const NOT_HERE = new Set(['/agents', '/logout'])

/** Carry out a typed slash line. Reports whether it was taken. */
export function slash(line: string, send: (op: RunOp) => boolean): boolean {
  const [name = '', ...rest] = line.slice(1).split(' ')
  const args = rest.join(' ').trim()
  switch (name) {
    case '':
      return false
    case 'new':
      void openSession()
      return true
    case 'model':
      if (args) return send({ op: 'switch_model', ref: args })
      navigate({ screen: 'models' })
      return true
    case 'login':
      if (args) setLogin(args)
      else {
        navigate({ screen: 'models' })
        flash('Pick a model to sign in to its provider, or say which: /login openai')
      }
      return true
    case 'resume':
      navigate({ screen: 'chat' })
      return true
    case 'skills':
      navigate({ screen: 'skills' })
      return true
    case 'artifacts': {
      const id = store.get().activeRun
      navigate(id ? { screen: 'run', id } : { screen: 'chat' })
      return true
    }
    default:
      return send({ op: 'command', name, args })
  }
}
