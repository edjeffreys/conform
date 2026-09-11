package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/edjeffreys/conform/internal/config"
)

func Routes() []string {
	out := []string{direct.Route()}
	for _, m := range mappers {
		out = append(out, m.Route())
	}
	return out
}

type Server struct {
	srv     *http.Server
	ln      net.Listener
	rewrite []config.Rewrite
	accept  func(path string) bool

	incoming chan string
	changed  chan []string
	failed   chan error
}

// A path no library covers is refused rather than ignored, so a mount mismatch
// shows up in the sender's own log.
func Start(ctx context.Context, cfg config.Webhook, accept func(path string) bool) (*Server, error) {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}
	s := &Server{
		ln: ln, rewrite: cfg.Rewrite, accept: accept,
		incoming: make(chan string),
		changed:  make(chan []string),
		failed:   make(chan error, 1),
	}
	mux := http.NewServeMux()
	for _, m := range append([]Mapper{direct}, mappers...) {
		mux.HandleFunc("POST "+m.Route(), s.handler(m))
	}
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			s.failed <- err
		}
	}()
	go s.loop(ctx)
	return s, nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Paths posted while the receiver is busy accumulate into its next batch.
func (s *Server) Changed() <-chan []string { return s.changed }

func (s *Server) Err() <-chan error { return s.failed }

func (s *Server) Close() error { return s.srv.Close() }

func (s *Server) loop(ctx context.Context) {
	queued := map[string]bool{}
	for {
		var out chan []string
		var batch []string
		if len(queued) > 0 {
			out = s.changed
			batch = make([]string, 0, len(queued))
			for p := range queued {
				batch = append(batch, p)
			}
			slices.Sort(batch)
		}

		select {
		case <-ctx.Done():
			s.srv.Close()
			return
		case p := <-s.incoming:
			queued[p] = true
		case out <- batch:
			clear(queued)
		}
	}
}

func (s *Server) handler(m Mapper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req, err := m.decode(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sent := req.Paths

		paths := make([]string, len(sent))
		for i, p := range sent {
			paths[i] = rewrite(p, s.rewrite)
			if s.accept(paths[i]) {
				continue
			}
			msg := p + " is not a file any library covers"
			if paths[i] != p {
				msg = fmt.Sprintf("%s (as %s) is not a file any library covers", p, paths[i])
			}
			fmt.Fprintln(os.Stderr, "  ! webhook:", msg)
			http.Error(w, msg, http.StatusUnprocessableEntity)
			return
		}

		for _, p := range paths {
			select {
			case s.incoming <- p:
			case <-r.Context().Done():
				return
			}
		}
		if len(paths) > 0 {
			w.WriteHeader(http.StatusAccepted)
		}
	}
}
