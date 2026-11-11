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
			// One token per device, like Emby: DeviceId d1 keeps the legacy token.
			token := "upstream-token-1"
			if device := embyRequestAuthorizationAttribute(r, "DeviceId"); device != "" && device != "d1" {
				token = "upstream-token-" + device
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"` + token + `","User":{"Name":"real-user"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/Sessions/Logout":
			fake.mu.Lock()
			fake.loggedOut = append(fake.loggedOut, r.Header.Get("X-Emby-Token"))
			fake.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Items":
			if !strings.HasPrefix(r.Header.Get("X-Emby-Token"), "upstream-token-") && !strings.HasPrefix(r.URL.Query().Get("api_key"), "upstream-token-") {
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
	return doShareLoginDevice(t, base, username, password, "d1")
}

func doShareLoginDevice(t *testing.T, base, username, password, device string) (*http.Response, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"Username":%q,"Pw":%q}`, username, password)
	request, _ := http.NewRequest(http.MethodPost, base+"/Users/AuthenticateByName", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Test", Device="Phone", DeviceId="`+device+`", Version="1"`)
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
	return shareGetItemsWithToken(t, base, "upstream-token-1")
}

func shareGetItemsWithToken(t *testing.T, base, token string) int {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, base+"/Items", nil)
	request.Header.Set("X-Emby-Token", token)
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

func TestShareUserDeviceLimitSignsOutLeastRecentDevice(t *testing.T) {
	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, base := startShareTestSite(t, app, fake.server.URL)
	userID := createShareUserForTest(t, app, site.ID, 0)
	if _, err := app.db.db.Exec(`UPDATE share_users SET max_devices=2 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	app.reloadShareUsers()
	for _, device := range []string{"phone", "tv"} {
		if response, body := doShareLoginDevice(t, base, "friend", "friend-pass", device); response.StatusCode != http.StatusOK {
			t.Fatalf("login %s status = %d body=%s", device, response.StatusCode, body)
		}
	}
	// Make the phone the most recently active device, then add a third.
	if status := shareGetItemsWithToken(t, base, "upstream-token-phone"); status != http.StatusOK {
		t.Fatalf("phone items status = %d", status)
	}
	app.pm.shares.mu.Lock()
	app.pm.shares.lastSeen[shareTokenHash("upstream-token-tv")] = time.Now().Add(-time.Hour)
	app.pm.shares.lastSeen[shareTokenHash("upstream-token-phone")] = time.Now()
	app.pm.shares.mu.Unlock()
	if response, body := doShareLoginDevice(t, base, "friend", "friend-pass", "pad"); response.StatusCode != http.StatusOK {
		t.Fatalf("login pad status = %d body=%s", response.StatusCode, body)
	}
	if got := app.pm.shares.deviceCount(userID); got != 2 {
		t.Fatalf("devices = %d, want 2", got)
	}
	if status := shareGetItemsWithToken(t, base, "upstream-token-tv"); status != http.StatusUnauthorized {
		t.Fatalf("oldest device status = %d, want 401", status)
	}
	for _, token := range []string{"upstream-token-phone", "upstream-token-pad"} {
		if status := shareGetItemsWithToken(t, base, token); status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", token, status)
		}
	}
	// Signing in again from the same device replaces its old session.
	if response, _ := doShareLoginDevice(t, base, "friend", "friend-pass", "pad"); response.StatusCode != http.StatusOK {
		t.Fatalf("pad re-login status = %d", response.StatusCode)
	}
	if got := app.pm.shares.deviceCount(userID); got != 2 {
		t.Fatalf("devices after re-login = %d, want 2", got)
	}
}

func TestShareResetPeriodAndMonthlyReset(t *testing.T) {
	location := time.FixedZone("CST", 8*3600)
	cases := []struct {
		now          time.Time
		day          int
		period, next string
	}{
		{time.Date(2026, 11, 11, 9, 0, 0, 0, location), 1, "2026-11-01", "2026-12-01"},
		{time.Date(2026, 11, 11, 9, 0, 0, 0, location), 15, "2026-10-15", "2026-11-15"},
		{time.Date(2026, 1, 3, 9, 0, 0, 0, location), 5, "2025-12-05", "2026-01-05"},
	}
	for _, tc := range cases {
		period, next := shareResetPeriod(tc.now, tc.day)
		if period != tc.period || next.Format("2006-01-02") != tc.next {
			t.Fatalf("shareResetPeriod(%s, %d) = %s, %s", tc.now, tc.day, period, next.Format("2006-01-02"))
		}
	}
	if period, _ := shareResetPeriod(time.Now(), 0); period != "" {
		t.Fatalf("disabled reset day returned %q", period)
	}

	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, _ := startShareTestSite(t, app, fake.server.URL)
	userID := createShareUserForTest(t, app, site.ID, 0)
	if _, err := app.db.db.Exec(`UPDATE share_users SET traffic_used=5000, traffic_reset_day=1, traffic_reset_period='2000-01-01' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	app.reloadShareUsers()
	app.pm.shares.flush()
	var used int64
	var period string
	if err := app.db.db.QueryRow(`SELECT traffic_used, traffic_reset_period FROM share_users WHERE id=?`, userID).Scan(&used, &period); err != nil {
		t.Fatal(err)
	}
	if used != 0 || period == "2000-01-01" {
		t.Fatalf("after monthly reset used=%d period=%q", used, period)
	}
	// A second flush in the same cycle keeps new usage.
	if _, err := app.db.db.Exec(`UPDATE share_users SET traffic_used=42 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	app.pm.shares.flush()
	if err := app.db.db.QueryRow(`SELECT traffic_used FROM share_users WHERE id=?`, userID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 42 {
		t.Fatalf("usage reset twice in one cycle: %d", used)
	}
}

func TestSharePortalShowsStatusWithoutPassword(t *testing.T) {
	app := newTestApp(t)
	fake := newFakeShareUpstream(t)
	site, _ := startShareTestSite(t, app, fake.server.URL)
	userID := createShareUserForTest(t, app, site.ID, 10*1024*1024*1024)
	item, err := app.db.getShareUser(userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.portalToken) != 32 {
		t.Fatalf("portal token = %q", item.portalToken)
	}
	recorder := httptest.NewRecorder()
	app.handleSharePortal(recorder, httptest.NewRequest(http.MethodGet, "/share/"+item.portalToken, nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "Friend") || !strings.Contains(body, "share-site") {
		t.Fatalf("portal status=%d body=%s", recorder.Code, body)
	}
	if strings.Contains(body, "friend-pass") || strings.Contains(body, "real-pass") {
		t.Fatal("portal leaked a password")
	}
	if csp := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("portal CSP = %q", csp)
	}
	recorder = httptest.NewRecorder()
	app.handleSharePortal(recorder, httptest.NewRequest(http.MethodGet, "/share/00000000000000000000000000000000", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown token status = %d", recorder.Code)
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
