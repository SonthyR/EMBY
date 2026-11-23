package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTaggedEmby struct {
	server        *httptest.Server
	tag           string
	mu            sync.Mutex
	stoppedHits   int
	logoutTokens  []string
	streamRelease chan struct{}
	streamStarted chan struct{}
}

func newFakeTaggedEmby(t *testing.T, tag string, holdStream bool) *fakeTaggedEmby {
	t.Helper()
	fake := &fakeTaggedEmby{tag: tag}
	if holdStream {
		fake.streamRelease = make(chan struct{})
		fake.streamStarted = make(chan struct{}, 4)
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/users/authenticatebyname"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["Username"] != "real-"+tag || body["Pw"] != "pass-"+tag {
				http.Error(w, "invalid upstream credentials", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"token-` + tag + `","ServerId":"srv-` + tag + `"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/logout"):
			fake.mu.Lock()
			fake.logoutTokens = append(fake.logoutTokens, r.Header.Get("X-Emby-Token"))
			fake.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/playing"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/playing/progress"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/playing/stopped"):
			fake.mu.Lock()
			fake.stoppedHits++
			fake.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(path, "/videos/"):
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("head-" + tag + ":"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			if fake.streamStarted != nil {
				select {
				case fake.streamStarted <- struct{}{}:
				default:
				}
				<-fake.streamRelease
			}
			_, _ = w.Write([]byte("tail-" + tag))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"server":"` + tag + `","path":"` + r.URL.Path + `"}`))
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func startTaggedShareSite(t *testing.T, app *App, name, tag, targetURL string) *Site {
	t.Helper()
	port := freePort(t)
	releasePort(port)
	site, err := app.db.CreateSiteRecord(Site{
		Name:         name,
		ListenPort:   port,
		IngressMode:  ingressModePort,
		TargetURL:    targetURL,
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       "infuse",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord(%s): %v", name, err)
	}
	if _, err := app.db.saveSiteAccount(site.ID, 0, SiteAccountInput{
		Username: "real-" + tag,
		Password: "pass-" + tag,
	}); err != nil {
		t.Fatalf("saveSiteAccount(%s): %v", name, err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	return site
}

func newPersonalSharePanelServer(t *testing.T, app *App) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me/login", app.csrfMiddleware(app.handleShareSelfLogin))
	mux.HandleFunc("/api/me", app.handleShareSelf)
	mux.HandleFunc("/api/me/", app.handleShareSelf)
	mux.HandleFunc("/api/share-users", app.handleShareUsers)
	mux.HandleFunc("/api/share-users/", app.handleShareUserByID)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("panel-root"))
	})
	srv := httptest.NewServer(app.publicHostRouter(mux))
	t.Cleanup(srv.Close)
	return srv
}

func TestSharePersonalRouteValidationAndConflicts(t *testing.T) {
	app := newTestApp(t)
	sitePort := freePort(t)
	releasePort(sitePort)
	site, err := app.db.CreateSiteRecord(Site{
		Name:         "path-site",
		ListenPort:   sitePort,
		IngressMode:  ingressModePath,
		PathPrefix:   "/taken",
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       "infuse",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })

	for _, bad := range []string{"api", "app", "legacy", "_meridian", "bad space", "taken"} {
		if _, err := app.normalizeShareUserInput(ShareUserInput{
			Username:    "u-" + strings.ReplaceAll(bad, " ", ""),
			Password:    "pass1234",
			RoutePrefix: bad,
			Sites:       []ShareUserSiteInput{{SiteID: site.ID}},
		}, 0, true); err == nil {
			t.Fatalf("expected route_prefix %q to be rejected", bad)
		}
	}

	valid, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "wsuser",
		Password:    "pass1234",
		RoutePrefix: "WS",
		Sites:       []ShareUserSiteInput{{SiteID: site.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("valid route_prefix rejected: %v", err)
	}
	if valid.RoutePrefix != "/ws" {
		t.Fatalf("normalized route_prefix = %q, want /ws", valid.RoutePrefix)
	}
	firstID, err := app.saveShareUser(0, valid)
	if err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()

	// Duplicate share route_prefix must be rejected for another user, but allowed for self.
	if _, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "other",
		Password:    "pass1234",
		RoutePrefix: "/ws",
		Sites:       []ShareUserSiteInput{{SiteID: site.ID}},
	}, 0, true); err == nil {
		t.Fatal("duplicate route_prefix should be rejected")
	}
	if _, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "wsuser",
		RoutePrefix: "ws",
		Sites:       []ShareUserSiteInput{{SiteID: site.ID}},
	}, firstID, false); err != nil {
		t.Fatalf("updating same user with same route_prefix failed: %v", err)
	}
}

func TestSharePersonalRouteSwitchWhenIdle(t *testing.T) {
	app := newTestApp(t)
	embyA := newFakeTaggedEmby(t, "alpha", false)
	embyB := newFakeTaggedEmby(t, "beta", false)
	siteA := startTaggedShareSite(t, app, "Alpha 服", "alpha", embyA.server.URL)
	siteB := startTaggedShareSite(t, app, "Beta 服", "beta", embyB.server.URL)

	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "ws",
		Password:    "secret123",
		RoutePrefix: "ws",
		Sites:       []ShareUserSiteInput{{SiteID: siteA.ID}, {SiteID: siteB.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	userID, err := app.saveShareUser(0, input)
	if err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()

	panel := newPersonalSharePanelServer(t, app)

	// 1. Exact prefix redirects to trailing slash.
	noRedirectClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirResp, err := noRedirectClient.Get(panel.URL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	_ = redirResp.Body.Close()
	if redirResp.StatusCode != http.StatusPermanentRedirect || redirResp.Header.Get("Location") != "/ws/" {
		t.Fatalf("GET /ws = %d (%q), want 308 /ws/", redirResp.StatusCode, redirResp.Header.Get("Location"))
	}

	// 2. Non-owner / upstream account cannot sign in through /ws.
	badResp, _ := doShareLogin(t, panel.URL+"/ws", "real-alpha", "pass-alpha")
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-owner login on /ws = %d, want 401", badResp.StatusCode)
	}

	// 3. Owner signs in through /ws -> hits Site A (#1).
	resp, body := doShareLogin(t, panel.URL+"/ws", "ws", "secret123")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "token-alpha") {
		t.Fatalf("first /ws login = %d %s", resp.StatusCode, string(body))
	}

	reqA, _ := http.NewRequest(http.MethodGet, panel.URL+"/ws/Items", nil)
	reqA.Header.Set("X-Emby-Token", "token-alpha")
	resA, err := http.DefaultClient.Do(reqA)
	if err != nil {
		t.Fatalf("GET /ws/Items: %v", err)
	}
	bodyA, _ := io.ReadAll(resA.Body)
	_ = resA.Body.Close()
	if resA.StatusCode != http.StatusOK || !strings.Contains(string(bodyA), `"server":"alpha"`) {
		t.Fatalf("GET /ws/Items before switch = %d %s", resA.StatusCode, string(bodyA))
	}

	// 4. Share text uses single fixed URL.
	stored, err := app.db.getShareUser(userID)
	if err != nil {
		t.Fatalf("getShareUser: %v", err)
	}
	shareText, err := app.shareUserText(stored, "https://emby.99889898.xyz")
	if err != nil {
		t.Fatalf("shareUserText: %v", err)
	}
	if !strings.Contains(shareText, "https://emby.99889898.xyz/ws") || !strings.Contains(shareText, "Alpha 服") {
		t.Fatalf("shareText missing fixed route:\n%s", shareText)
	}

	// 5. Friend logs into self-service web UI and drags Site B to #1.
	loginReq, _ := http.NewRequest(http.MethodPost, panel.URL+"/api/me/login", strings.NewReader(`{"username":"ws","password":"secret123"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Origin", panel.URL)
	loginRes, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatalf("/api/me/login: %v", err)
	}
	_ = loginRes.Body.Close()
	if loginRes.StatusCode != http.StatusOK {
		t.Fatalf("/api/me/login = %d", loginRes.StatusCode)
	}
	var sessionCookie *http.Cookie
	for _, c := range loginRes.Cookies() {
		if c.Name == shareSessionCookie {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing share session cookie")
	}

	orderPayload, _ := json.Marshal(map[string]any{"site_ids": []int64{siteB.ID, siteA.ID}})
	orderReq, _ := http.NewRequest(http.MethodPut, panel.URL+"/api/me/sites/order", bytes.NewReader(orderPayload))
	orderReq.Header.Set("Content-Type", "application/json")
	orderReq.Header.Set("Origin", panel.URL)
	orderReq.AddCookie(sessionCookie)
	orderRes, err := http.DefaultClient.Do(orderReq)
	if err != nil {
		t.Fatalf("PUT /api/me/sites/order: %v", err)
	}
	var orderBody struct {
		ActiveSiteID int64 `json:"active_site_id"`
		Revoked      int   `json:"revoked"`
		Draining     int   `json:"draining"`
	}
	_ = json.NewDecoder(orderRes.Body).Decode(&orderBody)
	_ = orderRes.Body.Close()
	if orderRes.StatusCode != http.StatusOK || orderBody.ActiveSiteID != siteB.ID || orderBody.Revoked != 1 || orderBody.Draining != 0 {
		t.Fatalf("order response = %d %+v", orderRes.StatusCode, orderBody)
	}

	// 6. Old Site A token is immediately rejected, and re-login on /ws enters Site B.
	staleReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/ws/Items", nil)
	staleReq.Header.Set("X-Emby-Token", "token-alpha")
	staleRes, err := http.DefaultClient.Do(staleReq)
	if err != nil {
		t.Fatalf("stale GET /ws/Items: %v", err)
	}
	_ = staleRes.Body.Close()
	if staleRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale token after idle switch = %d, want 401", staleRes.StatusCode)
	}

	respB, bodyB := doShareLogin(t, panel.URL+"/ws", "ws", "secret123")
	if respB.StatusCode != http.StatusOK || !strings.Contains(string(bodyB), "token-beta") {
		t.Fatalf("re-login on /ws after switch = %d %s", respB.StatusCode, string(bodyB))
	}
}

func TestSharePersonalRouteDrainsActiveVideoUntilStopped(t *testing.T) {
	app := newTestApp(t)
	embyA := newFakeTaggedEmby(t, "alpha", false)
	embyB := newFakeTaggedEmby(t, "beta", false)
	siteA := startTaggedShareSite(t, app, "Alpha 服", "alpha", embyA.server.URL)
	siteB := startTaggedShareSite(t, app, "Beta 服", "beta", embyB.server.URL)

	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "viewer",
		Password:    "secret123",
		RoutePrefix: "viewer",
		Sites:       []ShareUserSiteInput{{SiteID: siteA.ID}, {SiteID: siteB.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	userID, err := app.saveShareUser(0, input)
	if err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()

	panel := newPersonalSharePanelServer(t, app)
	resp, body := doShareLogin(t, panel.URL+"/viewer", "viewer", "secret123")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "token-alpha") {
		t.Fatalf("initial login = %d %s", resp.StatusCode, string(body))
	}

	// Start Emby playback on Site A.
	playReq, _ := http.NewRequest(http.MethodPost, panel.URL+"/viewer/Sessions/Playing", strings.NewReader(`{}`))
	playReq.Header.Set("X-Emby-Token", "token-alpha")
	playRes, err := http.DefaultClient.Do(playReq)
	if err != nil || playRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Sessions/Playing failed: %v", err)
	}
	_ = playRes.Body.Close()

	// Switch priority to Site B while video is playing.
	if _, err := app.db.db.Exec(`UPDATE share_user_sites SET sort_order=1 WHERE share_user_id=? AND site_id=?`, userID, siteB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE share_user_sites SET sort_order=2 WHERE share_user_id=? AND site_id=?`, userID, siteA.ID); err != nil {
		t.Fatal(err)
	}
	app.reloadShareUsers()
	revoked, draining := app.pm.shares.reconcileSiteSwitch(userID, false)
	if revoked != 0 || draining != 1 {
		t.Fatalf("reconcile while playing = revoked:%d draining:%d, want 0, 1", revoked, draining)
	}

	// Video stream & progress continue routing to Site A uninterrupted!
	vidReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/viewer/Videos/123/stream.mkv", nil)
	vidReq.Header.Set("X-Emby-Token", "token-alpha")
	vidRes, err := http.DefaultClient.Do(vidReq)
	if err != nil {
		t.Fatalf("GET video while draining: %v", err)
	}
	vidBody, _ := io.ReadAll(vidRes.Body)
	_ = vidRes.Body.Close()
	if vidRes.StatusCode != http.StatusOK || !strings.Contains(string(vidBody), "head-alpha:tail-alpha") {
		t.Fatalf("video during drain = %d %q", vidRes.StatusCode, string(vidBody))
	}

	// Closing the video sends Sessions/Playing/Stopped -> reaches Site A AND immediately cuts off the old session.
	stopReq, _ := http.NewRequest(http.MethodPost, panel.URL+"/viewer/Sessions/Playing/Stopped", strings.NewReader(`{}`))
	stopReq.Header.Set("X-Emby-Token", "token-alpha")
	stopRes, err := http.DefaultClient.Do(stopReq)
	if err != nil {
		t.Fatalf("Sessions/Playing/Stopped: %v", err)
	}
	_ = stopRes.Body.Close()
	if stopRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Stopped status = %d, want 204", stopRes.StatusCode)
	}
	embyA.mu.Lock()
	stoppedHits := embyA.stoppedHits
	embyA.mu.Unlock()
	if stoppedHits != 1 {
		t.Fatalf("Site A stoppedHits = %d, want 1", stoppedHits)
	}

	// Immediately after closing video, old Site A token is rejected.
	afterReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/viewer/Items", nil)
	afterReq.Header.Set("X-Emby-Token", "token-alpha")
	afterRes, err := http.DefaultClient.Do(afterReq)
	if err != nil {
		t.Fatalf("GET after stop: %v", err)
	}
	_ = afterRes.Body.Close()
	if afterRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after video stopped = %d, want 401", afterRes.StatusCode)
	}

	// Re-login with same URL and credentials immediately enters Site B.
	loginB, bodyB := doShareLogin(t, panel.URL+"/viewer", "viewer", "secret123")
	if loginB.StatusCode != http.StatusOK || !strings.Contains(string(bodyB), "token-beta") {
		t.Fatalf("re-login after video stopped = %d %s", loginB.StatusCode, string(bodyB))
	}
}

func TestSharePersonalRouteDrainsInFlightStreamOnAdminSwitchAndExpiresOnIdle(t *testing.T) {
	origIdle := shareStreamIdleTimeout
	origSweep := shareDrainSweepInterval
	shareStreamIdleTimeout = 120 * time.Millisecond
	shareDrainSweepInterval = 40 * time.Millisecond
	t.Cleanup(func() {
		shareStreamIdleTimeout = origIdle
		shareDrainSweepInterval = origSweep
	})

	app := newTestApp(t)
	embyA := newFakeTaggedEmby(t, "alpha", true)
	embyB := newFakeTaggedEmby(t, "beta", false)
	siteA := startTaggedShareSite(t, app, "Alpha 服", "alpha", embyA.server.URL)
	siteB := startTaggedShareSite(t, app, "Beta 服", "beta", embyB.server.URL)

	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "streamuser",
		Password:    "secret123",
		RoutePrefix: "su",
		Sites:       []ShareUserSiteInput{{SiteID: siteA.ID}, {SiteID: siteB.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	userID, err := app.saveShareUser(0, input)
	if err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()
	panel := newPersonalSharePanelServer(t, app)

	resp, _ := doShareLogin(t, panel.URL+"/su", "streamuser", "secret123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}

	// Start an in-flight video stream on Site A.
	streamDone := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, panel.URL+"/su/Videos/99/stream.mkv", nil)
		req.Header.Set("X-Emby-Token", "token-alpha")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			streamDone <- "err:" + err.Error()
			return
		}
		data, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		streamDone <- strconv.Itoa(res.StatusCode) + ":" + string(data)
	}()

	select {
	case <-embyA.streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight video stream")
	}

	// Admin switches this user to Site B ONLY (removing Site A in the backend).
	adminBody, _ := json.Marshal(ShareUserInput{
		Username:    "streamuser",
		RoutePrefix: "su",
		Sites:       []ShareUserSiteInput{{SiteID: siteB.ID}},
	})
	adminReq, _ := http.NewRequest(http.MethodPut, panel.URL+"/api/share-users/"+strconv.FormatInt(userID, 10), bytes.NewReader(adminBody))
	adminReq.Header.Set("Content-Type", "application/json")
	adminRes, err := http.DefaultClient.Do(adminReq)
	if err != nil {
		t.Fatalf("admin PUT share-user: %v", err)
	}
	_ = adminRes.Body.Close()
	if adminRes.StatusCode != http.StatusOK {
		t.Fatalf("admin PUT share-user = %d", adminRes.StatusCode)
	}
	if app.pm.shares.drainingCount(userID) != 1 {
		t.Fatalf("drainingCount = %d, want 1", app.pm.shares.drainingCount(userID))
	}

	// Release the in-flight stream: it must complete intact!
	close(embyA.streamRelease)
	result := <-streamDone
	if result != "200:head-alpha:tail-alpha" {
		t.Fatalf("in-flight stream interrupted: %q", result)
	}

	// After idle timeout with no Stopped call, the draining session is revoked automatically.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.pm.shares.drainingCount(userID) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if app.pm.shares.drainingCount(userID) != 0 {
		t.Fatal("draining session was not revoked after stream idle timeout")
	}
	staleReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/su/Items", nil)
	staleReq.Header.Set("X-Emby-Token", "token-alpha")
	staleRes, err := http.DefaultClient.Do(staleReq)
	if err != nil {
		t.Fatalf("GET after drain timeout: %v", err)
	}
	_ = staleRes.Body.Close()
	if staleRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale token after idle drain timeout = %d, want 401", staleRes.StatusCode)
	}
}

func TestSharePersonalRouteFallbackWhenFirstSiteDisabled(t *testing.T) {
	app := newTestApp(t)
	embyA := newFakeTaggedEmby(t, "alpha", false)
	embyB := newFakeTaggedEmby(t, "beta", false)
	siteA := startTaggedShareSite(t, app, "Alpha 服", "alpha", embyA.server.URL)
	siteB := startTaggedShareSite(t, app, "Beta 服", "beta", embyB.server.URL)

	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "fallback",
		Password:    "secret123",
		RoutePrefix: "fb",
		Sites:       []ShareUserSiteInput{{SiteID: siteA.ID}, {SiteID: siteB.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := app.saveShareUser(0, input); err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()

	// Disable #1 site: /fb must automatically fall back to #2 enabled site (Site B).
	if err := app.pm.StopSite(siteA.ID); err != nil {
		t.Fatalf("StopSite: %v", err)
	}
	if err := app.db.SetSiteEnabled(siteA.ID, false); err != nil {
		t.Fatalf("SetSiteEnabled: %v", err)
	}

	panel := newPersonalSharePanelServer(t, app)
	resp, body := doShareLogin(t, panel.URL+"/fb", "fallback", "secret123")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "token-beta") {
		t.Fatalf("login with #1 disabled = %d %s, want token-beta", resp.StatusCode, string(body))
	}
}
