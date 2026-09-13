import { EmptyState } from '@/ui/EmptyState'

/**
 * The screens phase one does not build.
 *
 * They are reachable - the canvas puts Tickets and Worktrees in the navigation,
 * and a person who clicks one is owed an answer - and each says the same true
 * thing: the concept it is about arrives with tickets, which is the next phase.
 * Drawing the tables against fixtures would be worse: a screen that looks
 * finished and does nothing is harder to tell from a broken one.
 */

export function Tickets() {
  return (
    <Screen title="Tickets">
      <EmptyState
        title="Tickets need a project."
        detail="A ticket belongs to a repository inside a project. Tickets arrive in the next phase."
      />
    </Screen>
  )
}

export function Task() {
  return (
    <Screen title="Task">
      <EmptyState
        title="Tasks need a project."
        detail="Overview, discussion, changes and runs are drawn against a ticket, which arrives in the next phase."
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
