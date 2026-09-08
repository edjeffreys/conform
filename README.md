# conform

Bring a media library into line with a profile written in git — no GUI, no
database of jobs.

> **Status:** the reconciler, the Kubernetes orchestrator, tests and a container
> image. The Helm chart and web UI mentioned in [Distribution](#distribution)
> are designed but not yet built. What is here works standalone today.

## Why not Tdarr

Tdarr's flows, plugins and library settings live in its own MongoDB, edited
through a web UI. The container is declarable; everything that decides what it
actually *does* is not. That is the whole reason this exists.

## The model

conform is a reconciler, not a queue.

| | |
|---|---|
| **Desired state** | a profile in `conform.yaml` |
| **Observed state** | what `ffprobe` reports about the file |
| **Action** | whatever closes the gap |

Nothing about a file's history is consulted to decide whether it needs work, so
`conform plan` is a pure function of the library and the config, and running
`apply` twice is a no-op. That property is not incidental — it is what lets the
config be the only source of truth, and it is enforced rather than assumed: the
runner re-probes and re-plans every output before committing it, and refuses
anything that does not come back clean.

There is no job database because the library *is* the database. A file is
non-conformant if and only if its own streams say so.

### The two things that do need state

A pure reconcile has one failure mode: a file that can never satisfy the
profile is retried forever. Two cases hit it —

- ffmpeg cannot process the file at all;
- the re-encode comes out *larger* than the source, so keeping the original is
  the better outcome and the change is rejected.

Both are recorded as excuses, alongside a probe cache. Every record is keyed on
the file's size and mtime, so replacing the file at a path discards its excuse
with it — a new download is judged on its own merits, never on its
predecessor's.

## Trying it

Needs Go and ffmpeg. `conform.local.yaml` uses `libx265` and a `./media`
directory, so it runs anywhere:

```sh
go build -o conform ./cmd/conform

./conform probe media/some-file.mkv           # what conform sees
./conform plan  -config conform.local.yaml    # what it would do; touches nothing
./conform apply -config conform.local.yaml    # do it
./conform plan  -config conform.local.yaml    # must now be a no-op
```

`plan` is read-only and always safe. `apply -dry-run` goes through the apply
path without writing. Add `-limit 1` to try exactly one file, and `-verbose` to
see the ffmpeg command lines.

Both also take paths, which are judged by whichever library contains them:

```sh
./conform plan  -config conform.local.yaml media/some-file.mkv
./conform apply -config conform.local.yaml media/some-file.mkv
```

The plan is re-derived from the config either way, so a process handed a single
path reaches the same verdict a full pass would have. That is what the
orchestrator described under [Distribution](#distribution) will rely on, instead
of sending a worker a description of the work. Such a worker reads the probe
cache but never writes it — the cache has one owner, and a hundred of them
would otherwise be racing over it.

### Exit status

0 whenever conform reached a verdict, **including** "this file cannot be
processed". That outcome is already recorded as an excuse, and failing the run
for it as well would have the ledger and a job runner's own retries multiply.

Non-zero is a fault: a bad config, a path no library covers, a directory that
cannot be read, ffmpeg missing. Those are worth retrying; a file that will not
encode is not.

Run the tests with `go test ./...`.

## Config

`conform.local.yaml` is a software-encoder profile that runs anywhere.
`conform.example.yaml` is the same rules against Intel QuickSync.

Every rule is a predicate on what is **acceptable**, never an instruction to
act. A file satisfying all of them is left alone. Rules left empty impose no
constraint.

```yaml
profiles:
  standard:
    container: mkv         # anything else is remuxed
    video:
      codecs: [hevc]       # acceptable as-is; anything else is re-encoded
      maxHeight: 1080      # taller is downscaled
      encoder: {name: hevc_qsv, options: {global_quality: "24"}}
    audio:
      languages: [eng, und]   # other languages are dropped
      codecs: [aac, ac3, eac3]
      maxChannels: 6          # wider is downmixed
      order: [language, channels]   # any other order is remuxed into this one
      encoder: {name: eac3, options: {b: "640k"}}
      stereoCompanion:              # a stereo track must exist alongside surround
        encoder: {name: aac, options: {b: "192k"}}
    subtitles:
      languages: [eng]
      codecs: [subrip, ass, hdmv_pgs_subtitle]
      order: [language]
```

Options are emitted with a full stream specifier (`-crf:v:0`, not `-crf`), so a
profile stays correct on a file with more than one video track. Keys are sorted,
so a given profile always produces byte-identical arguments.

`execution.tempDir` needs room for one source-sized file per concurrent worker,
and should not be replicated network storage: a transcode writes a full working
copy of everything it processes, so a replicated volume multiplies that write by
its replica count.

### The stereo companion

`stereoCompanion` requires a stereo track alongside every surround one in the
same language, and derives it where it is missing. It is a predicate like the
rest: a file that already has both is left alone, which is what stops it adding
a track on every pass.

The point is the *downmix*, not the extra track. A player folding 5.1 down to
stereo puts the centre channel — where dialogue sits — well below the music and
effects around it. Measured on a test file with dialogue in the centre and
music in the other four channels:

| stereo track | dialogue vs music |
|---|---|
| player's own downmix | **−7.6 dB** |
| `stereoCompanion` | **+2.9 dB** |

The default filter lifts the centre and pulls the rest down to leave room for
it:

```
pan=stereo|FL=1.4*FC+0.5*FL+0.5*BL|FR=1.4*FC+0.5*FR+0.5*BR
```

Set `filter` to override it — `compand` for night-mode compression,
`dynaudnorm` after the pan for a flatter result. The derived stream is tagged
with its source's language explicitly rather than relying on ffmpeg to copy it,
because matching it back on the next pass is exactly what converges.

A surround stream the profile is already downmixing (`maxChannels: 2`) needs no
companion — the rule is a predicate on what the output will hold, not on the
input.

### Stream order

`order` is a predicate like every other rule: it says what order the kept
streams of that type are acceptable in, so a file already like that is left
alone and anything else is remuxed into it — no re-encode. Two keys:

- `language` — the stream's position in that rule's own `languages` list, so
  the list doubles as a preference order. A language the profile does not list
  sorts last, which only happens when the filter matched nothing.
- `channels` — most channels first. Audio only.

Ties keep the file's own order, which is what makes the order total. Without
that, "is this file already in order?" and "what order would I emit?" could
disagree and the file would be remuxed on every pass, forever.

Only those two keys, and deliberately so. A key whose value the transcode
itself changes — `codec`, say — could order the output differently from the
input that produced it, and the run would reject its own work at
[verification](#replacing-a-file). `language` survives a copy untouched, and
`channels` settles after one downmix.

Ordering one type never regroups the others: a file that interleaves audio and
subtitle streams keeps that layout, with only the audio positions rewritten.

### Choices worth knowing about

- **Subtitles are dropped, never converted.** Turning image-based PGS into text
  needs OCR; guessing at it corrupts subtitles silently rather than failing.
- **A language filter that matches nothing is ignored.** Otherwise a mis-tagged
  file would be stripped of its only audio track.
- **Cover art is carried, not judged.** ffprobe reports it as a video stream, so
  measuring it against the video rules would mark every file with an embedded
  poster as needing a re-encode of one still frame.
- **A file with no video stream is never touched**, whatever its container.
- **An untagged stream reads as `und`.** So `languages: [eng]` drops untagged
  subtitles, and `[eng, und]` keeps them. The "matched nothing, keep
  everything" fallback is audio-only: a file whose subtitles are all mis-tagged
  loses them, where a file whose audio is mis-tagged does not.
- **Size is only checked on a re-encode that adds nothing.** A remux can grow
  from container overhead alone, and a profile that asks for an extra track
  means the file to grow. The check is there to catch a re-encode that got
  bigger for nothing, so neither case should trip it.

## Replacing a file

The original is never opened for writing. conform encodes to `tempDir`, verifies,
copies the result into the source's *own* directory as a hidden staging file, and
renames over the original — a rename within one filesystem, which is atomic. The
temp directory is often a different volume, so renaming straight from it would
not be.

A container change writes the new extension and removes the old file afterwards,
so a crash in between leaves two copies rather than none.

Verification is three checks, all of which must pass:

1. the output probes cleanly;
2. its duration is at least `minDurationRatio` of the source — ffmpeg exits 0
   after writing a truncated file more often than it reports an error;
3. re-planning the output returns `none`.

Check 3 is the important one. It is direct proof that the file now conforms,
and therefore that the next pass will leave it alone. If it fails, the profile
is unsatisfiable rather than the file being bad, and the message says so.

## Distribution

One transcode is one ffmpeg process and splitting a single file across workers
is rarely worth it, so conform parallelises across files instead.

Rather than ship a scheduler, conform uses Kubernetes as the queue. `conform
orchestrate` plans the library and creates one Job per non-conformant file; the
cluster scheduler places it; the Job runs `conform apply <path>`, which
re-derives the same plan from the same config and exits 0 with its verdict
recorded.

```sh
conform orchestrate -config /config/conform.yaml -dry-run -verbose  # the Jobs it would create
conform orchestrate -config /config/conform.yaml -interval 6h       # keep the library topped up
```

`-dry-run` creates nothing, but still reads the cluster: a Job *is* the
profile's PodTemplate with a path appended, so there is nothing to show without
fetching it.

### Placement is data

A profile names a `core/v1 PodTemplate` and conform copies it — device
resources, node selectors, tolerations, the mounts that make the library
visible — without interpreting any of it. conform never learns what a GPU is,
so this works against any device plugin.

```yaml
apiVersion: v1
kind: PodTemplate
metadata:
  name: conform-qsv
  namespace: media
template:
  spec:
    containers:
      - name: conform
        image: ghcr.io/edjeffreys/conform:latest
        args: ["apply", "-config", "/config/conform.yaml"]   # conform appends the path
        resources:
          limits:
            gpu.intel.com/i915: 1
```

The path is *appended* to the container's args rather than replacing them, so
the subcommand and any flags stay in the template with the rest of the
placement data. A container declaring no args is an error, not a default: the
path would otherwise become the whole command line.

### Still no job database

A Job's name is a hash of the file's path, size and mtime, so creating one that
already exists is how a later pass discovers the file is in flight — the
cluster holds the queue, and conform holds nothing. Size and mtime are in the
hash so a replaced file gets a new name instead of colliding with the finished
Job of its predecessor.

`orchestrator.maxActive` caps the Jobs in flight; whatever is over the cap is
simply not created, and the next pass re-derives it from the library the same
way it derived this one.

This is also where [exit status](#exit-status) earns its keep. `backoffLimit`
retries infrastructure faults; the excuse ledger owns files that cannot be
encoded. Because a media verdict exits 0, the two never multiply.

Not built yet: a Helm chart, and a web UI that observes and triggers. Desired
state stays in git — an editable profile in a web form rebuilds the problem
conform exists to solve.

## Licence

MIT. Note that the QuickSync image variant includes Intel's `non-free` VA-API
driver from Debian, which carries its own terms.
