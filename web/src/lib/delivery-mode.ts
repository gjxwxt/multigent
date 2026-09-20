// Renders the platform's own answer about remote dependencies. There is
// deliberately no step-ID or field-name heuristic here: a custom workflow that
// renames `create_pr` must not be silently classified either way, because a
// warning that guesses wrong in the "looks fine" direction is worse than no
// warning. Everything below comes from two backend facts — the definition's
// `config.requires_remote` markers and the derived `remoteBindingVerified`.

export type WorkflowStepLike = {
  id: string
  title?: string
  config?: Record<string, string>
}

export type ProjectRemoteState = {
  /** JSON null = the platform could not read the binding table. Not the same as false. */
  remoteBindingVerified?: boolean | null
  remoteBindingNamespace?: string
  /** Project-level declaration: "required" | "local" | "" (falls back to server default). */
  remotePipelineRequired?: string
}

export type DeliveryNotice =
  | { state: 'none' }
  | { state: 'undeclared'; tone: 'neutral' }
  | { state: 'ready'; tone: 'green'; steps: WorkflowStepLike[]; namespace: string }
  | { state: 'localBranch'; tone: 'amber'; steps: WorkflowStepLike[] }
  | { state: 'requiredUnbound'; tone: 'red'; steps: WorkflowStepLike[] }
  | { state: 'bindingUnknown'; tone: 'neutral'; steps: WorkflowStepLike[] }

export function remoteDependentSteps(steps?: WorkflowStepLike[]): WorkflowStepLike[] {
  return (steps ?? []).filter((step) => String(step.config?.requires_remote ?? '').toLowerCase().trim() === 'true')
}

export function evaluateDeliveryNotice(
  steps: WorkflowStepLike[] | undefined,
  project: ProjectRemoteState | undefined,
): DeliveryNotice {
  const dependent = remoteDependentSteps(steps)
  if (dependent.length === 0) {
    return { state: 'undeclared', tone: 'neutral' }
  }
  if (!project || project.remoteBindingVerified === null || project.remoteBindingVerified === undefined) {
    return { state: 'bindingUnknown', tone: 'neutral', steps: dependent }
  }
  if (project.remoteBindingVerified === true) {
    return { state: 'ready', tone: 'green', steps: dependent, namespace: project.remoteBindingNamespace ?? '' }
  }
  // Unbound. Whether that degrades delivery or blocks it is the project's own
  // declaration, never this screen's opinion.
  if (String(project.remotePipelineRequired ?? '').toLowerCase().trim() === 'required') {
    return { state: 'requiredUnbound', tone: 'red', steps: dependent }
  }
  return { state: 'localBranch', tone: 'amber', steps: dependent }
}
