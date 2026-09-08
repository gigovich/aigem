/**
 * The wire, as the daemon writes it.
 *
 * Every shape here is a Go struct's JSON in `internal/web` or an event in
 * `internal/uisession`, and nothing in this file is a decision: a field that is
 * optional in Go is optional here, and a name is spelled the way the daemon
 * spells it. Anything the interface decides - a label, a glyph, a colour -
 * lives below the wire types, keyed off them.
 */

/** Meta is `GET /api/meta`, and the `hello` frame's payload byte for byte. */
export type Meta = {
  version: string
  defaultModel: string
  rev: number
  ui: boolean
  /**
   * What this daemon serves. Partial rather than a full record: an absent key
   * means unsupported, which is the daemon's own rule, and typing it as `Feature`
   * is what stops a misspelt name from compiling into a screen that silently
   * never appears.
   */
  features: Partial<Record<Feature, boolean>>
}

/** Feature names the daemon publishes. An absent key means unsupported. */
export type Feature =
  | 'controlSocket'
  | 'runs'
  | 'models'
  | 'providerLogin'
  | 'skills'
  | 'commands'
  | 'usage'
  | 'activity'

export type Run = {
  id: string
  sessionId?: string
  mode: string
  title?: string
  model?: string
  root?: string
  status: string
  created: string
  updated: string
  /** Always present: it is what a page reads before it opens a socket. */
  live: boolean
  running?: boolean
  waiting?: boolean
  step?: boolean
  seq?: number
}

export type NewRun = { mode?: string; title?: string; model?: string }

export type Model = {
  ref: string
  provider: string
  name: string
  contextWindow?: number
  maxTokens?: number
  reasoning?: boolean
  needsAuth: boolean
  authenticated: boolean
  default: boolean
}

export type Login = {
  id: string
  provider: string
  url: string
  code?: string
  acceptsPaste?: boolean
  state: 'pending' | 'done' | 'failed' | 'cancelled'
  error?: string
}

export type SkillSummary = {
  name: string
  description: string
  projectLocal?: boolean
  builtin?: boolean
  userInvocable: boolean
  modelInvocable: boolean
  conditional?: boolean
  argumentHint?: string
}

export type Skill = SkillSummary & {
  whenToUse?: string
  allowedTools: string[]
  disallowedTools: string[]
  model?: string
  effort?: string
  context?: string
  agent?: string
  paths: string[]
  body: string
  bodyTruncated?: boolean
}

export type PendingSkills = { names: string[]; invalidated?: boolean }
export type Skills = { items: SkillSummary[]; pending?: PendingSkills }
export type SkillApproval = { loaded: string[]; notices: string[] }

export type Command = { name: string; description: string }

export type LimitWindow = {
  name: string
  usedPercent?: number
  windowMinutes?: number
  resetAt?: string
  remaining?: string
}

export type ProviderUsage = {
  provider: string
  model?: string
  plan?: string
  credits?: string
  windows: LimitWindow[]
  observedAt?: string
}

export type Activity = {
  seq?: number
  at?: string
  kind: string
  text: string
  runRef?: string
}

export type Artifact = {
  path: string
  old?: string
  new?: string
  created?: boolean
  oldBytes?: number
  newBytes?: number
  truncated?: boolean
}

/**
 * The session's event vocabulary, from `internal/uisession/event.go`.
 *
 * It is duplicated here because a browser cannot import Go, and the duplicate
 * is the thing most likely to drift: a kind renamed on one side renders as a
 * conversation that did not happen rather than as a compile error. The Go test
 * `TestTheBrowsersEventVocabularyMatchesThisPackages` reads this object and
 * fails when the two sets differ, which is what makes writing it down safe.
 */
export const EventKind = {
  UserMessage: 'user_message',
  TurnStart: 'turn_start',
  TurnEnd: 'turn_end',
  Content: 'content',
  Reasoning: 'reasoning',
  AssistantMessage: 'assistant_message',
  ToolBatch: 'tool_batch',
  ToolStart: 'tool_start',
  ToolEnd: 'tool_end',
  AgentStart: 'agent_start',
  AgentEnd: 'agent_end',
  SubToolStart: 'sub_tool_start',
  SubToolEnd: 'sub_tool_end',
  SubNotice: 'sub_notice',
  Notice: 'notice',
  Error: 'error',
  Usage: 'usage',
  Todo: 'todo',
  BudgetExhausted: 'budget_exhausted',
  FileChanged: 'file_changed',
  ApprovalRequest: 'approval_request',
  ApprovalResolved: 'approval_resolved',
  SessionMeta: 'session_meta',
  Presence: 'presence',
  Desync: 'desync',
} as const

