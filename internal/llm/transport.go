package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx response from a provider.
type APIError struct {
	Status     int
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm api error (HTTP %d): %s", e.Status, e.Message)
}

// retryableError marks transient failures (network errors, truncated streams,
// overloaded events inside a stream).
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func transient(err error) error {
	if err == nil {
		return nil
	}
	return &retryableError{err}
}

func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 15 * time.Minute, // non-streaming calls can think for a long time
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr}
}

type transport struct {
	client     *http.Client
	timeout    time.Duration
	maxRetries int
	headers    map[string]string
	logger     *slog.Logger
	name       string
}

func newTransport(spec ProviderSpec, name string, logger *slog.Logger) transport {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return transport{
		client:     newHTTPClient(),
		timeout:    timeout,
		maxRetries: spec.MaxRetries,
		headers:    spec.Headers,
		logger:     logger,
		name:       name,
	}
}

// call runs attempt with retries and exponential backoff. Each attempt gets
// its own timeout.
func (t *transport) call(ctx context.Context, attempt func(ctx context.Context) (*Response, error)) (*Response, error) {
	var lastErr error
	for i := 0; i <= t.maxRetries; i++ {
		actx, cancel := context.WithTimeout(ctx, t.timeout)
		resp, err := attempt(actx)
		cancel()
		if err == nil {
			resp.Usage.Calls = 1
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		}
		wait, ok := retryDelay(err, i)
		if !ok || i == t.maxRetries {
			break
		}
		t.logger.Warn("llm call failed, retrying", "model", t.name, "attempt", i+1, "wait", wait.Round(time.Millisecond), "err", truncate(err.Error(), 300))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		}
	}
	return nil, lastErr
}

func retryDelay(err error, attempt int) (time.Duration, bool) {
	backoff := time.Duration(float64(2*time.Second) * float64(int(1)<<min(attempt, 6)))
	backoff = min(backoff, 90*time.Second)
	backoff = time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if !apiErr.Retryable {
			return 0, false
		}
		if apiErr.RetryAfter > 0 {
			return min(apiErr.RetryAfter, 5*time.Minute), true
		}
		return backoff, true
	}
	var re *retryableError
	if errors.As(err, &re) {
		return backoff, true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return backoff, true // per-attempt timeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return backoff, true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return backoff, true
	}
	msg := err.Error()
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "GOAWAY") || strings.Contains(msg, "stream error") {
		return backoff, true
	}
	return 0, false
}

func retryableStatus(code int) bool {
	switch code {
	case 408, 409, 425, 429, 500, 502, 503, 504, 520, 521, 522, 523, 524, 529:
		return true
	}
	return false
}

// post sends a JSON body and returns the response for 2xx statuses. Other
// statuses are converted to *APIError (the body is consumed and closed).
func (t *transport) post(ctx context.Context, url string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vibeci/1")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return t.do(req)
}

func (t *transport) do(req *http.Request) (*http.Response, error) {
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, transient(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	apiErr := &APIError{
		Status:    resp.StatusCode,
		Message:   errorMessage(b),
		Retryable: retryableStatus(resp.StatusCode),
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		} else if when, err := http.ParseTime(ra); err == nil {
			apiErr.RetryAfter = time.Until(when)
		}
	}
	return nil, apiErr
}

// errorMessage extracts a readable message from an error body: an "error"
// object with "type" or "code" and "message" (Anthropic, OpenAI), an
// "error" string, or a top-level "message". Other bodies are returned as
// they are, truncated.
func errorMessage(b []byte) string {
	var v struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &v); err == nil {
		switch e := v.Error.(type) {
		case map[string]any:
			msg, _ := e["message"].(string)
			typ, _ := e["type"].(string)
			if typ == "" {
				typ, _ = e["code"].(string)
			}
			if msg != "" {
				return strings.TrimSpace(typ + ": " + msg)
			}
		case string:
			if e != "" {
				return e
			}
		}
		if v.Message != "" {
			return v.Message
		}
	}
	return truncate(strings.TrimSpace(string(b)), 2000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// readJSON decodes a bounded JSON response body.
func readJSON(r io.Reader, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return transient(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decoding provider response: %w (body: %s)", err, truncate(string(b), 500))
	}
	return nil
}
