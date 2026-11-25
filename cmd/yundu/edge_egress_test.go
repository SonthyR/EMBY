package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEdgeAgentEgressAuthAndSecurity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Seen-Host", r.Host)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok:" + r.URL.Path))
	}))
	defer upstream.Close()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: "http://127.0.0.1:19999",
		NodeID:    7,
		Token:     "edge-secret-token-7",
		Port:      19090,
	})
	agentSrv := httptest.NewServer(agent.handler())
	defer agentSrv.Close()

	// 1. Missing ticket -> 401
	resp, err := http.Get(agentSrv.URL + edgeEgressPath)
	if err != nil {
		t.Fatalf("missing ticket request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing ticket status = %d, want 401", resp.StatusCode)
	}

	// 2. Wrong token / tampered ticket -> 401
	badTokenTicket, err := sealEdgeEgressTicket("wrong-token", edgeEgressTicket{
		NodeID:    7,
		TargetURL: upstream.URL + "/System/Info/Public",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("seal wrong token ticket: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, agentSrv.URL+edgeEgressPath, nil)
	req.Header.Set(edgeEgressTicketHeader, badTokenTicket)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wrong token request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", resp.StatusCode)
	}

	// 3. Wrong node ID -> 401
	wrongNodeTicket, err := sealEdgeEgressTicket("edge-secret-token-7", edgeEgressTicket{
		NodeID:    99,
		TargetURL: upstream.URL + "/System/Info/Public",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("seal wrong node ticket: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, agentSrv.URL+edgeEgressPath, nil)
	req.Header.Set(edgeEgressTicketHeader, wrongNodeTicket)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wrong node request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong node status = %d, want 401", resp.StatusCode)
	}

	// 4. Expired ticket -> 401
	expiredTicket, err := sealEdgeEgressTicket("edge-secret-token-7", edgeEgressTicket{
		NodeID:    7,
		TargetURL: upstream.URL + "/System/Info/Public",
		ExpiresAt: time.Now().Add(-10 * time.Second).Unix(),
	})
	if err != nil {
		t.Fatalf("seal expired ticket: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, agentSrv.URL+edgeEgressPath, nil)
	req.Header.Set(edgeEgressTicketHeader, expiredTicket)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("expired request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired status = %d, want 401", resp.StatusCode)
	}

	// 5. Playback ticket cannot be replayed on egress endpoint (domain separation)
	playbackTicket, err := sealEdgePlaybackTicket("edge-secret-token-7", edgePlaybackTicket{
		NodeID:    7,
		SiteID:    1,
		TargetURL: upstream.URL + "/System/Info/Public",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("seal playback ticket: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, agentSrv.URL+edgeEgressPath, nil)
	req.Header.Set(edgeEgressTicketHeader, playbackTicket)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("playback ticket replay: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("playback ticket replay status = %d, want 401", resp.StatusCode)
	}

	// 6. Valid ticket -> 200 with marker header and Host preserved
	validTicket, err := sealEdgeEgressTicket("edge-secret-token-7", edgeEgressTicket{
		NodeID:    7,
		TargetURL: upstream.URL + "/System/Info/Public",
		Host:      "strict.emby.example",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("seal valid ticket: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, agentSrv.URL+edgeEgressPath, nil)
	req.Header.Set(edgeEgressTicketHeader, validTicket)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("valid request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok:/System/Info/Public" {
		t.Fatalf("valid egress got status=%d body=%q", resp.StatusCode, string(body))
	}
	if resp.Header.Get(edgeEgressMarkerHeader) != "1" {
		t.Fatalf("missing %s marker header", edgeEgressMarkerHeader)
	}
	if got := resp.Header.Get("X-Upstream-Seen-Host"); got != "strict.emby.example" {
		t.Fatalf("upstream Host = %q, want strict.emby.example", got)
	}
}

func TestEdgeFullSingleIPFullChainLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ipDir := filepath.Join(t.TempDir(), "ip2region")
	if err := os.MkdirAll(ipDir, 0o700); err != nil {
		t.Fatalf("mkdir ip2region: %v", err)
	}
	v4Raw := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|广东省|广州市|电信|CN"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write v4 xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	// Track every upstream hit by path and which edge marker header was injected
	// by the edge agent's custom upstream transport. Any request without X-Test-Edge
	// came directly from master (which MUST be 0 in edge_full mode!).
	var mu sync.Mutex
	var upstreamHits []struct {
		method string
		path   string
		edge   string
	}
	var logoutCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edgeTag := r.Header.Get("X-Test-Edge")
		mu.Lock()
		upstreamHits = append(upstreamHits, struct {
			method string
			path   string
			edge   string
		}{r.Method, r.URL.Path, edgeTag})
		mu.Unlock()

		if isWebSocketUpgrade(r) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: fake-accept\r\nX-Test-WS-Edge: "+edgeTag+"\r\n\r\n")
			line, _ := rw.ReadString('\n')
			_, _ = io.WriteString(conn, "echo:"+edgeTag+":"+line)
			return
		}

		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"tok-strict-1","User":{"Id":"u1"}}`))
		case "/Users/u1/Items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Id":"m1"}]}`))
		case "/Items/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`not found on upstream`))
		case "/Videos/101/stream.mkv":
			w.Header().Set("Content-Type", "video/x-matroska")
			_, _ = w.Write([]byte("video-via-" + edgeTag))
		case "/Sessions/Logout":
			logoutCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()

	// Helper to spin up a real edgeAgent whose outbound upstream client stamps X-Test-Edge.
	startTestEdgeAgent := func(nodeID int64, token, tag string) (*edgeAgent, *httptest.Server) {
		agent := newEdgeAgent(edgeAgentConfig{
			MasterURL: "http://127.0.0.1:19999",
			NodeID:    nodeID,
			Token:     token,
			Port:      19090,
		})
		stamp := func(base http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				cloned := req.Clone(req.Context())
				cloned.Header.Set("X-Test-Edge", tag)
				return base.RoundTrip(cloned)
			})
		}
		agent.upstreamClient.Transport = stamp(agent.upstreamClient.Transport)
		agent.egressClient.Transport = stamp(agent.egressClient.Transport)
		baseHandler := agent.handler()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-Test-Edge", tag)
			baseHandler.ServeHTTP(w, r)
		}))
		return agent, srv
	}

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x42}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)
	app := &App{db: db, dbPath: dbPath, pm: pm, ip2region: ipMgr, trustedProxies: []*net.IPNet{loopbackCIDR}}

	// Create Edge 1 and Edge 2 in DB, then start their live httptest edge agents.
	createEdge := func(name string) (int64, string) {
		rec := httptest.NewRecorder()
		app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"`+name+`","public_port":19090,"public_ip":"127.0.0.1","tls_mode":"http"}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("createEdge %s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		var n EdgeNode
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatalf("decode edge node: %v", err)
		}
		edgeRT.mu.RLock()
		tok := edgeRT.nodes[n.ID].tokenPlain
		edgeRT.mu.RUnlock()
		return n.ID, tok
	}

	edge1ID, edge1Token := createEdge("Edge-1")
	edge2ID, edge2Token := createEdge("Edge-2")
	_, edge1Srv := startTestEdgeAgent(edge1ID, edge1Token, "edge-1")
	defer edge1Srv.Close()
	_, edge2Srv := startTestEdgeAgent(edge2ID, edge2Token, "edge-2")
	defer edge2Srv.Close()

	bindAgentServer := func(id int64, rawURL string) {
		u, _ := url.Parse(rawURL)
		port, _ := strconv.Atoi(u.Port())
		if _, err := db.db.Exec(`UPDATE edge_nodes SET public_ip=?, public_port=?, public_host='', tls_mode=? WHERE id=?`, u.Hostname(), port, edgeNodeTLSModeHTTP, id); err != nil {
			t.Fatalf("update edge node port: %v", err)
		}
		if err := edgeRT.reload(); err != nil {
			t.Fatalf("reload edgeRT: %v", err)
		}
		edgeRT.mu.Lock()
		live := edgeRT.nodes[id]
		live.lastSeen = time.Now()
		live.agentVersion = appVersion
		edgeRT.mu.Unlock()
	}
	bindAgentServer(edge1ID, edge1Srv.URL)
	bindAgentServer(edge2ID, edge2Srv.URL)

	// Configure CT ISP route: Edge-1 first, then Edge-2, then Master.
	setCTOrder := func(first, second int64) {
		rec := httptest.NewRecorder()
		body := `{"group":"ct","nodes":[{"node_id":` + jsonInt(int(first)) + `,"enabled":true},{"node_id":` + jsonInt(int(second)) + `,"enabled":true},{"node_id":0,"enabled":true}]}`
		app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("setCTOrder status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	setCTOrder(edge1ID, edge2ID)

	// Create edge_full site (auto ISP selection: EdgePinnedNodeID=0) with an upstream account.
	site, err := db.CreateSiteRecord(Site{
		Name:            "StrictEmby",
		TargetURL:       upstream.URL,
		ListenPort:      18201,
		IngressMode:     ingressModePath,
		PathPrefix:      "/strict",
		Enabled:         true,
		UAMode:          "passthrough",
		EdgeRoutingMode: edgeRoutingModeEdgeFull,
	}, SiteAccountInput{
		AccountName: "upstream-vip",
		Username:    "real-user",
		Password:    "real-pass",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if err := pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	defer pm.GracefulShutdown(context.Background())

	// Create share user assigned to this site.
	shareUserRec := httptest.NewRecorder()
	shareReqBody := `{"username":"alice","password":"alice-pass-123","enabled":true,"sites":[{"site_id":` + jsonInt(int(site.ID)) + `,"site_account_id":0}]}`
	app.handleShareUsers(shareUserRec, httptest.NewRequest(http.MethodPost, "/api/share-users", strings.NewReader(shareReqBody)))
	if shareUserRec.Code != http.StatusCreated {
		t.Fatalf("create share user status=%d body=%s", shareUserRec.Code, shareUserRec.Body.String())
	}
	var createdUser ShareUser
	if err := json.Unmarshal(shareUserRec.Body.Bytes(), &createdUser); err != nil {
		t.Fatalf("decode share user: %v", err)
	}

	siteHandler, ok := pm.SiteHandler(site.ID)
	if !ok {
		t.Fatalf("site handler missing")
	}
	masterSrv := httptest.NewServer(siteHandler)
	defer masterSrv.Close()

	noRedirectClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Step 1: Login via master -> MUST exit via Edge-1 and persist egress_node_id=edge1ID!
	loginReq, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"alice","Pw":"alice-pass-123"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("X-Real-IP", "1.2.3.4")
	loginReq.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Infuse", Device="AppleTV", DeviceId="dev-1", Version="7.7"`)
	loginResp, err := noRedirectClient.Do(loginReq)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	_ = loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK || !strings.Contains(string(loginBody), "tok-strict-1") {
		t.Fatalf("login got status=%d body=%s", loginResp.StatusCode, string(loginBody))
	}

	if gotNode, ok := pm.shares.sessionEgressNodeID(shareTokenHash("tok-strict-1")); !ok || gotNode != edge1ID {
		t.Fatalf("session egress_node_id = (%d, %v), want (%d, true)", gotNode, ok, edge1ID)
	}

	// Step 2: Simulate active playback on Edge-1 and change CT ISP priority to Edge-2 first!
	// Because Alice's session is actively watching on Edge-1, subsequent API + genuine 404 +
	// video 307 + WebSocket MUST stay pinned on Edge-1 without interrupting playback!
	pm.shares.markEdgePlayback(shareTokenHash("tok-strict-1"), "Edge-1")
	setCTOrder(edge2ID, edge1ID)

	// 2a. Metadata API -> Edge-1
	itemsReq, _ := http.NewRequest(http.MethodGet, masterSrv.URL+"/Users/u1/Items", nil)
	itemsReq.Header.Set("X-Real-IP", "1.2.3.4")
	itemsReq.Header.Set("X-Emby-Token", "tok-strict-1")
	itemsResp, err := noRedirectClient.Do(itemsReq)
	if err != nil {
		t.Fatalf("items request: %v", err)
	}
	_ = itemsResp.Body.Close()
	if itemsResp.StatusCode != http.StatusOK {
		t.Fatalf("items status=%d, want 200", itemsResp.StatusCode)
	}

	// 2b. Genuine upstream 404 MUST remain 404 (not confused with missing /_yundu/egress)
	missReq, _ := http.NewRequest(http.MethodGet, masterSrv.URL+"/Items/missing", nil)
	missReq.Header.Set("X-Real-IP", "1.2.3.4")
	missReq.Header.Set("X-Emby-Token", "tok-strict-1")
	missResp, err := noRedirectClient.Do(missReq)
	if err != nil {
		t.Fatalf("missing item request: %v", err)
	}
	_ = missResp.Body.Close()
	if missResp.StatusCode != http.StatusNotFound {
		t.Fatalf("genuine upstream 404 got status=%d, want 404", missResp.StatusCode)
	}

	// 2c. Video stream MUST 307-redirect to Edge-1 (NOT Edge-2!) and Edge-1 streams from upstream
	vidReq, _ := http.NewRequest(http.MethodGet, masterSrv.URL+"/Videos/101/stream.mkv?Static=true", nil)
	vidReq.Header.Set("X-Real-IP", "1.2.3.4")
	vidReq.Header.Set("X-Emby-Token", "tok-strict-1")
	vidResp, err := noRedirectClient.Do(vidReq)
	if err != nil {
		t.Fatalf("video request: %v", err)
	}
	_ = vidResp.Body.Close()
	if vidResp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("video status=%d, want 307", vidResp.StatusCode)
	}
	loc := vidResp.Header.Get("Location")
	if !strings.HasPrefix(loc, edge1Srv.URL+edgeTicketPathPrefix) {
		t.Fatalf("video redirect Location = %q, want prefix %q (must stay pinned to Edge-1)", loc, edge1Srv.URL+edgeTicketPathPrefix)
	}
	streamResp, err := http.Get(loc)
	if err != nil {
		t.Fatalf("follow edge stream: %v", err)
	}
	streamBody, _ := io.ReadAll(streamResp.Body)
	_ = streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK || string(streamBody) != "video-via-edge-1" {
		t.Fatalf("edge stream got status=%d body=%q", streamResp.StatusCode, string(streamBody))
	}

	// 2d. WebSocket upgrade MUST also tunnel through Edge-1
	masterURL, _ := url.Parse(masterSrv.URL)
	wsConn, err := net.DialTimeout("tcp", masterURL.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial master ws: %v", err)
	}
	_, _ = io.WriteString(wsConn, "GET /embywebsocket?api_key=tok-strict-1 HTTP/1.1\r\nHost: "+masterURL.Host+"\r\nX-Real-IP: 1.2.3.4\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	wsReader := bufio.NewReader(wsConn)
	wsResp, err := http.ReadResponse(wsReader, nil)
	if err != nil {
		_ = wsConn.Close()
		t.Fatalf("read ws handshake: %v", err)
	}
	if wsResp.StatusCode != http.StatusSwitchingProtocols {
		_ = wsConn.Close()
		t.Fatalf("ws status=%d, want 101", wsResp.StatusCode)
	}
	_, _ = io.WriteString(wsConn, "ping\n")
	echoLine, err := wsReader.ReadString('\n')
	_ = wsConn.Close()
	if err != nil || echoLine != "echo:edge-1:ping\n" {
		t.Fatalf("ws tunnel echo = %q (err=%v), want echo:edge-1:ping", echoLine, err)
	}

	// Step 3: Verify ZERO direct hits from master so far, all hits came from edge-1!
	mu.Lock()
	for _, hit := range upstreamHits {
		if hit.edge != "edge-1" {
			mu.Unlock()
			t.Fatalf("upstream hit %s %s had edge=%q, want edge-1 (master IP leaked!)", hit.method, hit.path, hit.edge)
		}
	}
	hitsBeforeOffline := len(upstreamHits)
	mu.Unlock()
	if hitsBeforeOffline < 5 {
		t.Fatalf("expected >=5 upstream hits via edge-1, got %d", hitsBeforeOffline)
	}

	// Step 4: Fail-closed on a pinned site when Edge-1 goes offline -> MUST return 503 with 0 upstream hits!
	site.EdgePinnedNodeID = edge1ID
	if err := db.UpdateSiteRecord(*site); err != nil {
		t.Fatalf("UpdateSiteRecord pinned: %v", err)
	}
	if err := pm.StartSite(*site); err != nil {
		t.Fatalf("restart pinned site: %v", err)
	}
	siteHandler, _ = pm.SiteHandler(site.ID)
	masterSrv.Config.Handler = siteHandler

	edgeRT.mu.Lock()
	edgeRT.nodes[edge1ID].lastSeen = time.Now().Add(-5 * time.Minute)
	edgeRT.mu.Unlock()

	offlineReq, _ := http.NewRequest(http.MethodGet, masterSrv.URL+"/Users/u1/Items", nil)
	offlineReq.Header.Set("X-Real-IP", "1.2.3.4")
	offlineReq.Header.Set("X-Emby-Token", "tok-strict-1")
	offlineResp, err := noRedirectClient.Do(offlineReq)
	if err != nil {
		t.Fatalf("offline request: %v", err)
	}
	_ = offlineResp.Body.Close()
	if offlineResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline bound edge node status=%d, want 503 (fail-closed)", offlineResp.StatusCode)
	}
	mu.Lock()
	hitsAfterOffline := len(upstreamHits)
	mu.Unlock()
	if hitsAfterOffline != hitsBeforeOffline {
		t.Fatalf("master leaked direct request while locked edge node was offline! before=%d after=%d", hitsBeforeOffline, hitsAfterOffline)
	}

	// Step 5: Bring Edge-1 back online, kick Alice's session -> upstream /Sessions/Logout MUST go via Edge-1!
	edgeRT.mu.Lock()
	edgeRT.nodes[edge1ID].lastSeen = time.Now()
	edgeRT.mu.Unlock()

	if _, err := pm.shares.revokeSessions(createdUser.ID, 0, nil); err != nil {
		t.Fatalf("revokeSessions: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && logoutCount.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if logoutCount.Load() != 1 {
		t.Fatalf("expected 1 upstream logout call, got %d", logoutCount.Load())
	}
	mu.Lock()
	lastHit := upstreamHits[len(upstreamHits)-1]
	mu.Unlock()
	if lastHit.path != "/Sessions/Logout" || lastHit.edge != "edge-1" {
		t.Fatalf("logout hit = %+v, want /Sessions/Logout via edge-1", lastHit)
	}
}

func TestEdgeFullOutdatedAgentReturnsClearUpgradeError(t *testing.T) {
	// Simulate an older v1.4.7 edge node that does NOT implement /_yundu/egress
	// and responds with plain 404 (without X-Yundu-Egress: 1).
	oldAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer oldAgent.Close()

	rt := &edgeEgressTransport{base: http.DefaultTransport}
	req, _ := http.NewRequest(http.MethodGet, "http://upstream.example/System/Info/Public", nil)
	req = req.WithContext(withEdgeEgressTarget(req.Context(), edgeEgressTarget{
		NodeID:    1,
		NodeName:  "Old-Edge",
		PublicURL: oldAgent.URL,
		Token:     "tok-1",
	}))
	resp, err := rt.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != errEdgeAgentEgressUnsupported {
		t.Fatalf("outdated agent error = %v, want errEdgeAgentEgressUnsupported", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
