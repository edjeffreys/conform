# conform

Keep a media library in line with a profile you write in git. No GUI, no
database of jobs, no clicking through flow builders.

You describe what an acceptable file looks like. conform probes what you have,
works out the difference, and closes it.

> **Status:** the reconciler, the Kubernetes orchestrator, tests and a container
> image all work today. The Helm chart and web UI under
> [Distribution](#distribution) are designed but not built yet.

## What it looks like

`plan` reads your library and tells you what it would change. It touches
nothing:

```
~/Projects/conform main
❯ ./conform plan -config conform.local.yaml -verbose

transcode sample.mp4
          · video: codec h264 not in hevc
          · container mp4 is not mkv
          $ ffmpeg [-hide_banner -nostdin -y -i media/sample.mp4 -map 0:0 -map 0:1 -c:v:0 libx265 -crf:v:0 28 -preset:v:0 veryfast -x265-params:v:0 log-level=error -c:a:0 copy -map_metadata 0 -map_chapters 0 OUTPUT.mkv]

1 files — 0 conformant, 0 remux, 1 transcode
```

Two reasons this file falls short, and the exact ffmpeg command that would fix
it. `apply` runs it:

```
~/Projects/conform main
❯ ./conform apply -config conform.local.yaml -verbose

          $ ffmpeg -hide_banner -nostdin -y -i media/sample.mp4 -map 0:0 -map 0:1 -c:v:0 libx265 -crf:v:0 28 -preset:v:0 veryfast -x265-params:v:0 log-level=error -c:a:0 copy -map_metadata 0 -map_chapters 0 .conform-tmp/conform-e9f07b9c0ad7b80e.mkv
transcode sample.mkv 346.2MB → 50.1MB (-86%) in 59s

1 files — 0 conformant, 0 remux, 1 transcode
```

Run `plan` again and you get the point of the whole thing:

```
~/Projects/conform main
❯ ./conform plan -config conform.local.yaml

1 files — 1 conformant, 0 remux, 0 transcode
```

Nothing left to do — and nothing left to do *tomorrow* either, because the
answer comes from the file's own streams rather than from a record of what was
done to it.

There is also `probe`, which shows you a file the way conform sees it:

```
~/Projects/conform main
❯ ./conform probe media/sample.mkv

{
  "path": "media/sample.mkv",
  "size": 52526545,
  "modTime": "2026-09-10T16:20:37.658426681+01:00",
  "container": "mkv",
  "duration": 235.008,
  "streams": [
    {
      "index": 0,
      "type": "video",
      "codec": "hevc",
      "profile": "Main",
      "language": "eng",
      "width": 1920,
      "height": 1080,
      "default": true
    },
    {
      "index": 1,
      "type": "audio",
      "codec": "aac",
      "profile": "LC",
      "language": "eng",
      "channels": 2,
      "default": true
    }
  ]
}
```

## Try it

You need Go and ffmpeg. `conform.local.yaml` uses `libx265` and a `./media`
directory, so it runs on any laptop:

```sh
go build -o conform ./cmd/conform

./conform probe media/some-file.mkv           # what conform sees
./conform plan  -config conform.local.yaml    # what it would do
./conform apply -config conform.local.yaml    # do it
./conform plan  -config conform.local.yaml    # must now be a no-op
```

`plan` is always safe. `apply -dry-run` walks the whole apply path without
writing anything. `-limit 1` tries exactly one file, and `-verbose` shows the
ffmpeg command lines.

Both commands also take paths:

```sh
./conform plan  -config conform.local.yaml media/some-file.mkv
./conform apply -config conform.local.yaml media/some-file.mkv
```

Tests are `go test ./...`.

## The idea

conform is a reconciler, not a queue.

| | |
|---|---|
| **Desired state** | a profile in `conform.yaml` |
| **Observed state** | what `ffprobe` reports about the file |
| **Action** | whatever closes the gap |

Every rule is a predicate on what is **acceptable**, never an instruction to
act. `codecs: [hevc]` means "hevc is fine as it is" — so the output of a
transcode satisfies the rule that triggered it, and the second pass leaves it
alone. A rule phrased as an action would loop forever.

Nothing about a file's history is consulted, so `conform plan` is a pure
function of the library and the config, and running `apply` twice is a no-op.
That is not left to trust: the runner re-probes and re-plans every output
before committing it, and refuses anything that does not come back clean.

There is no job database because the library **is** the database. A file needs
work if and only if its own streams say so.

<details>
<summary><b>The two things that do need state</b></summary>

A pure reconcile has one failure mode: a file that can never satisfy the
profile is retried forever. Two cases hit it —

- ffmpeg cannot process the file at all;
- the re-encode comes out *larger* than the source, so keeping the original is
  the better outcome and the change is rejected.

Both are recorded as excuses, alongside a probe cache. Every record is keyed on
the file's size and mtime, so replacing the file at a path discards its excuse
with it — a new download is judged on its own merits, never on its
predecessor's.

Only an attempt that reached the media can spend a file's error budget. A
failure before that — a device that will not open, a volume with no room left
— is the worker's, and is [a fault](#exit-status) rather than a mark against
whichever file happened to be in hand.

</details>

### Why not Tdarr

Tdarr's flows, plugins and library settings live in its own MongoDB, edited
through a web UI. The container is declarable; everything that decides what it
actually *does* is not. That is the whole reason this exists.

## Config

A profile is a list of predicates. A file satisfying all of them is left alone,
and a rule left empty imposes no constraint.

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

Two files ship with the repo. `conform.example.yaml` is the profile above,
against Intel QuickSync; `conform.local.yaml` is the same rules with `libx265`
instead, so it runs anywhere ffmpeg does.

`execution.tempDir` needs room for one source-sized file per concurrent worker.
Don't point it at replicated network storage — a transcode writes a full
working copy of everything it processes, and a replicated volume multiplies
that write by its replica count.

<details>
<summary><b>QuickSync and older Intel silicon</b></summary>

QuickSync needs the oneVPL runtime for Gen11 or newer silicon, which the image
ships. Older Intel parts have no runtime in current Debian and want
`hevc_vaapi`, which drives the same hardware through the layer underneath.

Encoder options are emitted with a full stream specifier (`-crf:v:0`, not
`-crf`), so a profile stays correct on a file with more than one video track.
Keys are sorted, so a given profile always produces byte-identical arguments.

</details>

### The stereo companion

`stereoCompanion` requires a stereo track alongside every surround one in the
same language, and derives it where it is missing. Like every other rule it is
a predicate: a file that already has both is left alone, which is what stops it
adding a track on every pass.

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

`order` says what order the kept streams of that type are acceptable in. A file
already like that is left alone; anything else is remuxed — no re-encode. Two
keys:

- **`language`** — the stream's position in that rule's own `languages` list,
  so the list doubles as a preference order. A language the profile does not
  list sorts last.
- **`channels`** — most channels first. Audio only.

Ordering one type never regroups the others: a file that interleaves audio and
subtitle streams keeps that layout, with only the audio positions rewritten.

<details>
<summary><b>Why only those two keys</b></summary>

Ties keep the file's own order, which is what makes the order total. Without
that, "is this file already in order?" and "what order would I emit?" could
disagree, and the file would be remuxed on every pass, forever.

A key whose value the transcode itself changes — `codec`, say — could order the
output differently from the input that produced it, and the run would reject
its own work at [verification](#replacing-a-file). `language` survives a copy
untouched, and `channels` settles after one downmix.

</details>

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
  from container overhead alone, and a profile asking for an extra track means
  the file to grow. The check catches a re-encode that got bigger for nothing.

## Replacing a file

The original is never opened for writing. conform encodes to `tempDir`,
verifies, copies the result into the source's *own* directory as a hidden
staging file, then renames over the original — a rename within one filesystem,
which is atomic. The temp directory is often a different volume, so renaming
straight from it would not be.

A container change writes the new extension and removes the old file
afterwards, so a crash in between leaves two copies rather than none.

Verification is three checks, all of which must pass:

1. the output probes cleanly;
2. its duration is at least `minDurationRatio` of the source — ffmpeg exits 0
   after writing a truncated file more often than it reports an error;
3. re-planning the output returns `none`.

Check 3 is the important one. It is direct proof that the file now conforms,
and therefore that the next pass will leave it alone. If it fails, the profile
is unsatisfiable rather than the file being bad, and the message says so.

## Exit status

**0 whenever conform reached a verdict**, including "this file cannot be
processed". That outcome is already recorded as an excuse, and failing the run
for it as well would have the ledger and a job runner's own retries multiply.

**Non-zero is a fault**: a bad config, a path no library covers, a directory
that cannot be read, ffmpeg missing, a hardware encoder that cannot open its
device. Those are worth retrying; a file that will not encode is not.

<details>
<summary><b>The hardware-encoder case, which has to be got right</b></summary>

A worker whose GPU is missing fails every file it is given, and an excuse keys
on the file — so a broken image would quietly spend each file's error budget
until the whole library was excused for a fault none of it had.

Where a profile names a hardware encoder, conform therefore creates the device
*before* encoding anything and exits non-zero if it cannot, and treats ffmpeg's
device-setup errors as faults if one appears mid-encode. The check runs once
per worker.

</details>

## New files

A pass finds everything eventually. Two settings have conform pick up a new
file as soon as it arrives instead, and either one keeps a full `apply` or
`orchestrate` running after its first pass:

```yaml
libraries:
  - name: tv
    path: /data/TV
    profile: standard
    watch: true          # act on files as they appear

webhook:
  listen: ":8080"        # accept new files' paths from other services
```

Either way, conform only learns *where to look*. The file is judged exactly as a
full pass would judge it, so one that already conforms is left alone.

A run given file paths, `-limit` or `-dry-run` still exits after one pass. That
matters for a [worker Job](#distribution): it reads the same config, and would
otherwise never finish.

Passes run one at a time. A file that arrives during a long pass waits for it
to finish rather than being lost.

### Webhooks

Anything that knows when a file is finished can tell conform about it — a
download client's on-completion hook, a script, cron:

```sh
curl -X POST http://conform:8080/webhook \
  -d '{"paths": ["/data/TV/Show/Season 1/Show - S01E01.mkv"]}'
```

conform answers `202` once the paths are queued and acts on them straight away,
with no settle delay. A path no library covers is refused with `422`, and the
whole request with it, so a mistake shows up at the sender instead of vanishing.

Services that send their own payload rather than a list of paths get a route
that translates it:

| service | URL | tick |
|---|---|---|
| Sonarr | `http://conform:8080/webhook/sonarr` | On File Import, On File Upgrade |
| Radarr | `http://conform:8080/webhook/radarr` | On File Import, On File Upgrade |

Add each under **Settings → Connect → Webhook**; **Test** should succeed. Any
other event they send is answered `200` and ignored, so ticking more triggers is
harmless. Another service is one mapper and a handful of its real payloads — see
[`internal/webhook`](internal/webhook/README.md). For downloads managed by Sonarr or Radarr this is the better choice
than watching: they notify only once an import is complete.

When a service sees the library at a different path than conform does, rewrite
the prefix. The longest matching `from` wins, and it only matches whole path
components:

```yaml
webhook:
  listen: ":8080"
  rewrite:
    - from: /tv        # as Sonarr sees it
      to: /data/TV     # as conform sees it
```

There is no authentication yet. Keep the port on a network only your media stack
can reach — a cluster Service rather than an Ingress.

### Watching a folder

`watch: true` catches files that arrive any other way — copied, downloaded or
moved in, at the root or any depth. A directory moved in whole is walked, so a
season arrives as its episodes.

A watched file is acted on once it has gone a minute without a write, so a
download is not picked up half-written. That is a heuristic. Just before
committing, conform also checks the source still has the size and mtime it was
planned from, and drops the encode without charging an excuse if not. A
downloader that writes into an incomplete directory and moves the file in once
it is done never exposes a partial file at all.

<details>
<summary><b>Where watching does not work</b></summary>

A missed event costs latency, never correctness — but it is missed silently:

- **Network mounts.** NFS and SMB deliver no events for writes made by another
  machine. Leave `watch` off for those and use the webhook or `-interval`.
- **Watch limits.** Linux needs one inotify watch per directory, and exits
  saying so when `fs.inotify.max_user_watches` runs out. macOS keeps a file
  descriptor per watched file, so it does not suit a large library.
- **A burst too big for the kernel's queue.** conform notices the dropped events
  and walks every library again.

</details>

## Distribution

One transcode is one ffmpeg process, and splitting a single file across workers
is rarely worth it, so conform parallelises across files instead.

Rather than ship a scheduler, conform uses Kubernetes as the queue.
`conform orchestrate` plans the library and creates one Job per non-conformant
file; the cluster scheduler places it; the Job runs `conform apply <path>`,
which re-derives the same plan from the same config and exits 0 with its
verdict recorded.

```sh
conform orchestrate -config /config/conform.yaml -dry-run -verbose  # the Jobs it would create
conform orchestrate -config /config/conform.yaml -interval 6h       # keep the library topped up
```

`-dry-run` creates nothing, but still reads the cluster: a Job *is* the
profile's PodTemplate with a path appended, so there is nothing to show without
fetching it.

This is what makes a single-path run matter. The plan is re-derived from the
config either way, so a worker handed one path reaches the same verdict a full
pass would have — no description of the work has to travel between them. Such a
worker reads the probe cache but never writes it; the cache has one owner, and
a hundred workers would otherwise race over it.

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

`orchestrator.maxActive` caps the Jobs in flight. Whatever is over the cap is
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
