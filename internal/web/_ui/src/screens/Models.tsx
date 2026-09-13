import { useCallback, useMemo, useState } from 'react'
import { api } from '@/lib/api'
import { compact } from '@/lib/format'
import { navigate } from '@/lib/route'
import { explain, flash, refresh, setBanner, setLogin, useApp } from '@/state/app'
import { MODEL_STATUS } from '@/lib/wire'
import type { Model, StatusInfo } from '@/lib/wire'
import { DataGrid } from '@/ui/DataGrid'
import type { Column } from '@/ui/DataGrid'
import { EmptyState } from '@/ui/EmptyState'
import { FilterInput } from '@/ui/FilterInput'
import { StatusChip } from '@/ui/StatusChip'
import { Modal } from '@/ui/Modal'
import { usePublishInspector } from '@/state/inspector'

/** Available / No key, from the shared dictionary rather than a local copy. */
function state(m: Model): StatusInfo {
  return !m.needsAuth || m.authenticated ? MODEL_STATUS.available : MODEL_STATUS.noKey
}

export function Models({ selected }: { selected?: string }) {
  const { models, runs, usage, defaultModel } = useApp((s) => ({
    models: s.models,
    runs: s.runs,
    usage: s.usage,
    defaultModel: s.meta?.defaultModel ?? '',
  }))
  const [confirming, setConfirming] = useState<Model | null>(null)
  const [filter, setFilter] = useState('')

  const inUse = (ref: string) => runs.filter((r) => r.live && r.model === ref).length
  // What `/` focuses, and what the status bar has been promising. A project can
  // declare a thousand models in .aigem/models.json; a table that long is not
  // read, it is searched.
  const needle = filter.trim().toLowerCase()
  const shown = needle
    ? models.filter((m) => `${m.name} ${m.ref}`.toLowerCase().includes(needle))
    : models

  const makeDefault = async (m: Model) => {
    setConfirming(null)
    try {
      await api.setDefaultModel(m.ref)
      flash(`${m.name} is the default model`)
      // The daemon announces this on the control stream, but the tab that asked
      // should not have to wait for its own change to come back around.
      await refresh.models()
    } catch (err) {
      setBanner(explain(err))
    }
  }

  const chosen = models.find((m) => m.ref === selected)
  const askDefault = useCallback((m: Model) => setConfirming(m), [])
  // The provider's quota, if it has reported one. It belongs beside the model
  // rather than on a screen of its own: what a person wants to know about a
  // limit is whether the model they are about to choose is near it.
  const quota = usage.find((u) => u.provider === chosen?.provider)

  // Memoised because it is the publish effect's dependency: a fresh object per
  // render would republish the panel on every keystroke elsewhere on the page.
  const panel = useMemo(() => {
    if (!chosen) return null
    const s = state(chosen)
    return {
      kind: 'model',
      id: chosen.ref,
      title: chosen.name,
      fields: [
        { key: 'provider', value: chosen.provider },
        { key: 'context', value: chosen.contextWindow ? compact(chosen.contextWindow) : '—' },
        { key: 'max output', value: chosen.maxTokens ? compact(chosen.maxTokens) : '—' },
        { key: 'reasoning', value: chosen.reasoning ? 'yes' : 'no' },
        { key: 'status', value: s.label, color: s.color },
        { key: 'default', value: chosen.default ? 'yes' : 'no' },
      ],
      listTitle: quota ? `${quota.provider} usage` : undefined,
      list: (quota?.windows ?? []).map((w) => ({
        icon: '·',
        text: w.name,
        meta: w.remaining || (w.usedPercent ? `${Math.round(w.usedPercent)}%` : ''),
      })),
      actions: [
        ...(chosen.default ? [] : [{ label: 'Make default', onClick: () => askDefault(chosen) }]),
        ...(chosen.needsAuth && !chosen.authenticated
          ? [{ label: `Sign in to ${chosen.provider}`, onClick: () => setLogin(chosen.provider) }]
          : []),
      ],
    }
  }, [chosen, quota, askDefault])
  usePublishInspector(panel)

  const columns: Column<Model>[] = [
    {
      key: 'model',
      header: 'Model',
      width: 'minmax(160px,1.4fr)',
      cell: (m) => (
        <span className="flex min-w-0 items-baseline gap-2">
          <span className="overflow-hidden text-[12.5px] font-medium text-ellipsis whitespace-nowrap">
            {m.name}
          </span>
          <span className="flex-none font-mono text-[10px] text-fg-subtle">{m.provider}</span>
          {m.default && (
            <span className="flex-none rounded-[3px] border border-line-strong px-[5px] py-px font-mono text-[9.5px] text-fg-muted">
              default
            </span>
          )}
        </span>
      ),
    },
    {
      key: 'status',
      header: 'Status',
      width: '96px',
      cell: (m) => <StatusChip status={state(m)} />,
    },
    {
      key: 'context',
      header: 'Context',
      width: '72px',
      cell: (m) => (
        <span className="font-mono text-[11px] text-fg-muted">
          {m.contextWindow ? compact(m.contextWindow) : '—'}
        </span>
      ),
    },
    {
      key: 'reason',
      header: 'Reason',
      width: '68px',
      cell: (m) => (
        <span className="font-mono text-[11px] text-fg-muted">{m.reasoning ? 'yes' : '—'}</span>
      ),
    },
    {
      key: 'used',
      header: 'In use by',
      width: '100px',
      cell: (m) => {
        const n = inUse(m.ref)
        return (
          <span className="font-mono text-[10.5px] text-fg-subtle">
            {n === 0 ? '—' : `${n} run${n > 1 ? 's' : ''}`}
          </span>
        )
      },
    },
    {
      // The canvas's last column is a price per million tokens. The daemon does
      // not carry pricing on this API and inventing a number for a screen a
      // person makes a choice on would be worse than the column being absent,
      // so the slot shows the output cap, which is on the wire.
      key: 'maxout',
      header: 'Max out',
      width: '72px',
      align: 'right',
      cell: (m) => (
        <span className="font-mono text-[11px] text-fg-subtle">
          {m.maxTokens ? compact(m.maxTokens) : '—'}
        </span>
      ),
    },
  ]

  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <h1 className="m-0 text-[16px] font-semibold tracking-[-0.015em]">Models</h1>
        <p className="mt-1 mb-0 text-[12px] text-fg-muted">
          The routing pool available to agents. Select a row to inspect it; the default is what a
          new session starts on{defaultModel ? ` — currently ${defaultModel}` : ''}.
        </p>
        <FilterInput value={filter} onChange={setFilter} label="Filter models" className="mt-[10px] w-[280px]" />
      </div>
      <DataGrid
        label="Models"
        columns={columns}
        rows={shown}
        rowKey={(m) => m.ref}
        minWidth={720}
        selected={(m) => m.ref === selected}
        onSelect={(m) => navigate({ screen: 'models', id: m.ref })}
        empty={
          needle ? (
            <EmptyState inline title={`No model matches “${filter.trim()}”.`} />
          ) : (
            <EmptyState
              title="No models are configured."
              detail="Add one to .aigem/models.json, or sign a provider in from the inspector."
            />
          )
        }
      />

      {confirming && (
        <Modal
          title="Make this the default model?"
          subtitle={confirming.ref}
          onClose={() => setConfirming(null)}
          confirm={{ label: 'Set as default', onClick: () => void makeDefault(confirming) }}
        >
          Every session started after this - in a browser or in a terminal - opens on{' '}
          {confirming.name} unless it names another. Conversations already running keep the model
          they started on.
        </Modal>
      )}

    </>
  )
}
