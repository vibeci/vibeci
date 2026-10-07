package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Broker serves the sandbox API over a unix socket. It is the only VibeCI
// component with access to the Docker socket, and it holds no credentials.
type Broker struct {
	P      *DockerProvider
	Logger *slog.Logger
}

// Handler returns the HTTP API.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		v, err := b.P.Version(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "docker": v})
	})
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, b.P.List())
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if err := decodeStrict(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		s, err := b.P.Create(r.Context(), req)
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, ErrInvalid) {
				code = http.StatusBadRequest
			}
			b.Logger.Warn("sandbox create failed", "err", err)
			writeErr(w, code, err)
			return
		}
		writeJSON(w, s.Info())
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		s, ok := b.P.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("no such sandbox"))
			return
		}
		var req ExecRequest
		if err := decodeStrict(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		res, err := s.Exec(r.Context(), req)
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, ErrInvalid) {
				code = http.StatusBadRequest
			}
			writeErr(w, code, err)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := b.P.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		if err := s.Close(r.Context()); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	return mux
}

// Serve listens on the unix socket until ctx is canceled.
func (b *Broker) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// The socket lives in a volume shared only with the harness container,
	// whose uid differs from ours; the directory is the access boundary.
	if err := os.Chmod(socket, 0o666); err != nil {
		ln.Close()
		return err
	}
	if err := b.P.CleanupStale(ctx); err != nil {
		b.Logger.Warn("stale sandbox cleanup failed", "err", err)
	}
	srv := &http.Server{Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.P.Reap(ctx)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		for _, info := range b.P.List() {
			if s, ok := b.P.Get(info.ID); ok {
				s.Close(sctx)
			}
		}
	}()
	b.Logger.Info("sandboxd listening", "socket", socket)
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// BrokerClient talks to sandboxd.
type BrokerClient struct {
	http *http.Client
}

// NewBrokerClient returns a client for the unix socket.
func NewBrokerClient(socket string) *BrokerClient {
	return &BrokerClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}}}
}

func (c *BrokerClient) call(ctx context.Context, method, p string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://sandboxd"+p, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandboxd: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		if resp.StatusCode == http.StatusBadRequest {
			return fmt.Errorf("%w: %s", ErrInvalid, e.Error)
		}
		return fmt.Errorf("sandboxd: HTTP %d: %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Health implements Provider.
func (c *BrokerClient) Health(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/v1/health", nil, nil)
}

// Create implements Provider.
func (c *BrokerClient) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var info Info
	if err := c.call(ctx, http.MethodPost, "/v1/sandboxes", req, &info); err != nil {
		return nil, err
	}
	return &remoteSandbox{c: c, info: info}, nil
}

type remoteSandbox struct {
	c    *BrokerClient
	info Info
}

func (s *remoteSandbox) Info() Info { return s.info }

func (s *remoteSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	var res ExecResult
	if err := s.c.call(ctx, http.MethodPost, "/v1/sandboxes/"+s.info.ID+"/exec", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *remoteSandbox) Close(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	return s.c.call(cctx, http.MethodDelete, "/v1/sandboxes/"+s.info.ID, nil, nil)
}
