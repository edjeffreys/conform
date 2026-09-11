package webhook

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edjeffreys/conform/internal/config"
)

func start(t *testing.T, rules ...config.Rewrite) (*Server, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Webhook{Listen: "127.0.0.1:0", Rewrite: rules}
	s, err := Start(ctx, cfg, func(path string) bool {
		return strings.HasPrefix(path, "/data/")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		s.Close()
	})
	return s, "http://" + s.Addr().String()
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(msg)
}

func next(t *testing.T, s *Server) []string {
	t.Helper()
	select {
	case got := <-s.Changed():
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("no batch")
		return nil
	}
}

func TestEveryRouteQueuesIntoOneBatch(t *testing.T) {
	s, base := start(t)

	requests := []struct{ route, body string }{
		{"/webhook", `{"paths":["/data/TV/c.mkv"]}`},
		{"/webhook/sonarr", `{"eventType":"Download","episodeFile":{"path":"/data/TV/b.mkv"}}`},
		{"/webhook/radarr", `{"eventType":"Download","movieFile":{"path":"/data/Movies/a.mkv"}}`},
		{"/webhook", `{"paths":["/data/TV/c.mkv"]}`},
	}
	for _, r := range requests {
		if code, msg := post(t, base+r.route, r.body); code != http.StatusAccepted {
			t.Fatalf("POST %s = %d %s, want 202", r.route, code, msg)
		}
	}

	want := []string{"/data/Movies/a.mkv", "/data/TV/b.mkv", "/data/TV/c.mkv"}
	if got := next(t, s); !slices.Equal(got, want) {
		t.Errorf("batch = %v, want %v", got, want)
	}
}

func TestRewriteAppliesBeforeTheLibraryCheck(t *testing.T) {
	s, base := start(t, config.Rewrite{From: "/tv", To: "/data/TV"}, config.Rewrite{From: "/films", To: "/elsewhere"})

	if code, msg := post(t, base+"/webhook/sonarr", `{"eventType":"Download","episodeFile":{"path":"/tv/Show/e1.mkv"}}`); code != http.StatusAccepted {
		t.Fatalf("rewritten path = %d %s, want 202", code, msg)
	}
	if got := next(t, s); !slices.Equal(got, []string{"/data/TV/Show/e1.mkv"}) {
		t.Errorf("batch = %v, want the rewritten path", got)
	}

	code, msg := post(t, base+"/webhook", `{"paths":["/films/a.mkv"]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(msg, "/films/a.mkv") || !strings.Contains(msg, "/elsewhere/a.mkv") {
		t.Errorf("bad mapping = %d %q, want 422 naming both forms", code, msg)
	}
}

func TestServerRefusesWhatItCannotActOn(t *testing.T) {
	s, base := start(t)

	checks := []struct {
		name, method, route, body string
		want                      int
	}{
		{"sonarr test, so saving the connection succeeds", "POST", "/webhook/sonarr", `{"eventType":"Test","episodeFile":{"path":"C:\\testpath"}}`, http.StatusOK},
		{"path outside every library", "POST", "/webhook", `{"paths":["/elsewhere/a.mkv"]}`, http.StatusUnprocessableEntity},
		{"one uncovered path among good ones", "POST", "/webhook", `{"paths":["/data/TV/a.mkv","/elsewhere/a.mkv"]}`, http.StatusUnprocessableEntity},
		{"import outside every library", "POST", "/webhook/radarr", `{"eventType":"Download","movieFile":{"path":"/elsewhere/a.mkv"}}`, http.StatusUnprocessableEntity},
		{"malformed body", "POST", "/webhook", `not json`, http.StatusBadRequest},
		{"wrong method", "GET", "/webhook", ``, http.StatusMethodNotAllowed},
		{"unknown service", "POST", "/webhook/lidarr", `{}`, http.StatusNotFound},
	}
	for _, c := range checks {
		req, err := http.NewRequest(c.method, base+c.route, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %s %s = %d, want %d", c.name, c.method, c.route, resp.StatusCode, c.want)
		}
	}

	select {
	case got := <-s.Changed():
		t.Errorf("queued %v from requests that should all have been refused", got)
	case <-time.After(200 * time.Millisecond):
	}
}
