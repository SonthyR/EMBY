package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInPlayerFavoriteSiteSwitchAndEdgeFullJPCO(t *testing.T) {
	app := newTestApp(t)
	edgeRT := newEdgeNodeRuntime(app.db, app.pm, nil)
	app.pm.SetEdgeRuntime(edgeRT)
	embyUHD := newFakeSeamlessEmby(t, "uhd", "uid-uhd", "srv-uhd", "tok-uhd", false)
	embySNTP := newFakeSeamlessEmby(t, "sntp", "uid-sntp", "srv-sntp", "tok-sntp", false)

	// Simulate RFC-UHD (edge_full pinned to JPCO): reject any request that does NOT
	// carry the mock JPCO edge egress header so a direct Master IP leak fails loudly.
	var jpcoEgressHits atomic.Int32
	var directMasterHits atomic.Int32
	embyRFC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-JPCO-Egress") != "yes" {
			directMasterHits.Add(1)
			http.Error(w, "master ip leaked", http.StatusForbidden)
			return
		}
		jpcoEgressHits.Add(1)
		p := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/users/authenticatebyname"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-rfc-jpco",
				"ServerId":    "srv-rfc",
				"User":        map[string]any{"Id": "uid-rfc", "Name": "real-rfc"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions/logout"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(p, "/views"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"rfc-movies","Name":"RFC 电影"}],"TotalRecordCount":1}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"server":"rfc"}`))
		}
	}))
	t.Cleanup(embyRFC.Close)

	// Create real edge node in DB and spin up a live edgeAgent serving /_yundu/egress.
	rec := httptest.NewRecorder()
	app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"RFC-JPCO","public_port":19090,"public_ip":"127.0.0.1","tls_mode":"http"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("createEdge RFC-JPCO status=%d body=%s", rec.Code, rec.Body.String())
	}
	var jpcoNode EdgeNode
	if err := json.Unmarshal(rec.Body.Bytes(), &jpcoNode); err != nil {
		t.Fatalf("decode edge node: %v", err)
	}
	edgeRT.mu.RLock()
	jpcoToken := edgeRT.nodes[jpcoNode.ID].tokenPlain
	edgeRT.mu.RUnlock()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: "http://127.0.0.1:19999",
		NodeID:    jpcoNode.ID,
		Token:     jpcoToken,
		Port:      19090,
	})
	stampJPCO := func(base http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			cloned.Header.Set("X-Test-JPCO-Egress", "yes")
			return base.RoundTrip(cloned)
		})
	}
	agent.upstreamClient.Transport = stampJPCO(agent.upstreamClient.Transport)
	agent.egressClient.Transport = stampJPCO(agent.egressClient.Transport)
	jpcoAgent := httptest.NewServer(agent.handler())
	t.Cleanup(jpcoAgent.Close)

	agentURL, _ := url.Parse(jpcoAgent.URL)
	agentPort, _ := strconv.Atoi(agentURL.Port())
	if _, err := app.db.db.Exec(`UPDATE edge_nodes SET public_ip=?, public_port=?, public_host='', tls_mode=? WHERE id=?`, agentURL.Hostname(), agentPort, edgeNodeTLSModeHTTP, jpcoNode.ID); err != nil {
		t.Fatalf("update edge node port: %v", err)
	}
	if err := edgeRT.reload(); err != nil {
		t.Fatalf("reload edgeRT: %v", err)
	}
	setJPCOOnline := func(online bool) {
		edgeRT.mu.Lock()
		live := edgeRT.nodes[jpcoNode.ID]
		live.agentVersion = appVersion
		if online {
			live.lastSeen = time.Now()
		} else {
			live.lastSeen = time.Now().Add(-5 * time.Minute)
		}
		edgeRT.mu.Unlock()
	}
	setJPCOOnline(true)

	siteUHD := startTaggedShareSite(t, app, "UHD", "uhd", embyUHD.server.URL)
	siteSNTP := startTaggedShareSite(t, app, "SNTP Media Lite 公益计划", "sntp", embySNTP.server.URL)

	// Start RFC-UHD with edge_full pinned to jpcoNode.ID.
	portRFC := freePort(t)
	releasePort(portRFC)
	siteRFC, err := app.db.CreateSiteRecord(Site{
		Name:             "RFC-UHD",
		ListenPort:       portRFC,
		IngressMode:      ingressModePort,
		TargetURL:        embyRFC.URL,
		PlaybackMode:     "direct",
		StreamHosts:      "[]",
		UAMode:           "passthrough",
		EdgeRoutingMode:  edgeRoutingModeEdgeFull,
		EdgePinnedNodeID: jpcoNode.ID,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord(RFC-UHD): %v", err)
	}
	if _, err := app.db.saveSiteAccount(siteRFC.ID, 0, SiteAccountInput{
		Username: "real-rfc",
		Password: "pass-rfc",
	}); err != nil {
		t.Fatalf("saveSiteAccount(RFC-UHD): %v", err)
	}
	if err := app.pm.StartSite(*siteRFC); err != nil {
		t.Fatalf("StartSite(RFC-UHD): %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(siteRFC.ID) })

	// Create multi-site personal-route user (UHD -> RFC-UHD -> SNTP) and a single-site user.
	multiInput, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "sonthy",
		Password:    "pass1234",
		RoutePrefix: "sonthy",
		Sites:       []ShareUserSiteInput{{SiteID: siteUHD.ID}, {SiteID: siteRFC.ID}, {SiteID: siteSNTP.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize multi: %v", err)
	}
	if _, err := app.saveShareUser(0, multiInput); err != nil {
		t.Fatalf("save multi: %v", err)
	}
	singleInput, err := app.normalizeShareUserInput(ShareUserInput{
		Username:    "single",
		Password:    "pass1234",
		RoutePrefix: "single",
		Sites:       []ShareUserSiteInput{{SiteID: siteUHD.ID}},
	}, 0, true)
	if err != nil {
		t.Fatalf("normalize single: %v", err)
	}
	if _, err := app.saveShareUser(0, singleInput); err != nil {
		t.Fatalf("save single: %v", err)
	}
	app.reloadShareUsers()
	panel := newPersonalSharePanelServer(t, app)

	// Override embyUHD and embySNTP /Views via default handler? Note fakeSeamlessEmby returns JSON without Items on /Views,
	// so let's make sure /Users/{uid}/Views returns an Items array for embyUHD & embySNTP by using custom servers below.
	// Wait: let's verify /Views on RFC-UHD (which returns {"Items":[...]}) and also test list/favorite/image endpoints directly.
	resp, _ := doShareLogin(t, panel.URL+"/sonthy", "sonthy", "pass1234")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multi login = %d", resp.StatusCode)
	}

	doJSON := func(method, reqPath, token string) (int, map[string]any, string) {
		t.Helper()
		req, _ := http.NewRequest(method, panel.URL+reqPath, nil)
		req.Header.Set("User-Agent", "CapyPlayer/1.1.8")
		if token != "" {
			req.Header.Set("X-Emby-Token", token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, reqPath, err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return res.StatusCode, parsed, string(raw)
	}

	assertExactlyOneFavorite := func(wantSiteID int64) {
		t.Helper()
		code, obj, raw := doJSON(http.MethodGet, "/sonthy/emby/Users/uid-uhd/Items?ParentId="+shareSwitchLibraryID, "tok-uhd")
		if code != http.StatusOK {
			t.Fatalf("list virtual switch items = %d %s", code, raw)
		}
		items, ok := obj["Items"].([]any)
		if !ok || len(items) != 3 {
			t.Fatalf("expected 3 virtual site items, got %s", raw)
		}
		favCount := 0
		wantItemID := shareSwitchSiteItemID(wantSiteID)
		for _, elem := range items {
			m := elem.(map[string]any)
			ud := m["UserData"].(map[string]any)
			isFav, _ := ud["IsFavorite"].(bool)
			if isFav {
				favCount++
				if m["Id"] != wantItemID {
					t.Fatalf("wrong active favorite: got %v, want %s", m["Id"], wantItemID)
				}
			}
		}
		if favCount != 1 {
			t.Fatalf("expected exactly 1 favorite heart, got %d (%s)", favCount, raw)
		}
	}

	// 1. Initial state: ONLY UHD is favorited (IsFavorite=true).
	assertExactlyOneFavorite(siteUHD.ID)

	// 2. Virtual PNG covers work WITHOUT X-Emby-Token (Emby players often omit token on /Images/).
	imgRes, err := http.Get(panel.URL + "/sonthy/emby/Items/" + shareSwitchLibraryID + "/Images/Primary")
	if err != nil {
		t.Fatalf("GET library PNG: %v", err)
	}
	imgBytes, _ := io.ReadAll(imgRes.Body)
	_ = imgRes.Body.Close()
	if imgRes.StatusCode != http.StatusOK || imgRes.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(imgBytes, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("invalid library PNG cover: status=%d ct=%q len=%d", imgRes.StatusCode, imgRes.Header.Get("Content-Type"), len(imgBytes))
	}

	// 3. Favorite click on RFC-UHD switches primary site locally without hitting RFC-UHD yet.
	code, favObj, raw := doJSON(http.MethodPost, "/sonthy/emby/Users/uid-uhd/FavoriteItems/"+shareSwitchSiteItemID(siteRFC.ID), "tok-uhd")
	if code != http.StatusOK || favObj["IsFavorite"] != true {
		t.Fatalf("favorite switch to RFC-UHD = %d %s", code, raw)
	}

	// 4. Refreshing /Users/{uid}/Views seamlessly hands off to RFC-UHD STRICTLY through JPCO edge node,
	//    and prepends "切服 · RFC-UHD" as Card #1!
	code, viewsObj, raw := doJSON(http.MethodGet, "/sonthy/emby/Users/uid-uhd/Views", "tok-uhd")
	if code != http.StatusOK {
		t.Fatalf("GET /Views after switching to RFC-UHD = %d %s", code, raw)
	}
	if directMasterHits.Load() != 0 {
		t.Fatalf("RFC-UHD received %d direct Master requests (IP leak!)", directMasterHits.Load())
	}
	if jpcoEgressHits.Load() < 2 {
		t.Fatalf("expected JPCO edge egress hits for login + /Views, got %d", jpcoEgressHits.Load())
	}
	viewItems, _ := viewsObj["Items"].([]any)
	if len(viewItems) != 2 {
		t.Fatalf("expected 2 views (switch card + RFC 电影), got %s", raw)
	}
	firstView := viewItems[0].(map[string]any)
	if firstView["Id"] != shareSwitchLibraryID || firstView["Name"] != "切服 · RFC-UHD" {
		t.Fatalf("first view = %+v, want Id=%s Name='切服 · RFC-UHD'", firstView, shareSwitchLibraryID)
	}

	// 5. After switching to RFC-UHD, ONLY RFC-UHD has a red heart (UHD is automatically cleared!).
	assertExactlyOneFavorite(siteRFC.ID)

	// 6. Simulate JPCO going OFFLINE while user is on RFC-UHD: user can STILL click favorite
	//    on SNTP (even via stale-cache DELETE!) locally on Master to escape the offline edge node!
	setJPCOOnline(false)

	code, favObj, raw = doJSON(http.MethodDelete, "/sonthy/emby/Users/uid-uhd/FavoriteItems/"+shareSwitchSiteItemID(siteSNTP.ID), "tok-uhd")
	if code != http.StatusOK || favObj["IsFavorite"] != true {
		t.Fatalf("stale-cache DELETE switch to SNTP while JPCO offline = %d %s", code, raw)
	}
	assertExactlyOneFavorite(siteSNTP.ID)

	// 7. Round-trip back to UHD: still exactly 1 favorite heart!
	code, _, raw = doJSON(http.MethodPost, "/sonthy/emby/Users/uid-uhd/FavoriteItems/"+shareSwitchSiteItemID(siteUHD.ID), "tok-uhd")
	if code != http.StatusOK {
		t.Fatalf("switch back to UHD = %d %s", code, raw)
	}
	assertExactlyOneFavorite(siteUHD.ID)

	// 8. Auxiliary detail endpoints on virtual card return clean 200 (never 500) and PlaybackInfo returns 404.
	code, _, _ = doJSON(http.MethodGet, "/sonthy/emby/Items/"+shareSwitchSiteItemID(siteUHD.ID)+"/Similar", "tok-uhd")
	if code != http.StatusOK {
		t.Fatalf("virtual card /Similar = %d, want 200", code)
	}
	code, _, _ = doJSON(http.MethodPost, "/sonthy/emby/Items/"+shareSwitchSiteItemID(siteUHD.ID)+"/PlaybackInfo", "tok-uhd")
	if code != http.StatusNotFound {
		t.Fatalf("virtual card /PlaybackInfo = %d, want 404", code)
	}

	// 9. Disabling system setting hides virtual switch view immediately.
	setJPCOOnline(true)
	_, _, _ = doJSON(http.MethodPost, "/sonthy/emby/Users/uid-uhd/FavoriteItems/"+shareSwitchSiteItemID(siteRFC.ID), "tok-uhd")
	settings := app.db.currentSystemSettings()
	settings.InPlayerSiteSwitchEnabled = false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatalf("saveSystemSettings(false): %v", err)
	}
	code, viewsObj, raw = doJSON(http.MethodGet, "/sonthy/emby/Users/uid-uhd/Views", "tok-uhd")
	if code != http.StatusOK {
		t.Fatalf("GET /Views with toggle off = %d %s", code, raw)
	}
	viewItems, _ = viewsObj["Items"].([]any)
	if len(viewItems) != 1 {
		t.Fatalf("expected only 1 real view when toggle disabled, got %s", raw)
	}
	if directMasterHits.Load() != 0 {
		t.Fatalf("RFC-UHD received %d direct Master requests (IP leak!)", directMasterHits.Load())
	}

	// 10. Single-site user does NOT get virtual switch items even when toggle is enabled.
	settings.InPlayerSiteSwitchEnabled = true
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatalf("saveSystemSettings(true): %v", err)
	}
	singleResp, _ := doShareLogin(t, panel.URL+"/single", "single", "pass1234")
	if singleResp.StatusCode != http.StatusOK {
		t.Fatalf("single login = %d", singleResp.StatusCode)
	}
	code, _, raw = doJSON(http.MethodGet, "/single/emby/Users/uid-uhd/Items?ParentId="+shareSwitchLibraryID, "tok-uhd")
	if code == http.StatusOK && strings.Contains(raw, shareSwitchSiteIDPrefix) {
		t.Fatal("single-site user should not expose virtual switch library")
	}
}

func TestShortInPlayerSiteName(t *testing.T) {
	cases := map[string]string{
		"UHD":               "UHD",
		"RFC-UHD":           "RFC-UHD",
		"鸢尾":                "鸢尾",
		"SNTP Media Lite 公益计划": "SNTP",
		"超长无空格中文站点名称测试":     "超长无空格中文站点名",
	}
	for in, want := range cases {
		if got := shortInPlayerSiteName(in); got != want {
			t.Errorf("shortInPlayerSiteName(%q) = %q, want %q", in, got, want)
		}
	}
}

func makeSolidTestPNG(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func TestSiteIconAutoFetchCustomUploadAndOuterCardSwitching(t *testing.T) {
	app := newTestApp(t)
	edgeRT := newEdgeNodeRuntime(app.db, app.pm, nil)
	app.pm.SetEdgeRuntime(edgeRT)

	redIcon := makeSolidTestPNG(t, color.NRGBA{R: 230, G: 40, B: 40, A: 255})
	blueIcon := makeSolidTestPNG(t, color.NRGBA{R: 40, G: 120, B: 240, A: 255})
	goldCustomIcon := makeSolidTestPNG(t, color.NRGBA{R: 245, G: 190, B: 20, A: 255})

	// Site A (UHD-like): /Users/{uid}/Images/Primary redirects 301 -> /img/u/default returning redIcon.
	embyA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/users/authenticatebyname"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-icon-a",
				"ServerId":    "srv-a",
				"User":        map[string]any{"Id": "uid-a", "Name": "real-a"},
			})
		case strings.HasSuffix(p, "/users/uid-a/images/primary"):
			http.Redirect(w, r, "/img/u/default", http.StatusMovedPermanently)
		case p == "/img/u/default":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(redIcon)
		case strings.HasSuffix(p, "/views"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"a-mov","Name":"A 电影"}],"TotalRecordCount":1}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(embyA.Close)

	// Site B (RFC-UHD-like, edge_full pinned to JPCO): MUST only be reached via JPCO edge tunnel!
	var directMasterHits atomic.Int32
	embyB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-JPCO-Icon") != "yes" {
			directMasterHits.Add(1)
			http.Error(w, "master ip leaked", http.StatusForbidden)
			return
		}
		p := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/users/authenticatebyname"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-icon-b",
				"ServerId":    "srv-b",
				"User":        map[string]any{"Id": "uid-b", "Name": "real-b"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/sessions/logout"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(p, "/users/uid-b/images/primary"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(blueIcon)
		case strings.HasSuffix(p, "/views"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"b-mov","Name":"B 电影"}],"TotalRecordCount":1}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(embyB.Close)

	// Spin up real JPCO edgeAgent.
	rec := httptest.NewRecorder()
	app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"JPCO-Icon","public_port":19090,"public_ip":"127.0.0.1","tls_mode":"http"}`)))
	var jpco EdgeNode
	_ = json.Unmarshal(rec.Body.Bytes(), &jpco)
	edgeRT.mu.RLock()
	jpcoTok := edgeRT.nodes[jpco.ID].tokenPlain
	edgeRT.mu.RUnlock()
	agent := newEdgeAgent(edgeAgentConfig{MasterURL: "http://127.0.0.1:19999", NodeID: jpco.ID, Token: jpcoTok, Port: 19090})
	stamp := func(base http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			cloned.Header.Set("X-Test-JPCO-Icon", "yes")
			return base.RoundTrip(cloned)
		})
	}
	agent.upstreamClient.Transport = stamp(agent.upstreamClient.Transport)
	agent.egressClient.Transport = stamp(agent.egressClient.Transport)
	agentSrv := httptest.NewServer(agent.handler())
	t.Cleanup(agentSrv.Close)
	au, _ := url.Parse(agentSrv.URL)
	ap, _ := strconv.Atoi(au.Port())
	_, _ = app.db.db.Exec(`UPDATE edge_nodes SET public_ip=?, public_port=?, public_host='', tls_mode=? WHERE id=?`, au.Hostname(), ap, edgeNodeTLSModeHTTP, jpco.ID)
	_ = edgeRT.reload()
	edgeRT.mu.Lock()
	edgeRT.nodes[jpco.ID].lastSeen = time.Now()
	edgeRT.nodes[jpco.ID].agentVersion = appVersion
	edgeRT.mu.Unlock()

	siteA := startTaggedShareSite(t, app, "UHD", "a", embyA.URL)
	portB := freePort(t)
	releasePort(portB)
	siteB, err := app.db.CreateSiteRecord(Site{
		Name:             "RFC-UHD",
		ListenPort:       portB,
		IngressMode:      ingressModePort,
		TargetURL:        embyB.URL,
		PlaybackMode:     "direct",
		StreamHosts:      "[]",
		UAMode:           "passthrough",
		EdgeRoutingMode:  edgeRoutingModeEdgeFull,
		EdgePinnedNodeID: jpco.ID,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord(B): %v", err)
	}
	_, _ = app.db.saveSiteAccount(siteB.ID, 0, SiteAccountInput{Username: "real-b", Password: "pass-b"})
	if err := app.pm.StartSite(*siteB); err != nil {
		t.Fatalf("StartSite(B): %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(siteB.ID) })

	input, _ := app.normalizeShareUserInput(ShareUserInput{
		Username:    "iconuser",
		Password:    "pass1234",
		RoutePrefix: "iconuser",
		Sites:       []ShareUserSiteInput{{SiteID: siteA.ID}, {SiteID: siteB.ID}},
	}, 0, true)
	_, _ = app.saveShareUser(0, input)
	app.reloadShareUsers()
	panel := newPersonalSharePanelServer(t, app)

	// 1. Trigger admin icon fetch on Site A (follows 301 -> /img/u/default) & Site B (strictly via JPCO).
	fetchRecA := httptest.NewRecorder()
	app.handleSiteByID(fetchRecA, httptest.NewRequest(http.MethodPost, "/api/sites/"+strconv.FormatInt(siteA.ID, 10)+"/icon/fetch", nil))
	if fetchRecA.Code != http.StatusOK {
		t.Fatalf("fetch icon A = %d %s", fetchRecA.Code, fetchRecA.Body.String())
	}
	fetchRecB := httptest.NewRecorder()
	app.handleSiteByID(fetchRecB, httptest.NewRequest(http.MethodPost, "/api/sites/"+strconv.FormatInt(siteB.ID, 10)+"/icon/fetch", nil))
	if fetchRecB.Code != http.StatusOK {
		t.Fatalf("fetch icon B = %d %s", fetchRecB.Code, fetchRecB.Body.String())
	}
	if directMasterHits.Load() != 0 {
		t.Fatalf("Site B received %d direct Master requests during icon fetch (IP leak!)", directMasterHits.Load())
	}

	// 2. Login on Site A; outermost "切服 · UHD" card uses Site A's icon (red-tinted center).
	resp, _ := doShareLogin(t, panel.URL+"/iconuser", "iconuser", "pass1234")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	getViews := func() (string, string) {
		req, _ := http.NewRequest(http.MethodGet, panel.URL+"/iconuser/emby/Users/uid-a/Views", nil)
		req.Header.Set("X-Emby-Token", "tok-icon-a")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /Views: %v", err)
		}
		var out struct {
			Items []struct {
				Name      string            `json:"Name"`
				ImageTags map[string]string `json:"ImageTags"`
			} `json:"Items"`
		}
		_ = json.NewDecoder(res.Body).Decode(&out)
		_ = res.Body.Close()
		if len(out.Items) == 0 {
			t.Fatal("empty /Views")
		}
		return out.Items[0].Name, out.Items[0].ImageTags["Primary"]
	}
	fetchOuterCard := func() []byte {
		res, err := http.Get(panel.URL + "/iconuser/emby/Items/" + shareSwitchLibraryID + "/Images/Primary")
		if err != nil {
			t.Fatalf("GET outer cover: %v", err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || !bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}) {
			t.Fatalf("bad outer cover: status=%d len=%d", res.StatusCode, len(raw))
		}
		return raw
	}

	nameA, tagA := getViews()
	outerPNGA := fetchOuterCard()
	if nameA != "切服 · UHD" {
		t.Fatalf("initial outer view name = %q, want '切服 · UHD'", nameA)
	}

	// 3. Switch via heart to Site B (RFC-UHD): outer card name, ImageTags.Primary, AND rendered PNG bytes all change!
	favReq, _ := http.NewRequest(http.MethodPost, panel.URL+"/iconuser/emby/Users/uid-a/FavoriteItems/"+shareSwitchSiteItemID(siteB.ID), nil)
	favReq.Header.Set("X-Emby-Token", "tok-icon-a")
	favRes, err := http.DefaultClient.Do(favReq)
	if err != nil || favRes.StatusCode != http.StatusOK {
		t.Fatalf("favorite switch to B failed: %v", err)
	}
	_ = favRes.Body.Close()

	nameB, tagB := getViews()
	outerPNGB := fetchOuterCard()
	if nameB != "切服 · RFC-UHD" {
		t.Fatalf("switched outer view name = %q, want '切服 · RFC-UHD'", nameB)
	}
	if tagB == tagA {
		t.Fatalf("expected outer card ImageTags.Primary to change after switching sites, stayed %q", tagA)
	}
	if bytes.Equal(outerPNGA, outerPNGB) {
		t.Fatal("expected outer card PNG bytes to switch from Site A's icon to Site B's icon")
	}

	// 4. Inner cards for Site A and Site B have distinct rendered PNGs matching their own icons.
	getSiteCard := func(sid int64) []byte {
		res, err := http.Get(panel.URL + "/iconuser/emby/Items/" + shareSwitchSiteItemID(sid) + "/Images/Primary")
		if err != nil {
			t.Fatalf("GET site card %d: %v", sid, err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		return raw
	}
	if bytes.Equal(getSiteCard(siteA.ID), getSiteCard(siteB.ID)) {
		t.Fatal("expected inner site cards A and B to display each server's own distinct icon")
	}

	// 5. Custom upload overrides auto icon, and DELETE cleanly restores auto icon.
	upRec := httptest.NewRecorder()
	upReq := httptest.NewRequest(http.MethodPut, "/api/sites/"+strconv.FormatInt(siteB.ID, 10)+"/icon", bytes.NewReader(goldCustomIcon))
	app.handleSiteByID(upRec, upReq)
	if upRec.Code != http.StatusOK {
		t.Fatalf("upload custom icon = %d %s", upRec.Code, upRec.Body.String())
	}
	_, tagBCustom := getViews()
	if tagBCustom == tagB {
		t.Fatal("expected ImageTags.Primary to update when custom icon is uploaded")
	}
	delRec := httptest.NewRecorder()
	app.handleSiteByID(delRec, httptest.NewRequest(http.MethodDelete, "/api/sites/"+strconv.FormatInt(siteB.ID, 10)+"/icon", nil))
	if delRec.Code != http.StatusOK || !strings.Contains(delRec.Body.String(), `"source":"auto"`) {
		t.Fatalf("delete custom icon should revert to auto: %d %s", delRec.Code, delRec.Body.String())
	}
	if directMasterHits.Load() != 0 {
		t.Fatalf("Site B received %d direct Master requests (IP leak!)", directMasterHits.Load())
	}
}
