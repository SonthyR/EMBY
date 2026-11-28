package main

import (
	"context"
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestV181SiteIconAuthenticatedFallback(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "Account-only icon", IngressMode: ingressModePath, PathPrefix: "/icons",
		TargetURL: "http://127.0.0.1:8096", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	png := makeSolidTestPNG(t, color.NRGBA{B: 128, A: 255})
	var authenticatedIconAvailable atomic.Bool
	authenticatedIconAvailable.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticatedIconAvailable.Load() && r.URL.Path == "/favicon.ico" && r.Header.Get("X-Emby-Token") == "temporary-token" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	var attemptedAccounts, loggedOut int
	loginSucceeds := false
	inst := &ProxyInstance{
		Site: *site,
		fetchSiteIcon: newSiteIconFetchFunc(
			http.DefaultTransport, upstreamURL, UAHeaderPolicy{}, upstreamHeaderPolicy{},
		),
		shareLogin: func(_ context.Context, _, _, _, _, _ string) (string, string, int, error) {
			attemptedAccounts++
			if !loginSucceeds || attemptedAccounts == 1 {
				return "", "", http.StatusUnauthorized, nil
			}
			return "temporary-token", "configured-account", http.StatusOK, nil
		},
		shareLogout: func(_ context.Context, token string) error {
			if token != "temporary-token" {
				t.Errorf("unexpected temporary logout token %q", token)
			}
			loggedOut++
			return nil
		},
	}
	app.pm.mu.Lock()
	app.pm.proxies[site.ID] = inst
	app.pm.mu.Unlock()

	fetch := func() *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		app.handleSiteIcon(rec, httptest.NewRequest(http.MethodPost, "/icon/fetch", nil), site.ID, "fetch")
		return rec
	}
	if rec := fetch(); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "没有可用的源站账号") {
		t.Errorf("expected missing account diagnosis, got %d %s", rec.Code, rec.Body)
	}
	for _, username := range []string{"account-one", "account-two"} {
		if _, err := app.db.saveSiteAccount(site.ID, 0, SiteAccountInput{Username: username, Password: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := fetch(); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "源站账号登录失败") {
		t.Errorf("expected actual login error, got %d %s", rec.Code, rec.Body)
	}
	attemptedAccounts = 0
	loginSucceeds = true
	rec := fetch()
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated icon fallback returned %d %s", rec.Code, rec.Body)
	}
	var result struct {
		RequiresConfirm bool   `json:"requires_confirm"`
		PreviewDataURL  string `json:"preview_data_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.RequiresConfirm || !strings.HasPrefix(result.PreviewDataURL, "data:image/png;base64,") {
		t.Fatalf("authenticated favicon did not require confirmation: %+v", result)
	}
	if attemptedAccounts != 2 || loggedOut != 1 {
		t.Fatalf("got %d login attempts and %d logouts, want 2 attempts and 1 logout", attemptedAccounts, loggedOut)
	}
	if app.db.hasAutoSiteIcon(site.ID) {
		t.Fatal("authenticated favicon was stored before administrator confirmation")
	}

	authenticatedIconAvailable.Store(false)
	if rec := fetch(); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "已登录源站账号，但未找到可用的头像或图标") {
		t.Errorf("expected image diagnosis after successful login, got %d %s", rec.Code, rec.Body)
	}
	if loggedOut != 2 {
		t.Fatalf("failed image fetch left its temporary login active, got %d logouts", loggedOut)
	}

	attemptedAccounts = 0
	if _, err := app.pm.shares.fetchSiteIconWithOutcome(context.Background(), site.ID, 0, false); err == nil {
		t.Fatal("automatic lookup unexpectedly found an anonymous icon")
	}
	if attemptedAccounts != 0 {
		t.Fatal("automatic icon lookup tried an account login")
	}
}
