import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '@/lib/api'
import { navigate } from '@/lib/route'
import { describe, flash, refresh, setBanner, useApp } from '@/state/app'
import { usePublishInspector } from '@/state/inspector'
import type { Skill, SkillSummary } from '@/lib/wire'
import { EmptyState } from '@/ui/EmptyState'
import { Markdown } from '@/ui/Markdown'
import { Modal } from '@/ui/Modal'

/** Enabled / Pending, in the canvas's own vocabulary for this screen. */
function state(s: SkillSummary, pending: string[]): { label: string; icon: string; color: string } {
  if (pending.includes(s.name)) {
    return { label: 'Pending approval', icon: '◐', color: 'var(--warning)' }
  }
  return { label: 'Enabled', icon: '●', color: 'var(--success)' }
}

function scopeOf(s: SkillSummary): string {
  if (s.builtin) return 'built in'
  return s.projectLocal ? 'project' : 'user'
}

/**
 * The skill catalogue, and the one mutation phase one has for it.
 *
 * The canvas's "Pending modification" and "Proposed diff" are a skill the agent
 * proposed rewriting; what the daemon actually has is a project's skill set
 * awaiting the person's approval. Both are the same question asked in the same
 * place - "this changes what the agent can do; do you allow it?" - so this is
 * that block, filled with what the daemon knows: the names it would load and
 * the notices loading them produced.
 */
