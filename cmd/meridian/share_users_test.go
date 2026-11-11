package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeShareUpstream struct {
	mu        sync.Mutex
	logins    []string
	loggedOut []string
	server    *httptest.Server
}

func newFakeShareUpstream(t *testing.T) *fakeShareUpstream {
	t.Helper()
	fake := &fakeShareUpstream{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.EqualFold(r.URL.Path, "/Users/AuthenticateByName"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			username, _ := body["Username"].(string)
			password, _ := body["Pw"].(string)
			fake.mu.Lock()
			fake.logins = append(fake.logins, username+":"+password)
			fake.mu.Unlock()
			if username != "real-user" || password != "real-pass" {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"upstream-token-1","User":{"Name":"real-user"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/Sessions/Logout":
			fake.mu.Lock()
			fake.loggedOut = append(fake.loggedOut, r.Header.Get("X-Emby-Token"))
			fake.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Items":
			if r.Header.Get("X-Emby-Token") != "upstream-token-1" && r.URL.Query().Get("api_key") != "upstream-token-1" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeShareUpstream) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logins...), append([]string(nil), f.loggedOut...)
}

func startShareTestSite(t *testing.T, app *App, upstream string) (*Site, string) {
	t.Helper()
	port := freePort(t)
	releasePort(port)
	site, err := app.db.CreateSiteRecord(Site{
		Name:         "share-site",
		ListenPort:   port,
		IngressMode:  ingressModePort,
		TargetURL:    upstream,
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       "infuse",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if _, err := app.db.saveSiteAccount(site.ID, 0, SiteAccountInput{Username: "real-user", Password: "real-pass"}); err != nil {
		t.Fatalf("saveSiteAccount: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	return site, fmt.Sprintf("http://127.0.0.1:%d", port)
}

func createShareUserForTest(t *testing.T, app *App, siteID int64, quota int64) int64 {
	t.Helper()
	input, err := app.normalizeShareUserInput(ShareUserInput{Username: "Friend", Password: "friend-pass", TrafficQuota: quota, Sites: []ShareUserSiteInput{{SiteID: siteID}}}, true)
	if err != nil {
		t.Fatalf("normalizeShareUserInput: %v", err)
	}
	id, err := app.saveShareUser(0, input)
	if err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()
	return id
}

func doShareLogin(t *testing.T, base, username, password string) (*http.Response, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"Username":%q,"Pw":%q}`, username, password)
	request, _ := http.NewRequest(http.MethodPost, base+"/Users/AuthenticateByName", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Test", Device="Phone", DeviceId="d1", Version="1"`)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response, data
}

func shareGetItems(t *testing.T, base string) int {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, base+"/Items", nil)
	request.Header.Set("X-Emby-Token", "upstream-token-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("items request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}

func TestShareUserLoginSwapsCredentialsAndTracksToken(t *testing.T) {
	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, base := startShareTestSite(t, app, fake.server.URL)
	userID := createShareUserForTest(t, app, site.ID, 0)

	response, body := doShareLogin(t, base, "friend", "wrong")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d body=%s", response.StatusCode, body)
	}
	if logins, _ := fake.snapshot(); len(logins) != 0 {
		t.Fatalf("wrong share password reached upstream: %v", logins)
	}

	response, body = doShareLogin(t, base, "friend", "friend-pass")
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "upstream-token-1") {
		t.Fatalf("share login status = %d body=%s", response.StatusCode, body)
	}
	logins, _ := fake.snapshot()
	if len(logins) != 1 || logins[0] != "real-user:real-pass" {
		t.Fatalf("upstream logins = %v, want saved account", logins)
	}
	if _, sessions := app.pm.shares.usage(userID); sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}

	if status := shareGetItems(t, base); status != http.StatusOK {
		t.Fatalf("items status = %d", status)
	}
	if used, _ := app.pm.shares.usage(userID); used < 4096 {
		t.Fatalf("share traffic = %d, want >= 4096", used)
	}

	// Plain upstream logins keep passing through untouched.
	response, body = doShareLogin(t, base, "real-user", "real-pass")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pass-through login status = %d body=%s", response.StatusCode, body)
	}
}

