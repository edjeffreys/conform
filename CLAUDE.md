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
- **Only a failure ffmpeg reached the media to produce may spend a file's
  error budget.** Anything that fails before that — no device, no room, a
  read-only mount — is a fault: it exits non-zero and is retried. An excuse
  keys on the file, so a worker's fault recorded against one would excuse a
  library that was never the problem.
- **Any UI observes and triggers; it never authors.** Desired state lives in
  git. An editable profile in a web form rebuilds the problem conform exists to
  solve.

## Settled architecture

Decided, so it does not need re-deriving. Not all of it is built yet.

- **Kubernetes is the queue.** An orchestrator plans the library and creates one
  Job per non-conformant file; the scheduler places it. No custom queue, no
  broker.
- **Placement is data.** A profile names a `core/v1 PodTemplate`; conform copies
  it, sets the container args, and wraps it in a Job. conform never decides
  where it runs, so it works against any device plugin. A worker may discover
  what ffmpeg can do in its own container — that is how an encoder preset
  resolves — but only in `run`, never in a plan, and never recorded in state.
- **Exit 0 whenever a verdict was reached**, even when the verdict is "this file
  cannot be processed". Non-zero is reserved for faults. Job `backoffLimit` then
  retries infrastructure failures and the excuse ledger owns media failures,
  without the two multiplying.
- **State is split by writer**: the probe cache has a single owner, and excuses
  are per-file sidecars written only by the worker that owns that file. Two
  processes must never write the same state file.

## Conventions

**Comment only where the behaviour is unintuitive.** The default is no
comment: a reader who can follow the code does not need it narrated. Write one
only when the code would look wrong, arbitrary or accidental without it — then
state that fact in one or two lines and stop. No restating the line below, no
doc comments that only expand the identifier's name, no error transcripts. A
comment that repeats its own line crowds out the ones that carry information.

**Conventional Commits**, for commits and PR titles alike: `<type>(<scope>):
<subject>`, imperative mood, lower case after the colon. Types: `feat`, `fix`,
`chore`, `docs`, `refactor`, `ci`, `test`. Scope is the package —
`fix(run)`, `feat(orchestrate)`, `refactor(state)` — or `ci`, `docs`, `deps`.

## Layout

```
cmd/conform/     the binary; one file per subcommand's plumbing
internal/
  config/        desired state — profiles, libraries, execution settings
  encoder/       ffmpeg's encoders: codec presets, quality levels, rendering
  media/         observed state — ffprobe, normalised into media.File
  orchestrate/   turning a plan into one Kubernetes Job per file
  plan/          the diff, and rendering it as ffmpeg arguments
  run/           executing one plan: encode, verify, replace
  scan/          walking a library
  state/         probe cache and excuse records
  watch/         filesystem events, settled into paths worth re-judging
  webhook/       paths posted by other services; translators for their payloads
```

## Testing

`go test ./...`. `media` and `run` are tested only where they do not shell
out. Prefer growing the table tests in `plan/plan_test.go` —
the planner is where correctness actually lives, and it is pure, so it is cheap
to test exhaustively.

A webhook mapper is held to its contract by real payloads under
`internal/webhook/testdata/<name>/`, not by hand-written cases; its
[README](internal/webhook/README.md) says what a new one needs.

`orchestrate` keeps the cluster behind a `Kube` interface, so building a Job is
tested without one. Keep it that way: everything that decides what a Job says
belongs on the pure side of that line.
