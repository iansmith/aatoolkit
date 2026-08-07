# aatoolkit

aatoolkit is an open, reusable engine for building voice/telephony agents: Twilio media
streaming, STT + a VAD/turn state machine, an LLM driver, a fact-extraction toolkit, a
generic runtime Go-policy loader, and a process supervisor. It is the *mechanism*; a
specific agent supplies its *meaning* (prompts, policy, ontology, identity) by injecting it
through `driver.Config` + `interp.Load` + the `host.Host` interface. Any number of agents can
run on the same engine.


## The one hard rule — standalone

**aatoolkit is a standalone module: it must not depend on anything outside its own
module.** Dependencies flow consumer → aatoolkit only, never the reverse — a consuming
agent injects its meaning through `driver.Config` + `interp.Load` + `host.Host`, but the
engine never imports or embeds the consumer. Verify the engine still stands alone with
`GOWORK=off go build ./...` and `GOWORK=off go test ./...` before committing. Keep the
engine generic: mechanism here, meaning in the consumer.

## Development rules

### 1. Pre-commit
- Run `gofmt`, the build, and the targeted tests for the area you touched before committing.
  Run the full suite only when touching shared/cross-cutting code.
- Commit, then push — only after the above are clean.

### 2. Tests
- **Tests-first for new behavior and for fixes.** For new behavior, write the test describing
  the contract and confirm it's red for the right reason before implementing. For a bug fix,
  write a test that reproduces it — red before, green after. Trivial tweaks, copy changes, and
  pure refactors are exempt.
- **A failing test is signal, not chore.** Investigate the root cause; never delete a test,
  narrow an assertion, `Skip()`, or cite an unverified "flake" to silence it.

### 3. Git
- **Never squash-merge or rebase-merge.** Use a real merge commit — squash and rebase lose
  fixup context and break `git bisect`.
- Always name the branch explicitly in `git push origin <branch>`.
- Never `--force`, `reset --hard`, `--no-verify`, or admin-merge unless explicitly asked. When
  a hook or check fails, fix the underlying issue rather than bypassing it.
- Create new commits rather than amending (except amending one fresh commit on a solo branch
  before anyone has pulled it).

### 4. Refactoring scope
- **Dedupe is in scope.** If you find 2+ near-identical code paths while working on a change,
  extract the helper and migrate the duplicates in the same PR.
- **Structural changes are out of scope without discussion** — renaming exported symbols,
  altering public signatures, moving files, or reshaping package boundaries.
- When extending an existing system, study its types and patterns first; mirror the existing
  vocabulary rather than inventing parallel terms.
- Foundational correctness over quick wins. "Nearly passing" is failing — don't declare a
  category of failures done by cherry-picking the easy cases.

### 5. Source of truth
- **One definition per value.** No duplicate constants, aliases, or parallel names. If
  something needs renaming, update every reference — never add an alias.
- Never edit generated files by hand. Edit the source and regenerate.

### 6. Agents and worktrees
- When running coding agents in parallel, commit and push before launching worktree agents
  (worktrees start from HEAD, not the working directory). Aim for milestones frequent enough
  that progress is visible but not noisy, and for parallelism that won't cause merge-back
  conflicts on the base branch.
- Every agent runs on its own branch in its own directory, commits only to that branch, and
  reports at each milestone. Restate the relevant rules in each agent prompt — agents start
  with no prior context.
- Never use `open` to display files unless explicitly asked (disruptive).

### 7. Environment
- Never modify `PATH` manually. If the project has special path/environment requirements, ask
  first. The shell env for local dev is sourced from `enable-aatoolkit` (see
  `enable-aatoolkit.example`); the real file holds secrets and is gitignored — keep it out of
  the repo (best kept one directory up, at the workspace root).

### 8. Ticket and PR text obey the same boundary as the code
- The pre-commit leak guard keeps this repo's tree free of the closed consuming project's
  identity, secrets, and content. **Ticket and pull-request text — titles, descriptions, and
  comments — is held to the same standard.**
- The two surfaces are exposed differently, and the ticket one is the easier to misjudge. A PR
  is public the moment it is posted. The tracker is private, so the exposure there is
  *indirect*: ticket prose gets lifted into commit messages, PR bodies, and doc comments as
  work proceeds, and those are as public as the code. A leak written once in a ticket tends to
  arrive in the repo later, quoted by someone who trusted the source.
- Do not name the closed project, its ticket prefix, its file paths, its symbol names, or its
  data (phone numbers, prompts, real message text). Say what the engine actually sees: "the
  consuming server", "a depending project", "the operator".
- Where a change is needed on the consumer's side, describe it generically and leave it to be
  tracked wherever that project tracks its work. Never cross-reference its tickets from one of
  ours. The reverse direction is fine — the closed side may reference these freely.
- Evidence from a manual end-to-end run is the usual trap: real numbers and real message text
  belong in local notes, redacted before they reach a PR body.
- No hook reaches the tracker or a PR body, so this rule is unenforced by construction. Scan
  prose for the same tokens the guard scans a diff for, before saving.

### 9. Documentation layout
- `docs/` is **gitignored** — personal notes, scratch, drafts. Not committed.
- `design/` is **tracked**, but don't add files to it without explicit confirmation — design
  docs are deliberate artifacts.

## Local development

aatoolkit is developed alongside its consumer(s) via a `go.work` workspace one directory up
(so cross-module edits resolve from local disk); `go.work` is not committed. CI and the
consumer's release builds use the pinned module version, so aatoolkit must always build and
test **standalone** — verify with `GOWORK=off go build ./... && GOWORK=off go test ./...`
before relying on a change.