export function Skills() {
  const { skills, selected } = useApp((s) => ({
    skills: s.skills,
    selected: s.selection?.kind === 'skill' ? s.selection.id : '',
  }))
  const [asking, setAsking] = useState(false)
  const [busy, setBusy] = useState(false)

  const pending = useMemo(() => skills.pending?.names ?? [], [skills])
  const chosen = skills.items.find((s) => s.name === selected) ?? skills.items[0]
  const name = chosen?.name ?? ''

  // The body is keyed by the skill it belongs to and reset during render, so
  // the previous skill's instructions are never painted under a new heading.
  const [loaded, setLoaded] = useState<{ name: string; skill: Skill | null }>({
    name: '',
    skill: null,
  })
  if (loaded.name !== name) setLoaded({ name, skill: null })
  const detail = loaded.skill

  useEffect(() => {
    if (!name) return
    const abort = new AbortController()
    void api
      .skill(name, abort.signal)
      .then((skill) => setLoaded({ name, skill }))
      .catch((err: unknown) => {
        if (abort.signal.aborted) return
        setBanner(describe(err))
      })
    return () => abort.abort()
  }, [name])

  const trust = useCallback(async () => {
    setAsking(false)
    setBusy(true)
    try {
      const result = await api.trustSkills()
      flash(
        result.loaded.length === 1
          ? `Loaded ${result.loaded[0]}`
          : `Loaded ${result.loaded.length} project skills`,
      )
      for (const notice of result.notices) setBanner(notice)
      await refresh.skills()
    } catch (err) {
      setBanner(describe(err))
    } finally {
      setBusy(false)
    }
  }, [])

  const panel = useMemo(() => {
    if (!chosen) return null
    const s = state(chosen, pending)
    return {
      kind: 'skill',
      id: chosen.name,
      title: chosen.description || chosen.name,
      fields: [
        { key: 'scope', value: scopeOf(chosen) },
        { key: 'state', value: s.label, color: s.color },
        { key: 'invoked by', value: invokers(chosen) },
        ...(chosen.argumentHint ? [{ key: 'arguments', value: chosen.argumentHint }] : []),
        ...(detail?.model ? [{ key: 'model', value: detail.model }] : []),
        ...(detail?.agent ? [{ key: 'agent', value: detail.agent }] : []),
      ],
      listTitle: detail?.allowedTools.length ? 'Allowed tools' : undefined,
      list: detail?.allowedTools.map((t) => ({ icon: '·', text: t })) ?? [],
      actions: [{ label: 'Open in a session', onClick: () => navigate({ screen: 'chat' }) }],
    }
  }, [chosen, detail, pending])
  usePublishInspector(panel)

  return (
    <div className="flex min-h-0 flex-1">
      <div className="flex w-[244px] flex-none flex-col overflow-hidden border-r border-line">
        <div className="flex flex-none items-center gap-2 border-b border-line px-3 py-[11px]">
          <h2 className="m-0 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
            Skills
          </h2>
          <span className="ml-auto font-mono text-[10px] text-fg-subtle">
            {skills.items.length}
          </span>
        </div>
        <div role="listbox" aria-label="Skills" className="flex-1 overflow-y-auto py-1">
          {skills.items.map((s) => {
            const st = state(s, pending)
            const active = s.name === chosen?.name
            return (
              <div
                key={s.name}
                role="option"
                aria-selected={active}
                tabIndex={0}
                onClick={() => navigate({ screen: 'skills', id: s.name })}
                onKeyDown={(e) => {
                  if (e.key !== 'Enter' && e.key !== ' ') return
                  e.preventDefault()
                  navigate({ screen: 'skills', id: s.name })
                }}
                className={`cursor-default border-l-2 px-3 py-[7px] hover:bg-s0 focus-visible:bg-s0 focus-visible:outline-none ${
                  active ? 'border-l-primary bg-s0' : 'border-l-transparent'
                }`}
              >
                <div className="flex items-center gap-[7px]">
                  <span
                    className="font-mono text-[11.5px]"
                    style={{ color: active ? 'var(--fg)' : 'var(--fg-muted)' }}
                  >
                    {s.name}
                  </span>
                  <span
                    aria-hidden="true"
                    className="ml-auto text-[9px]"
                    style={{ color: st.color }}
                  >
                    {st.icon}
                  </span>
                  <span className="sr-only">{st.label}</span>
                </div>
                <div className="overflow-hidden text-[10.5px] text-ellipsis whitespace-nowrap text-fg-subtle">
                  {scopeOf(s)}
                </div>
              </div>
            )
          })}
          {skills.items.length === 0 && <EmptyState inline title="No skills are loaded." />}
        </div>
      </div>

      <div className="min-w-0 flex-1 overflow-y-auto">
        {chosen ? (
          <>
            <div className="border-b border-line px-[18px] pt-[14px] pb-3">
              <div className="flex items-center gap-[10px]">
                <h1 className="m-0 font-mono text-[14px] font-medium">{chosen.name}</h1>
                <span
                  className="flex items-center gap-[6px] text-[11.5px]"
                  style={{ color: state(chosen, pending).color }}
                >
                  <span aria-hidden="true" className="text-[10px]">
                    {state(chosen, pending).icon}
                  </span>
                  {state(chosen, pending).label}
                </span>
              </div>
              <p className="mt-[6px] mb-0 text-[12px] text-fg-muted">{chosen.description}</p>
              <div className="mt-[10px] flex gap-4 font-mono text-[10.5px] text-fg-subtle">
                <span>
                  scope <span className="text-fg-muted">{scopeOf(chosen)}</span>
                </span>
                <span>
                  invoked by <span className="text-fg-muted">{invokers(chosen)}</span>
                </span>
                {chosen.conditional && (
                  <span>
                    activation <span className="text-fg-muted">conditional</span>
                  </span>
                )}
              </div>
            </div>

            <div className="max-w-[88ch] px-[18px] pt-[14px] pb-8">
              {pending.length > 0 && (
                <>
                  <h2 className="text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
                    Pending approval
                  </h2>
                  <div className="mt-2 rounded-r-md border border-l-2 border-line border-l-attention bg-bg px-3 py-[10px]">
                    <div className="flex items-center gap-2 font-mono text-[10.5px] text-fg-subtle">
                      <span aria-hidden="true" className="text-attention">
                        !
                      </span>
                      <span>
                        this project defines {pending.length} skill
                        {pending.length > 1 ? 's' : ''} the daemon has not loaded
                      </span>
                    </div>
                    <ul className="mt-[6px] mb-0 list-none pl-0 font-mono text-[12px] text-fg">
                      {pending.map((n) => (
                        <li key={n}>{n}</li>
                      ))}
                    </ul>
                    {skills.pending?.invalidated && (
                      <p className="mt-2 mb-0 text-[11.5px] text-fg-subtle">
                        The definitions changed since they were last approved.
                      </p>
                    )}
                    <div className="mt-3 flex items-center gap-2">
                      <span className="text-[11.5px] text-fg-subtle">
                        Loading them lets the agent run what this project's files say.
                      </span>
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => setAsking(true)}
                        className="ml-auto h-[28px] cursor-pointer rounded-md border border-primary bg-primary px-3 text-[12px] font-medium text-bg hover:brightness-110 disabled:opacity-50"
                      >
                        Review and load
                      </button>
                    </div>
                  </div>
                </>
              )}

              <h2 className="mt-5 mb-2 text-[10px] font-semibold tracking-[.07em] text-fg-subtle uppercase">
                Instructions
              </h2>
              {detail ? (
                <>
                  <Markdown source={detail.body} />
                  {detail.bodyTruncated && (
                    <p className="mt-3 text-[11.5px] text-fg-subtle">
                      This skill is longer than the API will send; the rest is on disk.
                    </p>
                  )}
                </>
              ) : (
                <p className="text-[12px] text-fg-subtle">Reading the skill…</p>
              )}
            </div>
          </>
        ) : (
          <EmptyState
            title="No skills are loaded."
            detail="A skill is a folder with a SKILL.md in it, under this project or your user directory."
          />
        )}
      </div>

      {asking && (
        <Modal
          title="Load this project's skills?"
          subtitle={`${pending.length} definition${pending.length > 1 ? 's' : ''}`}
          onClose={() => setAsking(false)}
          confirm={{ label: 'Load them', onClick: () => void trust() }}
          width={520}
        >
          A skill is instructions and a tool policy that this project's files decide. Loading them
          changes what the agent will do on its own, in every conversation this daemon is holding -
          including any a terminal has open. Approve them only if you trust this checkout.
        </Modal>
      )}
    </div>
  )
}

function invokers(s: SkillSummary): string {
  const who = [s.userInvocable && 'you', s.modelInvocable && 'the model'].filter(Boolean)
  return who.length > 0 ? who.join(' and ') : 'nothing'
}
