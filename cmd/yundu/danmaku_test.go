package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setupDanmakuTestApp(t *testing.T) (*App, *Site) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "danmaku.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x55}, 32))
	edgeRT := newEdgeNodeRuntime(db, pm, nil)
	pm.SetEdgeRuntime(edgeRT)
	t.Cleanup(func() { pm.GracefulShutdown(context.Background()) })

	app := &App{
		db:     db,
		dbPath: dbPath,
		pm:     pm,
	}

	site, err := db.CreateSiteRecord(Site{
		Name:        "MainEmby",
		IngressMode: ingressModePath,
		PathPrefix:  "/main",
		TargetURL:   "http://127.0.0.1:8096",
		Enabled:     true,
		UAMode:      "passthrough",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	return app, site
}

func createDanmakuTestShareUser(t *testing.T, app *App, siteID int64, username string, expiresAt string, enabled bool, quota int64, danmakuIDs []int64) ShareUser {
	t.Helper()
	payload := map[string]any{
		"username":           username,
		"password":           "pass1234",
		"enabled":            enabled,
		"expires_at":         expiresAt,
		"traffic_quota":      quota,
		"sites":              []map[string]any{{"site_id": siteID, "site_account_id": 0}},
		"danmaku_source_ids": danmakuIDs,
	}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "https://emby.example.com/api/share-users", bytes.NewReader(raw))
	req.Host = "emby.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	app.handleShareUsers(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create share user %s status=%d body=%s", username, rec.Code, rec.Body.String())
	}
	var user ShareUser
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatalf("decode share user: %v", err)
	}
	return user
}

func TestDanmakuReservedPathPrefix(t *testing.T) {
	if _, err := normalizePathPrefix("/dm"); err == nil {
		t.Fatalf("expected /dm to be rejected as reserved path prefix")
	}
}

