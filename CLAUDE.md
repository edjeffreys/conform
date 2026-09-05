# conform — project context

A declarative media transcoder. See `README.md` for what it does and why; this
file is about working in the repo.

## Invariants

These are load-bearing. Breaking one does not fail a test — it quietly costs
the property that makes the tool worth using.

- **`plan.Build` performs no I/O.** It is a pure function of a `media.File` and
  a `config.Profile`. That is what makes `conform plan` a trustworthy preview of
  `apply`, and what will let a distributed worker re-derive a plan and reach the
  same answer as the orchestrator without a task description being serialised
  between them.
- **Rules are predicates on what is acceptable, never instructions to act.**
  `codecs: [hevc]` means "hevc is fine as-is", so the output of a transcode
  satisfies the rule that triggered it. This is the whole reason repeated runs
  converge; a rule phrased as an action would loop forever.
- **Nothing replaces a file until the output has been re-planned and come back
  `none`.** Along with the probe and duration checks in `run.verify`, that is
  direct proof the next pass will leave the file alone. Do not add a fast path
  around it.
- **The original is never opened for writing.** Encode elsewhere, verify, then
  rename into place.
- **There is no job database.** The cache and excuse records are disposable —
  deleting them costs a re-probe, never correctness. Excuses key on size and
  mtime so a replaced file is judged fresh. Anything that would make this state
  authoritative belongs somewhere else.
- **Any UI observes and triggers; it never authors.** Desired state lives in
  git. An editable profile in a web form rebuilds the problem conform exists to
  solve.

## Settled architecture

Decided, so it does not need re-deriving. Not all of it is built yet.

- **Kubernetes is the queue.** An orchestrator plans the library and creates one
  Job per non-conformant file; the scheduler places it. No custom queue, no
  broker.
- **Placement is data.** A profile names a `core/v1 PodTemplate`; conform copies
  it, sets the container args, and wraps it in a Job. conform never learns what
  a GPU is, so it works against any device plugin.
- **Exit 0 whenever a verdict was reached**, even when the verdict is "this file
  cannot be processed". Non-zero is reserved for faults. Job `backoffLimit` then
  retries infrastructure failures and the excuse ledger owns media failures,
  without the two multiplying.
- **State is split by writer**: the probe cache has a single owner, and excuses
  are per-file sidecars written only by the worker that owns that file. Two
  processes must never write the same state file.

## Conventions

**Comments explain why, not what — 2 to 4 lines, the non-obvious fact only.**
State the constraint the next reader could not get from the code, then stop. No
error transcripts, no failure chains, no restating the line below. A comment
that repeats its own line is noise crowding out the ones that carry
information.

**Conventional Commits**, for commits and PR titles alike: `<type>(<scope>):
<subject>`, imperative mood, lower case after the colon. Types: `feat`, `fix`,
`chore`, `docs`, `refactor`, `ci`, `test`. Scope is the package —
`fix(run)`, `feat(orchestrate)`, `refactor(state)` — or `ci`, `docs`, `deps`.

## Layout

```
cmd/conform/     the binary; one file per subcommand's plumbing
internal/
  config/        desired state — profiles, libraries, execution settings
  media/         observed state — ffprobe, normalised into media.File
  plan/          the diff, and rendering it as ffmpeg arguments
  run/           executing one plan: encode, verify, replace
  scan/          walking a library
  state/         probe cache and excuse records
```

## Testing

`go test ./...`. `config`, `plan` and `state` have tests; `run` and `media` do
not, because both shell out. Prefer growing the table tests in
`plan/plan_test.go` — the planner is where correctness actually lives, and it is
pure, so it is cheap to test exhaustively.