export type EventKind = (typeof EventKind)[keyof typeof EventKind]

export type Decision = 'once' | 'always' | 'deny'

export type ApprovalOption = { value: Decision; label: string }

export type Approval = {
  kind: 'tool' | 'path'
  tool: string
  args?: unknown
  path?: string
  write?: boolean
  options: ApprovalOption[]
}

export type TodoItem = { text: string; status?: string }

export type Call = { id: string; name: string }

export type PresenceClient = { id: string; kind?: string; label?: string }

/** RunEvent is one step of a conversation, flat the way the session writes it. */
export type RunEvent = {
  seq: number
  time: string
  kind: EventKind
  id?: string
  run_id?: string
  agent?: string
  name?: string
  text?: string
  args?: unknown
  error?: string
  bytes?: number
  blob?: boolean
  ctx?: number
  round?: number
  calls?: Call[]
  tokens?: number
  todos?: TodoItem[]
  images?: number
  injected?: boolean
  interrupted?: boolean
  path?: string
  created?: boolean
  approval?: Approval
  decision?: Decision
  by?: string
  clients?: PresenceClient[]
  from?: number
}

/** The operations a client may send up a run socket. `stop` is phase 3. */
export type RunOp =
  | { op: 'submit'; text: string; images?: { media_type: string; data: string }[] }
  | { op: 'interrupt' }
  | { op: 'resolve'; id: string; decision: Decision; label?: string }
  | { op: 'command'; name: string; args?: string }
  | { op: 'step_mode'; on: boolean }
  | { op: 'switch_model'; ref: string; persist?: boolean }
  | { op: 'ping' }

/**
 * The refusal frame, shared by both sockets. It is deliberately not called
 * "error": that is a real event kind in a conversation, and a client mistake
 * must not be drawn as something that happened in the run.
 */
export const CLIENT_ERROR = 'client_error'

export type ClientError = { kind: typeof CLIENT_ERROR; op?: string; error: string }

/** Control frames. `hello` first, then one per published mutation. */
export type ControlFrame =
  | { type: 'hello'; rev: number; data: Meta }
  | { type: typeof CLIENT_ERROR; rev: number; data?: { op?: string; error: string } }
  | { type: ControlKind; rev: number; data?: unknown }

/** The kinds `cmd/aigem` publishes. A page refetches the collection each names. */
export const ControlKind = {
  RunUpdated: 'run.updated',
  ModelDefault: 'model.default',
  AuthUpdated: 'auth.updated',
  SkillsUpdated: 'skills.updated',
  ActivityUpdated: 'activity.updated',
} as const

export type ControlKind = (typeof ControlKind)[keyof typeof ControlKind]

/**
 * The status dictionary, from the design canvas's `S`. Label, glyph and colour
 * come from here and are never invented at the call site, so a status reads the
 * same on every screen.
 *
 * The glyph is not decoration: it is what keeps colour from being the only
 * carrier of the state.
 */
export type StatusKey =
  | 'running'
  | 'waiting'
  | 'attention'
  | 'failed'
  | 'completed'
  | 'stopped'
  | 'blocked'
  | 'planned'
  | 'progress'

export type StatusInfo = { label: string; icon: string; color: string }

export const STATUS: Record<StatusKey, StatusInfo> = {
  running: { label: 'Running', icon: '●', color: 'var(--running)' },
  waiting: { label: 'Waiting', icon: '◐', color: 'var(--warning)' },
  attention: { label: 'Needs attention', icon: '!', color: 'var(--attention)' },
  failed: { label: 'Failed', icon: '×', color: 'var(--danger)' },
  completed: { label: 'Completed', icon: '✓', color: 'var(--success)' },
  stopped: { label: 'Stopped', icon: '■', color: 'var(--fg-subtle)' },
  blocked: { label: 'Blocked', icon: '◇', color: 'var(--danger)' },
  planned: { label: 'Planned', icon: '○', color: 'var(--fg-subtle)' },
  progress: { label: 'In progress', icon: '◑', color: 'var(--primary)' },
}

/**
 * A run record's state as one of the dictionary's keys.
 *
 * `waiting` on the wire means "parked on an approval nobody has answered",
 * which is the design's `attention` and not its `waiting` - the latter is an
 * open session with nothing in flight, which is what a person sees between
 * turns.
 */
export function runStatus(run: Run): StatusKey {
  if (!run.live) return 'stopped'
  if (run.waiting) return 'attention'
  if (run.running) return 'running'
  return 'waiting'
}
