package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A TCP listener alone is not ready. The browser must wait for healthy HTTP.
func TestLaunchDevWebWaitsForHealth(t *testing.T) {
	t.Parallel()
	for _, failBrowser := range []bool{false, true} {
		t.Run(map[bool]string{false: "opens", true: "manual URL"}[failBrowser], func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/bonnie/v1/health" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				if requests.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"status":"ready"}`))
			}))
			defer s.Close()
			var output bytes.Buffer
			calls := 0
			err := launchDevWeb(t.Context(), s.URL, &output, func(_ context.Context, url string) error {
				calls++
				if requests.Load() < 2 || url != s.URL+"/web/" {
					t.Fatalf("opened %q before health readiness", url)
				}
				if failBrowser {
					return errors.New("no browser")
				}
				return nil
			})
			if err != nil || calls != 1 {
				t.Fatalf("launch = %v, calls = %d", err, calls)
			}
			if failBrowser && !strings.Contains(output.String(), s.URL+"/web/") {
				t.Fatalf("missing manual URL: %s", &output)
			}
			if !failBrowser && output.Len() != 0 {
				t.Fatalf("unexpected output: %s", &output)
			}
		})
	}
}

// Cancellation ends a readiness wait and must not open a browser.
func TestLaunchDevWebCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer
	err := launchDevWeb(ctx, "http://127.0.0.1:1", &output, func(context.Context, string) error {
		t.Fatal("opened browser after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("launch = %v, want canceled", err)
	}
}