func TestDanmakuCRUDAndLifecycleEndToEnd(t *testing.T) {
	app, site := setupDanmakuTestApp(t)

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		bodyBytes, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Echo", r.Method+":"+r.URL.RequestURI())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"method": r.Method,
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
			"body":   string(bodyBytes),
		})
	}))
	defer upstream.Close()

	// 1. Create danmaku source with an embedded secret prefix path (e.g. /secret-key)
	createBody, _ := json.Marshal(map[string]any{
		"name":           "赠送弹幕库",
		"upstream_url":   upstream.URL + "/secret-provider-key",
		"egress_node_id": 0,
		"enabled":        true,
		"note":           "仅限会员",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/danmaku-sources", bytes.NewReader(createBody))
	createRec := httptest.NewRecorder()
	app.handleDanmakuSources(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create danmaku source status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var src DanmakuSource
	if err := json.Unmarshal(createRec.Body.Bytes(), &src); err != nil {
		t.Fatalf("decode danmaku source: %v", err)
	}
	if src.ID <= 0 || src.Name != "赠送弹幕库" || !src.Enabled || !src.EgressOnline {
		t.Fatalf("unexpected created source: %+v", src)
	}

	// 2. Test connectivity via /api/danmaku-sources/{id}/test
	testReq := httptest.NewRequest(http.MethodPost, "/api/danmaku-sources/"+strconv.FormatInt(src.ID, 10)+"/test", nil)
	testRec := httptest.NewRecorder()
	app.handleDanmakuSourceByID(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test danmaku source status=%d body=%s", testRec.Code, testRec.Body.String())
	}
	var testRes DanmakuTestResult
	if err := json.Unmarshal(testRec.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("decode test result: %v", err)
	}
	if !testRes.OK || testRes.StatusCode != http.StatusOK {
		t.Fatalf("expected test OK 200, got %+v", testRes)
	}

	// 3. Create active user A & user B (assigned), unassigned user C, plus expired/disabled/quota users
	futureDate := time.Now().AddDate(0, 1, 0).Format("2006-01-02")
	pastDate := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	assignedIDs := []int64{src.ID}
	userA := createDanmakuTestShareUser(t, app, site.ID, "alice", futureDate, true, 0, assignedIDs)
	userB := createDanmakuTestShareUser(t, app, site.ID, "bob", futureDate, true, 0, assignedIDs)
	userUnassigned := createDanmakuTestShareUser(t, app, site.ID, "charlie", futureDate, true, 0, []int64{})
	userExpired := createDanmakuTestShareUser(t, app, site.ID, "expired_u", pastDate, true, 0, assignedIDs)
	userDisabled := createDanmakuTestShareUser(t, app, site.ID, "disabled_u", futureDate, false, 0, assignedIDs)
	userQuota := createDanmakuTestShareUser(t, app, site.ID, "quota_u", futureDate, true, 100, assignedIDs)
	if _, err := app.db.db.Exec(`UPDATE share_users SET traffic_used=200 WHERE id=?`, userQuota.ID); err != nil {
		t.Fatalf("update quota usage: %v", err)
	}
	app.reloadShareUsers()

	// Verify unassigned active user receives ZERO danmaku_sources in /api/me and 403 on /dm/
	storedUnassigned, err := app.db.getShareUser(userUnassigned.ID)
	if err != nil {
		t.Fatalf("getShareUser charlie: %v", err)
	}
	meReqCheck := httptest.NewRequest(http.MethodGet, "https://emby.example.com/api/me", nil)
	meReqCheck.Host = "emby.example.com"
	if mvC := app.buildShareSelfView(storedUnassigned, meReqCheck); len(mvC.DanmakuSources) != 0 {
		t.Fatalf("expected 0 danmaku sources for unassigned user, got %+v", mvC.DanmakuSources)
	}
	unassignedRec := httptest.NewRecorder()
	app.handleDanmakuProxy(unassignedRec, httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+storedUnassigned.danmakuToken+"/api/v2/comment/1", nil))
	if unassignedRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unassigned user, got %d body=%s", unassignedRec.Code, unassignedRec.Body.String())
	}

	// 4. Verify /api/me exposes personal danmaku URL for active user A without leaking upstream_url
	storedA, err := app.db.getShareUser(userA.ID)
	if err != nil {
		t.Fatalf("getShareUser A: %v", err)
	}
	meReq := httptest.NewRequest(http.MethodGet, "https://emby.example.com/api/me", nil)
	meReq.Host = "emby.example.com"
	meViewA := app.buildShareSelfView(storedA, meReq)
	if len(meViewA.DanmakuSources) != 1 {
		t.Fatalf("expected 1 danmaku source for active user, got %+v", meViewA.DanmakuSources)
	}
	dmURLA := meViewA.DanmakuSources[0].URL
	if !strings.Contains(dmURLA, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+storedA.danmakuToken) {
		t.Fatalf("unexpected personal danmaku url: %s", dmURLA)
	}
	rawMeJSON, _ := json.Marshal(meViewA)
	if strings.Contains(string(rawMeJSON), "secret-provider-key") || strings.Contains(string(rawMeJSON), upstream.URL) {
		t.Fatalf("upstream_url leaked in /api/me payload: %s", string(rawMeJSON))
	}

	// Verify expired/disabled/quota users receive ZERO danmaku_sources in /api/me
	for _, uid := range []int64{userExpired.ID, userDisabled.ID, userQuota.ID} {
		su, err := app.db.getShareUser(uid)
		if err != nil {
			t.Fatalf("getShareUser %d: %v", uid, err)
		}
		mv := app.buildShareSelfView(su, meReq)
		if len(mv.DanmakuSources) != 0 {
			t.Fatalf("expected 0 danmaku sources for inactive user %s (status=%s), got %+v", su.Username, mv.Status, mv.DanmakuSources)
		}
	}

	// 5. Proxy GET + POST requests for active user A
	dmGetPath := "/dm/" + strconv.FormatInt(src.ID, 10) + "/" + storedA.danmakuToken + "/api/v2/comment/12345?withRelated=true&chConvert=1"
	dmGetReq := httptest.NewRequest(http.MethodGet, dmGetPath, nil)
	dmGetRec := httptest.NewRecorder()
	app.handleDanmakuProxy(dmGetRec, dmGetReq)
	if dmGetRec.Code != http.StatusOK {
		t.Fatalf("active user GET danmaku status=%d body=%s", dmGetRec.Code, dmGetRec.Body.String())
	}
	if dmGetRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected CORS header *, got %q", dmGetRec.Header().Get("Access-Control-Allow-Origin"))
	}
	var echo map[string]any
	if err := json.Unmarshal(dmGetRec.Body.Bytes(), &echo); err != nil {
		t.Fatalf("decode proxy response: %v", err)
	}
	if echo["path"] != "/secret-provider-key/api/v2/comment/12345" || echo["query"] != "withRelated=true&chConvert=1" {
		t.Fatalf("unexpected proxied GET path/query: %+v", echo)
	}

	dmPostPath := "/dm/" + strconv.FormatInt(src.ID, 10) + "/" + storedA.danmakuToken + "/api/v2/match"
	dmPostReq := httptest.NewRequest(http.MethodPost, dmPostPath, strings.NewReader(`{"fileName":"Frieren.E01.mkv"}`))
	dmPostReq.Header.Set("Content-Type", "application/json")
	dmPostRec := httptest.NewRecorder()
	app.handleDanmakuProxy(dmPostRec, dmPostReq)
	if dmPostRec.Code != http.StatusOK {
		t.Fatalf("active user POST danmaku status=%d body=%s", dmPostRec.Code, dmPostRec.Body.String())
	}
	var postEcho map[string]any
	_ = json.Unmarshal(dmPostRec.Body.Bytes(), &postEcho)
	if postEcho["method"] != "POST" || postEcho["body"] != `{"fileName":"Frieren.E01.mkv"}` {
		t.Fatalf("unexpected proxied POST echo: %+v", postEcho)
	}

	// 6. Expired, disabled, and quota-exceeded users MUST receive 403 and NEVER hit upstream
	hitsBefore := upstreamHits.Load()
	for _, uid := range []int64{userExpired.ID, userDisabled.ID, userQuota.ID} {
		su, _ := app.db.getShareUser(uid)
		req := httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+su.danmakuToken+"/api/v2/comment/1", nil)
		rec := httptest.NewRecorder()
		app.handleDanmakuProxy(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for inactive user %s, got %d body=%s", su.Username, rec.Code, rec.Body.String())
		}
	}
	if upstreamHits.Load() != hitsBefore {
		t.Fatalf("inactive user requests leaked to upstream: before=%d after=%d", hitsBefore, upstreamHits.Load())
	}

	// 7. Reset user A's danmaku token -> old token fails (403), new token works (200), user B unaffected (200)
	oldTokenA := storedA.danmakuToken
	storedB, _ := app.db.getShareUser(userB.ID)

	resetReq := httptest.NewRequest(http.MethodPost, "/api/share-users/"+strconv.FormatInt(userA.ID, 10)+"/reset-danmaku-token", nil)
	resetRec := httptest.NewRecorder()
	app.handleShareUserByID(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset-danmaku-token status=%d body=%s", resetRec.Code, resetRec.Body.String())
	}

	// Old token A -> 403
	oldRec := httptest.NewRecorder()
	app.handleDanmakuProxy(oldRec, httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+oldTokenA+"/api/v2/comment/1", nil))
	if oldRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for rotated old token, got %d", oldRec.Code)
	}

	// New token A -> 200
	refreshedA, _ := app.db.getShareUser(userA.ID)
	if refreshedA.danmakuToken == "" || refreshedA.danmakuToken == oldTokenA {
		t.Fatalf("expected new danmaku token for user A")
	}
	newRec := httptest.NewRecorder()
	app.handleDanmakuProxy(newRec, httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+refreshedA.danmakuToken+"/api/v2/comment/1", nil))
	if newRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for new token, got %d body=%s", newRec.Code, newRec.Body.String())
	}

	// User B -> still 200
	bRec := httptest.NewRecorder()
	app.handleDanmakuProxy(bRec, httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+storedB.danmakuToken+"/api/v2/comment/1", nil))
	if bRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unaffected user B, got %d", bRec.Code)
	}
}

