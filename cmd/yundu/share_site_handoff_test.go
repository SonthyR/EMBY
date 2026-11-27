package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSeamlessEmby struct {
	server        *httptest.Server
	tag           string
	userID        string
	serverID      string
	token         string
	loginCount    atomic.Int32
	mu            sync.Mutex
	logoutTokens  []string
	lastPath      string
	lastQuery     string
	lastToken     string
	lastAuthHdr   string
	streamRelease chan struct{}
	streamStarted chan struct{}
}

func newFakeSeamlessEmby(t *testing.T, tag, uid, serverID, token string, holdStream bool) *fakeSeamlessEmby {
	t.Helper()
	f := &fakeSeamlessEmby{tag: tag, userID: uid, serverID: serverID, token: token}
	if holdStream {
		f.streamRelease = make(chan struct{})
		f.streamStarted = make(chan struct{}, 8)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		f.mu.Lock()
		f.lastPath = r.URL.Path
		f.lastQuery = r.URL.RawQuery
		f.lastToken = r.Header.Get("X-Emby-Token")
		f.lastAuthHdr = r.Header.Get("X-Emby-Authorization")
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/users/authenticatebyname"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["Username"] != "real-"+tag || body["Pw"] != "pass-"+tag {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			f.loginCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": f.token,
				"ServerId":    f.serverID,
				"User": map[string]any{
					"Id":       f.userID,
					"ServerId": f.serverID,
					"Name":     "real-" + tag,
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/logout"):
			f.mu.Lock()
			f.logoutTokens = append(f.logoutTokens, r.Header.Get("X-Emby-Token"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/playing"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/sessions/playing/stopped"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/system/info/public") || strings.HasSuffix(path, "/system/info"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         f.serverID,
				"ServerId":   f.serverID,
				"ServerName": "Emby-" + tag,
			})
		case strings.HasSuffix(path, "/users/"+strings.ToLower(f.userID)):
			if r.Header.Get("X-Emby-Token") != f.token {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":       f.userID,
				"ServerId": f.serverID,
				"Name":     "real-" + tag,
			})
		case strings.Contains(path, "/videos/"):
			if r.Header.Get("X-Emby-Token") != f.token {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("chunk-" + tag))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			if f.streamStarted != nil {
				select {
				case f.streamStarted <- struct{}{}:
				default:
				}
				<-f.streamRelease
			}
		default:
			if r.Header.Get("X-Emby-Token") != f.token {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server": tag,
				"path":   r.URL.Path,
				"query":  r.URL.RawQuery,
				"uid":    f.userID,
			})
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestSeamlessSiteSwitchMapsTokenUserIDAndVirtualServerID(t *testing.T) {
	app := newTestApp(t)
	embyA := newFakeSeamlessEmby(t, "alpha", "uid-alpha-111", "srv-real-alpha", "tok-alpha-aaa", false)
	embyB := newFakeSeamlessEmby(t, "beta", "uid-beta-999", "srv-real-beta", "tok-beta-bbb", false)
	siteA := startTaggedShareSite(t, app, "Alpha", "alpha", embyA.server.URL)
	siteB := startTaggedShareSite(t, app, "Beta", "beta", embyB.server.URL)

	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "alice",
		Password:    "pass1234",
		RoutePrefix: "alice",
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

	// 1. Login on /alice -> hits Site A and normalizes ServerId to stable virtual ID.
	resp, body := doShareLogin(t, panel.URL+"/alice", "alice", "pass1234")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %s", resp.StatusCode, string(body))
	}
	var loginPayload struct {
		AccessToken string `json:"AccessToken"`
		ServerId    string `json:"ServerId"`
		User        struct {
			Id       string `json:"Id"`
			ServerId string `json:"ServerId"`
		} `json:"User"`
	}
	if err := json.Unmarshal(body, &loginPayload); err != nil {
		t.Fatalf("unmarshal login: %v", err)
	}
	expectedVirtualSID := sharePersonalVirtualServerID(userID, "/alice")
	if loginPayload.ServerId != expectedVirtualSID || loginPayload.User.ServerId != expectedVirtualSID {
		t.Fatalf("virtual ServerId mismatch: got (%q, %q), want %q", loginPayload.ServerId, loginPayload.User.ServerId, expectedVirtualSID)
	}
	if loginPayload.AccessToken != "tok-alpha-aaa" || loginPayload.User.Id != "uid-alpha-111" {
		t.Fatalf("unexpected login identity: %+v", loginPayload)
	}

	// /System/Info/Public before switch also returns the same virtual ServerId.
	infoResA, err := http.Get(panel.URL + "/alice/System/Info/Public")
	if err != nil {
		t.Fatalf("GET System/Info/Public A: %v", err)
	}
	var infoA map[string]any
	_ = json.NewDecoder(infoResA.Body).Decode(&infoA)
	_ = infoResA.Body.Close()
	if infoA["Id"] != expectedVirtualSID || infoA["ServerId"] != expectedVirtualSID {
		t.Fatalf("System/Info/Public A = %+v, want virtual %q", infoA, expectedVirtualSID)
	}

	// 2. Drag Site B to #1.
	if _, err := app.db.db.Exec(`UPDATE share_user_sites SET sort_order=1 WHERE share_user_id=? AND site_id=?`, userID, siteB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE share_user_sites SET sort_order=2 WHERE share_user_id=? AND site_id=?`, userID, siteA.ID); err != nil {
		t.Fatal(err)
	}
	app.reloadShareUsers()
	revoked, draining, seamless := app.pm.shares.reconcileSiteSwitch(userID, false)
	if revoked != 0 || draining != 0 || seamless != 1 {
		t.Fatalf("reconcile = (%d,%d,%d), want (0,0,1)", revoked, draining, seamless)
	}

	// 3. Fire 4 concurrent client refresh requests carrying OLD Site A token & OLD Site A UserId!
	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, panel.URL+"/alice/Users/uid-alpha-111/Views?UserId=uid-alpha-111", nil)
			req.Header.Set("X-Emby-Token", "tok-alpha-aaa")
			req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Hills", Device="PC", DeviceId="dev-1", Version="1.0", Token="tok-alpha-aaa", UserId="uid-alpha-111"`)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			raw, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				errs <- "status=" + res.Status + " body=" + string(raw)
				return
			}
			if !strings.Contains(string(raw), `"server":"beta"`) || !strings.Contains(string(raw), `/Users/uid-beta-999/Views`) || !strings.Contains(string(raw), `UserId=uid-beta-999`) {
				errs <- "unexpected body=" + string(raw)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatalf("concurrent seamless refresh failed: %s", msg)
	}
	if got := embyB.loginCount.Load(); got != 1 {
		t.Fatalf("Site B silent login count = %d, want 1 (serialized)", got)
	}
	embyB.mu.Lock()
	gotAuth := embyB.lastAuthHdr
	embyB.mu.Unlock()
	if !strings.Contains(gotAuth, `Token="tok-beta-bbb"`) || !strings.Contains(gotAuth, `UserId="uid-beta-999"`) {
		t.Fatalf("Authorization header not rewritten: %q", gotAuth)
	}

	// 4. User identity endpoint maps upstream UID back to client's cached UID and preserves virtual ServerId.
	userReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/alice/Users/uid-alpha-111", nil)
	userReq.Header.Set("X-Emby-Token", "tok-alpha-aaa")
	userRes, err := http.DefaultClient.Do(userReq)
	if err != nil {
		t.Fatalf("GET /alice/Users/uid-alpha-111: %v", err)
	}
	var userObj map[string]any
	_ = json.NewDecoder(userRes.Body).Decode(&userObj)
	_ = userRes.Body.Close()
	if userRes.StatusCode != http.StatusOK || userObj["Id"] != "uid-alpha-111" || userObj["ServerId"] != expectedVirtualSID {
		t.Fatalf("user identity response = %d %+v", userRes.StatusCode, userObj)
	}

	// 5. /System/Info/Public after switch keeps the exact same virtual ServerId while hitting Site B.
	infoResB, err := http.Get(panel.URL + "/alice/System/Info/Public")
	if err != nil {
		t.Fatalf("GET System/Info/Public B: %v", err)
	}
	var infoB map[string]any
	_ = json.NewDecoder(infoResB.Body).Decode(&infoB)
	_ = infoResB.Body.Close()
	if infoB["ServerName"] != "Emby-beta" || infoB["Id"] != expectedVirtualSID || infoB["ServerId"] != expectedVirtualSID {
		t.Fatalf("System/Info/Public B = %+v, want Emby-beta with virtual %q", infoB, expectedVirtualSID)
	}

	// 6. Old Site A upstream token should be logged out asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	loggedOut := false
	for time.Now().Before(deadline) {
		embyA.mu.Lock()
		for _, tok := range embyA.logoutTokens {
			if tok == "tok-alpha-aaa" {
				loggedOut = true
			}
		}
		embyA.mu.Unlock()
		if loggedOut {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !loggedOut {
		t.Fatal("expected old Site A token to be logged out asynchronously after seamless switch")
	}
}

func TestShareMaxConcurrentStreamsLimit(t *testing.T) {
	app := newTestApp(t)
	var tokenSeq atomic.Int32
	holdRelease := make(chan struct{})
	streamStarted := make(chan struct{}, 8)
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/users/authenticatebyname"):
			n := tokenSeq.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-dev-" + string(rune('0'+n)),
				"User":        map[string]any{"Id": "uid-1"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions/playing"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions/playing/stopped"):
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(p, "/videos/"):
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("video-bytes"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			select {
			case streamStarted <- struct{}{}:
			default:
			}
			<-holdRelease
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(emby.Close)

	site := startTaggedShareSite(t, app, "Main", "main", emby.URL)

	// Validation check: >100 rejected.
	if _, err := app.normalizeShareUserInput(ShareUserInput{
		Username:             "badlimit",
		Password:             "pass1234",
		MaxConcurrentStreams: 101,
		Sites:                []ShareUserSiteInput{{SiteID: site.ID}},
	}, 0, true); err == nil {
		t.Fatal("expected max_concurrent_streams=101 to fail validation")
	}

	// Create user with max_devices=5, max_concurrent_streams=1.
	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:             "family",
		Password:             "pass1234",
		RoutePrefix:          "family",
		MaxDevices:           5,
		MaxConcurrentStreams: 1,
		Sites:                []ShareUserSiteInput{{SiteID: site.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	userID, err := app.saveShareUser(0, input)
	if err != nil {
		t.Fatalf("saveShareUser: %v", err)
	}
	app.reloadShareUsers()

	// Verify DB persistence & shareText formatting.
	stored, err := app.db.getShareUser(userID)
	if err != nil || stored.MaxConcurrentStreams != 1 {
		t.Fatalf("stored MaxConcurrentStreams = %d, err = %v", stored.MaxConcurrentStreams, err)
	}
	txt, err := app.shareUserText(stored, "https://emby.example.com")
	if err != nil || !strings.Contains(txt, "同时播放限制：最多 1 路") {
		t.Fatalf("shareUserText missing stream limit:\n%s", txt)
	}

	panel := newPersonalSharePanelServer(t, app)
	loginDevice := func(devID string) string {
		req, _ := http.NewRequest(http.MethodPost, panel.URL+"/family/Users/AuthenticateByName", strings.NewReader(`{"Username":"family","Pw":"pass1234"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Test", Device="`+devID+`", DeviceId="`+devID+`", Version="1.0"`)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("login %s: %v", devID, err)
		}
		var out struct {
			AccessToken string `json:"AccessToken"`
		}
		_ = json.NewDecoder(res.Body).Decode(&out)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || out.AccessToken == "" {
			t.Fatalf("login %s failed: %d", devID, res.StatusCode)
		}
		return out.AccessToken
	}

	tokDev1 := loginDevice("tv-livingroom")
	tokDev2 := loginDevice("phone-alice")

	// Start video stream on Device 1.
	streamDone := make(chan int, 2)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, panel.URL+"/family/Videos/10/stream.mkv", nil)
		req.Header.Set("X-Emby-Token", tokDev1)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			streamDone <- 0
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		streamDone <- res.StatusCode
	}()
	select {
	case <-streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Device 1 stream")
	}

	// Parallel Range request from the SAME Device 1 must NOT be blocked!
	go func() {
		req, _ := http.NewRequest(http.MethodGet, panel.URL+"/family/Videos/10/stream.mkv", nil)
		req.Header.Set("X-Emby-Token", tokDev1)
		req.Header.Set("Range", "bytes=1024-2048")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			streamDone <- 0
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		streamDone <- res.StatusCode
	}()
	select {
	case <-streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Device 1 parallel range stream")
	}

	// Verify active_streams count is 1 (deduplicated per device).
	if active := app.pm.shares.activeStreamCountForUser(userID, "", time.Now()); active != 1 {
		t.Fatalf("activeStreamCountForUser = %d, want 1", active)
	}

	// Device 2 attempting to play must receive 429 Too Many Requests.
	dev2PlayReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/family/Videos/20/stream.mkv", nil)
	dev2PlayReq.Header.Set("X-Emby-Token", tokDev2)
	dev2PlayRes, err := http.DefaultClient.Do(dev2PlayReq)
	if err != nil {
		t.Fatalf("Device 2 stream req: %v", err)
	}
	_ = dev2PlayRes.Body.Close()
	if dev2PlayRes.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Device 2 stream status = %d, want 429", dev2PlayRes.StatusCode)
	}

	// Device 2 is NOT signed out: browsing /Items still returns 200 OK.
	dev2BrowseReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/family/Items", nil)
	dev2BrowseReq.Header.Set("X-Emby-Token", tokDev2)
	dev2BrowseRes, err := http.DefaultClient.Do(dev2BrowseReq)
	if err != nil {
		t.Fatalf("Device 2 browse: %v", err)
	}
	_ = dev2BrowseRes.Body.Close()
	if dev2BrowseRes.StatusCode != http.StatusOK {
		t.Fatalf("Device 2 browse after 429 = %d, want 200", dev2BrowseRes.StatusCode)
	}

	// Stop Device 1 streams and send /Sessions/Playing/Stopped.
	close(holdRelease)
	for range 2 {
		if code := <-streamDone; code != http.StatusOK {
			t.Fatalf("Device 1 stream returned %d, want 200", code)
		}
	}
	stopReq, _ := http.NewRequest(http.MethodPost, panel.URL+"/family/Sessions/Playing/Stopped", strings.NewReader(`{}`))
	stopReq.Header.Set("X-Emby-Token", tokDev1)
	stopRes, err := http.DefaultClient.Do(stopReq)
	if err != nil || stopRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Device 1 Stopped = %v", err)
	}
	_ = stopRes.Body.Close()

	// Now Device 2 can immediately play (200 OK).
	dev2RetryReq, _ := http.NewRequest(http.MethodGet, panel.URL+"/family/Videos/20/stream.mkv", nil)
	dev2RetryReq.Header.Set("X-Emby-Token", tokDev2)
	dev2RetryRes, err := http.DefaultClient.Do(dev2RetryReq)
	if err != nil {
		t.Fatalf("Device 2 retry stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, dev2RetryRes.Body)
	_ = dev2RetryRes.Body.Close()
	if dev2RetryRes.StatusCode != http.StatusOK {
		t.Fatalf("Device 2 stream after Device 1 stopped = %d, want 200", dev2RetryRes.StatusCode)
	}
}

func TestSeamlessSiteSwitchPassthroughForwardsClientUserAgent(t *testing.T) {
	app := newTestApp(t)
	var mu sync.Mutex
	var loginUA, loginAuth, logoutUA string

	embyA := newFakeSeamlessEmby(t, "alpha", "uid-alpha-1", "srv-alpha", "tok-alpha", false)
	embyB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		// Simulate strict nginx/WAF upstreams (such as rfc.uhdnow.com) that return
		// 403 Forbidden whenever a request carries Go's default HTTP client UA.
		if ua == "" || strings.HasPrefix(strings.ToLower(ua), "go-http-client/") {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		p := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/users/authenticatebyname"):
			mu.Lock()
			loginUA = ua
			loginAuth = r.Header.Get("X-Emby-Authorization")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-beta-strict",
				"ServerId":    "srv-beta-strict",
				"User":        map[string]any{"Id": "uid-beta-strict", "Name": "real-beta"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions/logout"):
			mu.Lock()
			logoutUA = ua
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			if r.Header.Get("X-Emby-Token") != "tok-beta-strict" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"server": "beta-strict", "ua": ua})
		}
	}))
	t.Cleanup(embyB.Close)

	siteA := startTaggedShareSite(t, app, "Alpha", "alpha", embyA.server.URL)
	portB := freePort(t)
	releasePort(portB)
	siteB, err := app.db.CreateSiteRecord(Site{
		Name:         "StrictBeta",
		ListenPort:   portB,
		IngressMode:  ingressModePort,
		TargetURL:    embyB.URL,
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       "passthrough",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord(StrictBeta): %v", err)
	}
	if _, err := app.db.saveSiteAccount(siteB.ID, 0, SiteAccountInput{
		Username: "real-beta",
		Password: "pass-beta",
	}); err != nil {
		t.Fatalf("saveSiteAccount(StrictBeta): %v", err)
	}
	if err := app.pm.StartSite(*siteB); err != nil {
		t.Fatalf("StartSite(StrictBeta): %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(siteB.ID) })

	input, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "bob",
		Password:    "pass1234",
		RoutePrefix: "bob",
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

	// 1. Login on Site A.
	resp, _ := doShareLogin(t, panel.URL+"/bob", "bob", "pass1234")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial login = %d", resp.StatusCode)
	}

	// 2. Switch primary site to Site B (passthrough + strict WAF).
	if _, err := app.db.db.Exec(`UPDATE share_user_sites SET sort_order=1 WHERE share_user_id=? AND site_id=?`, userID, siteB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE share_user_sites SET sort_order=2 WHERE share_user_id=? AND site_id=?`, userID, siteA.ID); err != nil {
		t.Fatal(err)
	}
	app.reloadShareUsers()
	app.pm.shares.reconcileSiteSwitch(userID, false)

	// 3. Refresh from a real player UA (Hills Windows/1.7.2): must pass WAF and succeed.
	const playerUA = "Hills Windows/1.7.2 (windows; 19041.vb_release.191206-1406)"
	req, _ := http.NewRequest(http.MethodGet, panel.URL+"/bob/Users/uid-alpha-1/Views", nil)
	req.Header.Set("User-Agent", playerUA)
	req.Header.Set("X-Emby-Token", "tok-alpha")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Hills", Device="PC", DeviceId="dev-hills", Version="1.7.2", Token="tok-alpha"`)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seamless switch to strict site: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seamless switch to strict passthrough site failed: %d %s", res.StatusCode, string(raw))
	}
	mu.Lock()
	gotLoginUA := loginUA
	gotLoginAuth := loginAuth
	mu.Unlock()
	if gotLoginUA != playerUA {
		t.Fatalf("silent login User-Agent = %q, want %q", gotLoginUA, playerUA)
	}
	if !strings.Contains(gotLoginAuth, `Version="1.7.2"`) {
		t.Fatalf("silent login X-Emby-Authorization = %q, want Version=\"1.7.2\"", gotLoginAuth)
	}

	// 4. Verify background logout from Site B never leaks Go-http-client UA either.
	app.pm.shares.logoutUpstream(siteB.ID, "tok-beta-strict", 0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := logoutUA
		mu.Unlock()
		if got != "" {
			if strings.HasPrefix(strings.ToLower(got), "go-http-client/") {
				t.Fatalf("logout leaked Go-http-client UA: %q", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected Site B upstream logout request to pass WAF with non-Go User-Agent")
}
