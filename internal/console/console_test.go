package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesTheConsole(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name string
		path string
		code int
	}{
		{"the root", "/", http.StatusOK},
		// A route the page handles itself. It must resolve to the page rather
		// than 404, because the console is a single document — a reload on one
		// of its own routes is a normal thing to do.
		{"an unknown path falls back to the page", "/some/route", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.code {
				t.Fatalf("GET %s = %d, want %d", tc.path, w.Code, tc.code)
			}
			if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
				t.Errorf("Content-Type = %q, want HTML", w.Header().Get("Content-Type"))
			}
		})
	}
}

func TestThePageIsNotCached(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	// The page is what tells a client where the API is; a stale copy would keep
	// pointing at a deployment that has moved.
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestThePageIsSelfContained(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	body := w.Body.String()

	// Nothing external, because the console is served from a throwaway runner
	// behind a tunnel: a CDN reference would be a new failure mode — and a new
	// thing to trust — for a single page.
	for _, bad := range []string{"cdn.", "unpkg", "jsdelivr", "googleapis", "http://", "https://"} {
		if strings.Contains(body, bad) {
			t.Errorf("the console references %q; it should be self-contained", bad)
		}
	}
	if len(body) < 1000 {
		t.Fatalf("the console page is only %d bytes; it does not look like a whole page", len(body))
	}
}
