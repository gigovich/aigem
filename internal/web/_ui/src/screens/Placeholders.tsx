import { EmptyState } from '@/ui/EmptyState'

/**
 * The screens phase one does not build.
 *
 * They are reachable - the canvas puts Tickets and Worktrees in the navigation,
 * and a person who clicks one is owed an answer - and each says the same true
 * thing: the concept it is about arrives with projects, which is phase two.
 * Drawing the tables against fixtures would be worse: a screen that looks
 * finished and does nothing is harder to tell from a broken one.
 */

export function Tickets() {
  return (
    <Screen title="Tickets">
      <EmptyState
        title="Tickets need a project."
        detail="A ticket belongs to a repository inside a project, and this daemon works in the one directory it was started in. Projects, repositories and their tickets arrive in a later phase."
      />
    </Screen>
  )
}

export function Task() {
  return (
    <Screen title="Task">
      <EmptyState
        title="Tasks need a project."
        detail="Overview, discussion, changes and runs are drawn against a ticket, and tickets arrive with projects in a later phase."
      />
    </Screen>
  )
}

export function Repos() {
  return (
    <Screen title="Repositories & worktrees">
      <EmptyState
        title="Worktrees need a project."
        detail="Every autonomous run gets an isolated worktree under the project directory. Until there are projects, a conversation works in the directory the daemon was started in."
      />
    </Screen>
  )
}

function Screen({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <>
      <div className="flex-none border-b border-line px-[18px] pt-[14px] pb-3">
        <h1 className="m-0 text-[16px] font-semibold tracking-[-0.015em]">{title}</h1>
      </div>
      <div className="flex-1 overflow-y-auto">{children}</div>
    </>
  )
}