func TestShareUserDisableRejectsTokenAndLogsOutUpstream(t *testing.T) {
	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, base := startShareTestSite(t, app, fake.server.URL)
	userID := createShareUserForTest(t, app, site.ID, 0)
	if response, body := doShareLogin(t, base, "friend", "friend-pass"); response.StatusCode != http.StatusOK {
		t.Fatalf("share login status = %d body=%s", response.StatusCode, body)
	}

	if _, err := app.db.db.Exec(`UPDATE share_users SET enabled=0 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	app.reloadShareUsers()
	if status := shareGetItems(t, base); status != http.StatusUnauthorized {
		t.Fatalf("disabled share token status = %d, want 401", status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, loggedOut := fake.snapshot(); len(loggedOut) == 1 && loggedOut[0] == "upstream-token-1" {
			break
		}
		if time.Now().After(deadline) {
			_, loggedOut := fake.snapshot()
			t.Fatalf("upstream logout calls = %v", loggedOut)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if response, _ := doShareLogin(t, base, "friend", "friend-pass"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled share login status = %d, want 401", response.StatusCode)
	}
}

func TestShareUserQuotaBlocksFurtherRequests(t *testing.T) {
	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, base := startShareTestSite(t, app, fake.server.URL)
	createShareUserForTest(t, app, site.ID, 1024)
	if response, body := doShareLogin(t, base, "friend", "friend-pass"); response.StatusCode != http.StatusOK {
		t.Fatalf("share login status = %d body=%s", response.StatusCode, body)
	}
	if status := shareGetItems(t, base); status != http.StatusOK {
		t.Fatalf("first items status = %d", status)
	}
	if status := shareGetItems(t, base); status != http.StatusUnauthorized {
		t.Fatalf("over-quota items status = %d, want 401", status)
	}
}

func TestShareUserKickedTokenStaysRejectedLocally(t *testing.T) {
	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, base := startShareTestSite(t, app, fake.server.URL)
	userID := createShareUserForTest(t, app, site.ID, 0)
	if response, body := doShareLogin(t, base, "friend", "friend-pass"); response.StatusCode != http.StatusOK {
		t.Fatalf("share login status = %d body=%s", response.StatusCode, body)
	}
	if count, err := app.pm.shares.revokeSessions(userID, 0, nil); err != nil || count != 1 {
		t.Fatalf("revokeSessions = %d, %v", count, err)
	}
	// The fake upstream ignores logout, so only the local denylist can stop it.
	if status := shareGetItems(t, base); status != http.StatusUnauthorized {
		t.Fatalf("kicked token status = %d, want 401", status)
	}
	app.reloadShareUsers()
	if status := shareGetItems(t, base); status != http.StatusUnauthorized {
		t.Fatalf("kicked token after reload status = %d, want 401", status)
	}
	// Signing in again re-issues the same upstream token and must clear it.
	if response, body := doShareLogin(t, base, "friend", "friend-pass"); response.StatusCode != http.StatusOK {
		t.Fatalf("second share login status = %d body=%s", response.StatusCode, body)
	}
	if status := shareGetItems(t, base); status != http.StatusOK {
		t.Fatalf("re-issued token status = %d, want 200", status)
	}
}

func TestEmbyRequestTokensReadsHeadersAndQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/Videos/1/stream?api_key=query-token", nil)
	request.Header.Set("X-Emby-Authorization", `MediaBrowser Client="A", Token="header-token"`)
	tokens := embyRequestTokens(request)
	joined := strings.Join(tokens, ",")
	if !strings.Contains(joined, "header-token") || !strings.Contains(joined, "query-token") {
		t.Fatalf("tokens = %v", tokens)
	}
}
