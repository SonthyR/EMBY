package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yundu/web"
)

func TestStaticHandlerServesNewPanelByDefault(t *testing.T) {
	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		t.Fatalf("static fs: %v", err)
	}
	handler := staticHandler(staticFS)
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	for _, path := range []string{"/", "/app/", "/some/unknown/page"} {
		rr := get(path)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `src="/app/assets/`) {
			t.Fatalf("%s did not serve the new panel: %d %.200s", path, rr.Code, rr.Body.String())
		}
	}
	if rr := get("/legacy/"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `id="sidebar-version"`) {
		t.Fatalf("/legacy/ did not serve the previous panel: %d", rr.Code)
	}
	if rr := get("/app/assets/missing-chunk.js"); rr.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", rr.Code)
	}
	entries, err := fs.ReadDir(staticFS, "app/assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("embedded app assets missing: %v", err)
	}
	for _, entry := range entries {
		// go:embed silently drops names starting with "_" or "."; a chunk
		// named like that would 404 at runtime.
		if strings.HasPrefix(entry.Name(), "_") || strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("asset %q would be skipped by go:embed", entry.Name())
		}
	}
	asset := get("/app/assets/" + entries[0].Name())
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("hashed asset status=%d cache=%q", asset.Code, asset.Header().Get("Cache-Control"))
	}
}
