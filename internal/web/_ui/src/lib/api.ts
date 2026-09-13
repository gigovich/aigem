/**
 * The HTTP half of the daemon's API.
 *
 * Every mutation goes through here, because that is where a status code and an
 * error body mean something; the sockets carry the conversation and the change
 * notifications. One function per route, named for the route, so a screen never
 * builds a URL and a route that moves is one edit.
 */

import type {
  Activity,
  Artifact,
  Command,
  Login,
  Meta,
  Model,
  NewProject,
  NewRun,
  Project,
  ProviderUsage,
  Repository,
  Run,
  RunEvent,
  Skill,
  SkillApproval,
  Skills,
} from './wire'

/**
 * ApiError carries what the daemon answered, because the status is the thing a
 * caller acts on: 409 means the run has no session, 410 means reload rather
 * than retry, 503 means try again once something is closed.
 *
 * `detail` is the daemon's own text and is only ever rendered as text. A 400
 * from this API is written for a person and may name a model or a path on the
 * machine; a 500 carries nothing, and this leaves it empty rather than
 * inventing a sentence.
 */
export class ApiError extends Error {
  readonly status: number
  readonly detail: string
  readonly retryAfter?: number

  constructor(status: number, detail: string, retryAfter?: number) {
    super(detail || `request failed: ${status}`)
    this.name = 'ApiError'
    this.status = status
    this.detail = detail
    this.retryAfter = retryAfter
  }

  /** Whether closing something and asking again is the fix. */
  get busy(): boolean {
    return this.status === 503
  }
}

/** The whole API is same-origin, so a path is the whole of a request's address. */
async function send(path: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(path, init)
  if (res.ok) return res
  // Read once: a body is at most a sentence, and leaving it unread on an error
  // keeps the connection from being reused.
  const detail = (await res.text().catch(() => '')).trim()
  const after = Number(res.headers.get('Retry-After'))
  throw new ApiError(res.status, detail, Number.isFinite(after) && after > 0 ? after : undefined)
}

async function json<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await send(path, init)
  return (await res.json()) as T
}

function body(value: unknown): RequestInit {
  return {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(value),
  }
}

/** A cursor on this API is a non-negative whole number and nothing else. */
function query(params: Record<string, number | string | undefined>): string {
  const search = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '') continue
    search.set(k, String(v))
  }
  const s = search.toString()
  return s ? `?${s}` : ''
}

export const api = {
  meta: (signal?: AbortSignal) => json<Meta>('/api/meta', { signal }),

  runs: (signal?: AbortSignal) => json<Run[]>('/api/runs', { signal }),
  openRun: (req: NewRun, signal?: AbortSignal) =>
    json<Run>('/api/runs', { ...body(req), signal }),
  run: (id: string, signal?: AbortSignal) =>
    json<Run>(`/api/runs/${encodeURIComponent(id)}`, { signal }),
  closeRun: async (id: string, signal?: AbortSignal) => {
    await send(`/api/runs/${encodeURIComponent(id)}`, { method: 'DELETE', signal })
  },
  runEvents: (id: string, since = 0, limit = 0, signal?: AbortSignal) =>
    json<RunEvent[]>(
      `/api/runs/${encodeURIComponent(id)}/events${query({ since, limit: limit || undefined })}`,
      { signal },
    ),
  runArtifacts: (id: string, signal?: AbortSignal) =>
    json<Artifact[]>(`/api/runs/${encodeURIComponent(id)}/artifacts`, { signal }),
  /** The one route whose success body is text: a tool's own output, verbatim. */
  runBlob: async (id: string, seq: number, signal?: AbortSignal) => {
    const res = await send(`/api/runs/${encodeURIComponent(id)}/blobs/${seq}`, { signal })
    return await res.text()
  },

  projects: (signal?: AbortSignal) => json<Project[]>('/api/projects', { signal }),
  addProject: (req: NewProject, signal?: AbortSignal) =>
    json<Project>('/api/projects', { ...body(req), signal }),
  removeProject: async (id: string, signal?: AbortSignal) => {
    await send(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE', signal })
  },
  projectRepos: (id: string, signal?: AbortSignal) =>
    json<Repository[]>(`/api/projects/${encodeURIComponent(id)}/repos`, { signal }),

  models: (signal?: AbortSignal) => json<Model[]>('/api/models', { signal }),
  setDefaultModel: (ref: string, signal?: AbortSignal) =>
    json<Model>('/api/models/default', { ...body({ ref }), signal }),

  signOut: async (signal?: AbortSignal) => {
    await send('/api/auth/session', { method: 'DELETE', signal })
  },
  beginLogin: (provider: string, signal?: AbortSignal) =>
    json<Login>('/api/auth/login', { ...body({ provider }), signal }),
  login: (id: string, signal?: AbortSignal) =>
    json<Login>(`/api/auth/login/${encodeURIComponent(id)}`, { signal }),
  // The pasted callback is the request body itself, not a field in a JSON
  // document: the daemon reads it with io.ReadAll and hands the string to the
  // provider. Wrapping it in `{"value":...}` would paste the JSON.
  pasteLogin: (id: string, value: string, signal?: AbortSignal) =>
    json<Login>(`/api/auth/login/${encodeURIComponent(id)}/paste`, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain' },
      body: value,
      signal,
    }),
  cancelLogin: async (id: string, signal?: AbortSignal) => {
    await send(`/api/auth/login/${encodeURIComponent(id)}`, { method: 'DELETE', signal })
  },

  skills: (project = '', signal?: AbortSignal) =>
    json<Skills>(`/api/skills${query({ project })}`, { signal }),
  skill: (name: string, project = '', signal?: AbortSignal) =>
    json<Skill>(`/api/skills/${encodeURIComponent(name)}${query({ project })}`, { signal }),
  trustSkills: (project = '', signal?: AbortSignal) =>
    json<SkillApproval>('/api/skills/trust', { ...body(project ? { project } : {}), signal }),

  commands: (project = '', signal?: AbortSignal) =>
    json<Command[]>(`/api/commands${query({ project })}`, { signal }),
  usage: (signal?: AbortSignal) => json<ProviderUsage[]>('/api/usage', { signal }),
  activity: (since = 0, limit = 0, signal?: AbortSignal) =>
    json<Activity[]>(`/api/activity${query({ since, limit: limit || undefined })}`, { signal }),
}

/**
 * The websocket address for a same-origin path.
 *
 * The scheme has to follow the page's: a daemon behind an https proxy serves
 * this page over TLS, and a `ws://` socket from it is blocked as mixed content
 * with nothing in the console but a security error.
 */
export function socketURL(path: string, params?: Record<string, number | string | undefined>): string {
  const url = new URL(path + (params ? query(params) : ''), window.location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return url.toString()
}
