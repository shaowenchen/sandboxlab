package console

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
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

// Every element the script looks up is on the page.
//
// There is no build step and no bundler, so nothing else notices a renamed id:
// the script reaches for null, the console half-draws, and the failure is a
// blank section rather than an error. The sign-in work renamed several ids and
// deleted the whole key row, which is exactly the change this would have
// caught.
func TestEveryElementTheScriptUsesExists(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	page := w.Body.String()

	script := page[strings.Index(page, "<script>"):]

	// $("x") is the page's own helper for getElementById.
	used := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\$\("([A-Za-z0-9_-]+)"\)`),
		regexp.MustCompile(`getElementById\("([A-Za-z0-9_-]+)"\)`),
	} {
		for _, m := range re.FindAllStringSubmatch(script, -1) {
			used[m[1]] = true
		}
	}
	if len(used) == 0 {
		t.Fatal("found no element lookups in the script; the pattern above is probably wrong")
	}

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`).FindAllStringSubmatch(page, -1) {
		declared[m[1]] = true
	}

	var missing []string
	for id := range used {
		if !declared[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the script reaches for elements that are not on the page: %s", strings.Join(missing, ", "))
	}

	// And the reverse, for buttons: one the script never mentions does nothing
	// when it is clicked, and looks exactly like a button that works.
	//
	// The dialog actions have no id — they are identified by their value — so
	// they are not part of this.
	var inert []string
	for _, m := range regexp.MustCompile(`<button[^>]*\bid="([A-Za-z0-9_-]+)"`).FindAllStringSubmatch(page, -1) {
		if !used[m[1]] {
			inert = append(inert, m[1])
		}
	}
	sort.Strings(inert)
	if len(inert) > 0 {
		t.Errorf("these buttons have no handler, so clicking them does nothing: %s", strings.Join(inert, ", "))
	}
}

// The console has no user management left in it.
//
// The page is one file with no build step, so nothing else notices a view that
// was removed from the API but left on the screen: it would draw, fail every
// call it makes, and read as a broken deployment rather than a deleted feature.
func TestTheUserInterfaceIsGone(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	page := w.Body.String()

	for _, gone := range []string{"/users", "/whoami", "whoami(", "isAdmin", "owner", "quota"} {
		if strings.Contains(page, gone) {
			t.Errorf("the console still mentions %q", gone)
		}
	}

	// The sign-in card is still there, and now the eye beside it does something.
	for _, want := range []string{`id="signin-key"`, `id="signin-reveal"`, `$("signin-reveal").onclick`} {
		if !strings.Contains(page, want) {
			t.Errorf("the console is missing %q", want)
		}
	}
}
