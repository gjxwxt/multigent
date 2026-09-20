---
name: git-pr-delivery
description: Standard operating procedure for pushing task branches, generating changelogs, creating or updating Pull Requests, and preparing delivery evidence for human review.
---

# Skill: Git Branch & Pull Request Delivery (git-pr-delivery)

Use this skill when executing `changelog_cleanup`, `create_pr`, `push_to_remote`, or branch delivery milestones in Multigent workflows.

---

## 1. Core Operating Principles

1. **Work in Current Task Branch**:
   - Always query the actual branch name with `git rev-parse --abbrev-ref HEAD`.
   - **DO NOT invent or rename branches** in summary text or outputs — strictly use the exact branch name returned by `git rev-parse --abbrev-ref HEAD`.
   - Ensure all code changes, tests, and documentation are cleanly committed in the worktree.

2. **Autonomous Diff & Changelog Extraction**:
   - You do NOT need previous steps to pass text dumps of code diffs. Inspect Git directly against the base branch (default `main`):
     ```bash
     git log main..HEAD --oneline
     git diff --stat main..HEAD
     ```
   - Summarize:
     - **Core Feature / Fix**: 1-2 sentence description of what was delivered.
     - **Changed Components**: Key files modified and why.
     - **Test Evidence**: Single test summary (e.g. `39/39 PASS`, build status, smoke test results).
     - **Compatibility & Risks**: Backward-compatible notes or migrations.

---

## 2. PR Creation & Environment Adaptability

### Scenario A: Remote Git Platform Available (GitHub / GitLab / Enterprise Remote)
- If `git remote -v` has an active upstream or a Multigent GitHub/GitLab connection is configured:
  1. Push the task branch:
     ```bash
     git push -u origin HEAD
     ```
  2. If `gh` CLI or GitLab API is authenticated, create or update the PR:
     ```bash
     gh pr create --title "..." --body "..." --base main || gh pr view --web
     ```
  3. Capture the output `pr_url` (e.g. `https://gitlab.example.com/org/repo/-/merge_requests/123`) and `pr_number`.

### Scenario B: Local-Branch Delivery (no verified remote binding)
- **GATE THIS ON THE PROMPT, NOT ON CONVENIENCE.** The task prompt carries a `## Delivery mode` section with the platform's reading of the verified remote binding. Use this scenario **only** when it says ``Mode: `local_branch` ``. If it says `remote_bound`, you must produce a real merge request — the placeholder below is not an escape hatch. If it says `unknown`, do not assume: attempt the remote action once and report an environment blocker if it fails for a binding/credential reason.
- **Pushing still works and is still required.** Git push authenticates from the environment at the push boundary; it does not depend on the verified binding. Publishing the task branch is the delivery here — skipping the push destroys the work instead of degrading it.
- Artifacts for this mode:
  - `pr_url`: Set to `branch: <branch_name>` (e.g. `branch: feature/t-20260827-xxx`) or the internal repository branch link.
  - `pr_number`: Set to the current task ID or branch sequence (e.g. `t-20260827-xxx`).
  - `pr_diff_summary`: **Start with an explicit notice** that this is a task-branch handoff and **not a real merge request**, so nobody downstream reads it as review evidence that exists. Then give the full structured markdown summary from `git log` and `git diff --stat`.
  - `preview_url`: If a web preview container is available, use `/preview/<taskId>/`; otherwise `none`.
- **STRICT RULE**: **DO NOT** hang on interactive `gh auth login`, **DO NOT** fail the step, and **DO NOT** attempt to fake external network connections.
- What this mode genuinely cannot produce: a real merge request, pipeline evidence for HEAD, and the platform-side MR record. Say so in the output instead of substituting a value that looks like one.

---

## 3. Submitting the Step Output

Complete the step deterministically with `mga task step done`:

```bash
mga task step done --id <task-id> --status success \
  --summary "Pull request and branch delivery evidence prepared" \
  --output-json '{
    "pr_url": "branch: feature/t-...",
    "pr_number": "t-...",
    "pr_diff_summary": "## Summary\n- Added keyword search...",
    "preview_url": "/preview/<task-id>/"
  }'
```

---

## 4. Key Prohibitions (Anti-patterns)

- ❌ **DO NOT** loop through interactive login prompts (`gh auth login`).
- ❌ **DO NOT** invent custom branch names in text outputs; use `$(git rev-parse --abbrev-ref HEAD)`.
- ❌ **DO NOT** rebuild or recreate files outside the task git branch.
- ❌ **DO NOT** leave uncommitted modified files in the working directory before submitting the step.
- ❌ **DO NOT** omit required output fields (`pr_url`, `pr_number`, `pr_diff_summary`, `preview_url`).
