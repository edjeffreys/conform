# conform

Bring a media library into line with a profile written in git — no GUI, no
database of jobs.

> **Status:** the reconciler, its tests and a container image. The Kubernetes
> orchestrator, Helm chart and web UI described in [Distribution](#distribution)
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
      encoder: {name: eac3, options: {b: "640k"}}
    subtitles:
      languages: [eng]
      codecs: [subrip, ass, hdmv_pgs_subtitle]
```

Options are emitted with a full stream specifier (`-crf:v:0`, not `-crf`), so a
profile stays correct on a file with more than one video track. Keys are sorted,
so a given profile always produces byte-identical arguments.

`execution.tempDir` needs room for one source-sized file per concurrent worker,
and should not be replicated network storage: a transcode writes a full working
copy of everything it processes, so a replicated volume multiplies that write by
its replica count.

### Choices worth knowing about

- **Subtitles are dropped, never converted.** Turning image-based PGS into text
  needs OCR; guessing at it corrupts subtitles silently rather than failing.
- **A language filter that matches nothing is ignored.** Otherwise a mis-tagged
  file would be stripped of its only audio track.
- **Cover art is carried, not judged.** ffprobe reports it as a video stream, so
  measuring it against the video rules would mark every file with an embedded
  poster as needing a re-encode of one still frame.
- **A file with no video stream is never touched**, whatever its container.
- **Size is only checked on a re-encode.** A remux can grow slightly from
  container overhead alone, and refusing it on that basis would block a change
  that costs nothing.

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

*Designed, not yet built.* One transcode is one ffmpeg process and splitting a
single file across workers is rarely worth it, so conform parallelises across
files instead.

Rather than ship a scheduler, conform uses Kubernetes as the queue: an
orchestrator plans the library and creates one Job per non-conformant file,
and the cluster scheduler places it. A profile carries its own placement
requirements — device resources, node selectors, tolerations — which conform
passes through without interpreting, so a job needing a specific encoder can
only ever land somewhere that has one.

## Licence

MIT. Note that the QuickSync image variant includes Intel's `non-free` VA-API
driver from Debian, which carries its own terms.
