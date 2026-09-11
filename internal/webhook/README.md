# webhook

Every route turns one POST into a **`Request`**, and conform acts on nothing
else. `POST /webhook` takes a `Request` as it is; a service that sends its own
payload gets a **mapper** from that payload to a `Request`.

```json
{"paths": ["/data/TV/Show/Season 1/Show - S01E01.mkv"]}
```

## The contract

A mapper is `func(Payload) (Request, error)`, and what it returns decides the
response:

| mapper returns | response | meaning |
|---|---|---|
| a `Request` with paths | `202` | queue these files |
| a `Request` with no paths | `200` | an event conform does not act on — a test ping, a grab, a delete |
| an error | `400` | the payload is malformed, or claims an import but names no file |

The server then enforces the rest, identically for every mapper, so none of it
belongs in one:

- every path must be absolute, else `400`;
- `webhook.rewrite` is applied to each path;
- a path no library covers refuses the whole request with `422`, naming the path
  before and after rewriting.

Return only the files the event says are *now in the library*. An upgrade's
replaced originals, a deleted file, a download still in progress: none of these
is a file to judge.

## Adding a service

Four steps, using Sonarr ([`arr.go`](arr.go)) as the model.

**1. Declare the payload.** Only the fields the mapping reads. Everything else
in the body is ignored, so the service can add fields without breaking conform.

```go
type sonarrPayload struct {
	EventType   string   `json:"eventType"`
	EpisodeFile *arrFile `json:"episodeFile"`
}
```

**2. Map it.** Filter on the event first, so every other event is a `200`.

```go
func fromSonarr(p sonarrPayload) (Request, error) {
	if p.EventType != "Download" {
		return Request{}, nil
	}
	...
}
```

**3. Register it** in [`mappers.go`](mappers.go). The name is its route,
`/webhook/<name>`, and must be a lowercase slug.

```go
Map("sonarr", fromSonarr),
```

**4. Add real payloads** under `testdata/<name>/`, each beside a `.want.json`:

```
testdata/sonarr/download.json        a body Sonarr actually sends
testdata/sonarr/download.want.json   {"status": 202, "request": {"paths": [...]}}
```

Take them from the service itself where you can — its webhook test button, a
request bin, or the source that builds the payload. Cover at least its test
ping, a real import, and any event whose body names a file conform must *not*
queue. `/data/` paths are the ones the test server treats as covered.

`TestMappersHonourTheContract` then posts every payload to the mapper's route
and checks the status and the files queued. For each `202` it also posts the
expected `Request` to `/webhook` and requires the same result, which is what
holds a mapper to the contract and nothing more. A mapper with no payloads, or a
payload with no expectation, fails the test.