func TestDanmakuEdgeEgressAndFailClosed(t *testing.T) {
	app, site := setupDanmakuTestApp(t)

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"jpco-danmaku","path":"` + r.URL.Path + `"}`))
	}))
	defer upstream.Close()

	// Create an Edge Node and bring up a real edgeAgent locally
	masterMux := http.NewServeMux()
	masterMux.HandleFunc("/api/agent/heartbeat", app.handleAgentHeartbeat)
	masterSrv := httptest.NewServer(masterMux)
	defer masterSrv.Close()

	createNodeRec := httptest.NewRecorder()
	app.handleEdgeNodes(createNodeRec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"RFC-JPCO","public_port":19090}`)))
	if createNodeRec.Code != http.StatusCreated {
		t.Fatalf("create edge node status=%d body=%s", createNodeRec.Code, createNodeRec.Body.String())
	}
	var node EdgeNode
	_ = json.Unmarshal(createNodeRec.Body.Bytes(), &node)

	app.pm.edges.mu.RLock()
	nodeToken := app.pm.edges.nodes[node.ID].tokenPlain
	app.pm.edges.mu.RUnlock()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: masterSrv.URL,
		NodeID:    node.ID,
		Token:     nodeToken,
		BindAddr:  "127.0.0.1",
	})
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen agent: %v", err)
	}
	agentPort := rawLn.Addr().(*net.TCPAddr).Port
	agent.cfg.Port = agentPort
	agentSrv := &http.Server{Handler: agent.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = agentSrv.Serve(rawLn) }()
	defer agentSrv.Close()

	updNodeRec := httptest.NewRecorder()
	app.handleEdgeNodeByID(updNodeRec, httptest.NewRequest(http.MethodPut, "/api/edge-nodes/"+strconv.FormatInt(node.ID, 10), strings.NewReader(`{"name":"RFC-JPCO","public_port":`+strconv.Itoa(agentPort)+`,"public_ip":"127.0.0.1","tls_mode":"http","enabled":true}`)))
	if updNodeRec.Code != http.StatusOK {
		t.Fatalf("update edge node status=%d body=%s", updNodeRec.Code, updNodeRec.Body.String())
	}

	// Mark node online via heartbeat
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("agent heartbeat: %v", err)
	}

	// Create danmaku source pinned to RFC-JPCO edge node
	srcBody, _ := json.Marshal(map[string]any{
		"name":           "JPCO 专属弹幕",
		"upstream_url":   upstream.URL,
		"egress_node_id": node.ID,
		"enabled":        true,
	})
	srcRec := httptest.NewRecorder()
	app.handleDanmakuSources(srcRec, httptest.NewRequest(http.MethodPost, "/api/danmaku-sources", bytes.NewReader(srcBody)))
	if srcRec.Code != http.StatusCreated {
		t.Fatalf("create edge danmaku source status=%d body=%s", srcRec.Code, srcRec.Body.String())
	}
	var src DanmakuSource
	_ = json.Unmarshal(srcRec.Body.Bytes(), &src)
	if src.EgressName != "RFC-JPCO" || !src.EgressOnline {
		t.Fatalf("expected RFC-JPCO online, got %+v", src)
	}

	// Active user assigned to JPCO danmaku proxies through RFC-JPCO edge agent
	user := createDanmakuTestShareUser(t, app, site.ID, "jpco_user", "", true, 0, []int64{src.ID})
	stored, _ := app.db.getShareUser(user.ID)

	proxyRec := httptest.NewRecorder()
	app.handleDanmakuProxy(proxyRec, httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+stored.danmakuToken+"/api/v2/bangumi/42", nil))
	if proxyRec.Code != http.StatusOK || !strings.Contains(proxyRec.Body.String(), `"path":"/api/v2/bangumi/42"`) {
		t.Fatalf("expected 200 via edge egress, got %d body=%s", proxyRec.Code, proxyRec.Body.String())
	}

	// Live connectivity test via RFC-JPCO edge agent
	testRec := httptest.NewRecorder()
	app.handleDanmakuSourceByID(testRec, httptest.NewRequest(http.MethodPost, "/api/danmaku-sources/"+strconv.FormatInt(src.ID, 10)+"/test", nil))
	var testRes DanmakuTestResult
	_ = json.Unmarshal(testRec.Body.Bytes(), &testRes)
	if !testRes.OK || testRes.EgressName != "RFC-JPCO" {
		t.Fatalf("expected edge test OK via RFC-JPCO, got %+v", testRes)
	}

	// Disable RFC-JPCO -> MUST FAIL CLOSED (503) and NEVER leak Master IP to upstream!
	disableRec := httptest.NewRecorder()
	app.handleEdgeNodeByID(disableRec, httptest.NewRequest(http.MethodPut, "/api/edge-nodes/"+strconv.FormatInt(node.ID, 10), strings.NewReader(`{"name":"RFC-JPCO","public_port":`+strconv.Itoa(agentPort)+`,"public_ip":"127.0.0.1","tls_mode":"http","enabled":false}`)))
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable edge node status=%d body=%s", disableRec.Code, disableRec.Body.String())
	}

	hitsBefore := upstreamHits.Load()
	failRec := httptest.NewRecorder()
	app.handleDanmakuProxy(failRec, httptest.NewRequest(http.MethodGet, "/dm/"+strconv.FormatInt(src.ID, 10)+"/"+stored.danmakuToken+"/api/v2/bangumi/42", nil))
	if failRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed when pinned edge disabled, got %d body=%s", failRec.Code, failRec.Body.String())
	}
	failTestRec := httptest.NewRecorder()
	app.handleDanmakuSourceByID(failTestRec, httptest.NewRequest(http.MethodPost, "/api/danmaku-sources/"+strconv.FormatInt(src.ID, 10)+"/test", nil))
	var failTestRes DanmakuTestResult
	_ = json.Unmarshal(failTestRec.Body.Bytes(), &failTestRes)
	if failTestRes.OK {
		t.Fatalf("expected test to fail closed when pinned edge disabled, got %+v", failTestRes)
	}
	if upstreamHits.Load() != hitsBefore {
		t.Fatalf("fail-closed violated: master contacted upstream directly (before=%d after=%d)", hitsBefore, upstreamHits.Load())
	}
}
