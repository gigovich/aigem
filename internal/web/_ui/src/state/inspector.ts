/**
 * What the inspector is showing.
 *
 * The panel is drawn by the shell and filled by whichever screen owns the
 * selection. A store rather than a prop chain because the shell renders the
 * panel beside the screen, not inside it - and the alternative is the shell
 * knowing how to describe a model, a skill and a run.
 */

import { useEffect } from 'react'
import type { ReactNode } from 'react'
import { createStore, useStore } from '@/lib/store'
import type { StatusKey } from '@/lib/wire'
import type { Field } from '@/ui/FieldList'

export type InspectorRow = { icon: string; color?: string; text: string; meta?: string }

/**
 * What a screen publishes for the panel to draw.
 *
 * Declared here and not in the component that renders it: the contract belongs
 * to the publisher, and a `state/` module importing a type out of `shell/`
 * inverts the dependency every other import in this tree keeps.
 */
export type InspectorContent = {
  kind: string
  id: string
  title: ReactNode
  status?: StatusKey
  fields: Field[]
  progress?: { used: number; total: number }
  listTitle?: string
  list?: InspectorRow[]
  /** The agent's working plan, drawn above `list` when present. */
  plan?: InspectorRow[]
  actions?: { label: string; onClick: () => void }[]
} | null

const store = createStore<InspectorContent>(null)

export function setInspectorContent(content: InspectorContent) {
  store.set(content)
}

export function useInspectorContent(): InspectorContent {
  return useStore(store, (c) => c)
}

/**
 * Publish the panel's contents for as long as the screen is mounted.
 *
 * `content` must be memoised by the caller: it is the effect's dependency, and
 * a fresh object per render would republish on every keystroke.
 */
export function usePublishInspector(content: InspectorContent) {
  useEffect(() => {
    setInspectorContent(content)
    return () => setInspectorContent(null)
  }, [content])
}
