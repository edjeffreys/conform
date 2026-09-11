package webhook

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type expectation struct {
	Status  int     `json:"status"`
	Request Request `json:"request"`
}

func TestMapperRegistry(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	routes := map[string]bool{direct.Route(): true}
	for _, m := range mappers {
		if !slug.MatchString(m.Name) {
			t.Errorf("mapper name %q is not a lowercase slug, so it makes a poor route", m.Name)
		}
		if routes[m.Route()] {
			t.Errorf("route %s is registered twice", m.Route())
		}
		routes[m.Route()] = true
	}
}

// Posting the expected Request to /webhook must queue the same batch: a
// mapper's output is the generic contract and nothing beyond it.
func TestMappersHonourTheContract(t *testing.T) {
	for _, m := range mappers {
		t.Run(m.Name, func(t *testing.T) {
			dir := filepath.Join("testdata", m.Name)
			payloads := fixtures(t, dir)
			if len(payloads) == 0 {
				t.Fatalf("no fixtures in %s; every mapper needs real payloads to prove its mapping", dir)
			}
			for _, payload := range payloads {
				name := strings.TrimSuffix(filepath.Base(payload), ".json")
				t.Run(name, func(t *testing.T) {
					body := readFile(t, payload)
					var want expectation
					if err := json.Unmarshal(readFile(t, strings.TrimSuffix(payload, ".json")+".want.json"), &want); err != nil {
						t.Fatalf("expectation: %v", err)
					}

					s, base := start(t)
					code, msg := post(t, base+m.Route(), string(body))
					if code != want.Status {
						t.Fatalf("POST %s = %d %s, want %d", m.Route(), code, strings.TrimSpace(msg), want.Status)
					}
					if code != http.StatusAccepted {
						return
					}
					got := next(t, s)

					wantPaths := slices.Clone(want.Request.Paths)
					slices.Sort(wantPaths)
					wantPaths = slices.Compact(wantPaths)
					if !slices.Equal(got, wantPaths) {
						t.Errorf("%s queued %v, want %v", m.Route(), got, wantPaths)
					}

					generic, err := json.Marshal(want.Request)
					if err != nil {
						t.Fatal(err)
					}
					if code, msg := post(t, base+direct.Route(), string(generic)); code != http.StatusAccepted {
						t.Fatalf("the expected Request is not one /webhook accepts: %d %s", code, msg)
					}
					if again := next(t, s); !slices.Equal(again, got) {
						t.Errorf("/webhook queued %v for the same Request, but %s queued %v", again, m.Route(), got)
					}
				})
			}
		})
	}
}

func TestDirectRoute(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"paths", `{"paths":["/data/TV/a.mkv","/data/TV/b.mkv"]}`, http.StatusAccepted},
		{"no paths", `{"paths":[]}`, http.StatusBadRequest},
		{"misspelt field", `{"path":["/data/TV/a.mkv"]}`, http.StatusBadRequest},
		{"empty path", `{"paths":[""]}`, http.StatusBadRequest},
		{"relative path", `{"paths":["TV/a.mkv"]}`, http.StatusBadRequest},
		{"not json", `/data/TV/a.mkv`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, base := start(t)
			if code, msg := post(t, base+direct.Route(), c.body); code != c.want {
				t.Errorf("POST %s = %d %s, want %d", c.body, code, strings.TrimSpace(msg), c.want)
			}
		})
	}
}

func fixtures(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var payloads []string
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	for name := range names {
		switch {
		case strings.HasSuffix(name, ".want.json"):
			if !names[strings.TrimSuffix(name, ".want.json")+".json"] {
				t.Errorf("%s has no payload beside it", filepath.Join(dir, name))
			}
		case strings.HasSuffix(name, ".json"):
			if !names[strings.TrimSuffix(name, ".json")+".want.json"] {
				t.Errorf("%s has no .want.json beside it", filepath.Join(dir, name))
				continue
			}
			payloads = append(payloads, filepath.Join(dir, name))
		}
	}
	slices.Sort(payloads)
	return payloads
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
