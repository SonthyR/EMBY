package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestRewriteEmbyRequestToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/emby/Users/u1/Items?api_key=oldTok&X-Emby-Token=oldTok&Other=keep", nil)
	req.Header.Set("X-Emby-Token", "oldTok")
	req.Header.Set("X-MediaBrowser-Token", "oldTok")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Infuse", Device="AppleTV", DeviceId="d1", Version="7.7", Token="oldTok"`)
	req.Header.Set("Authorization", `Emby Client="Web", Token=oldTok, Device="Chrome"`)

	rewriteEmbyRequestToken(req, "oldTok", "newTok")

	if got := req.Header.Get("X-Emby-Token"); got != "newTok" {
		t.Fatalf("X-Emby-Token = %q, want newTok", got)
	}
	if got := req.Header.Get("X-MediaBrowser-Token"); got != "newTok" {
		t.Fatalf("X-MediaBrowser-Token = %q, want newTok", got)
	}
	if got := embyAuthorizationAttribute(req.Header.Get("X-Emby-Authorization"), "Token"); got != "newTok" {
		t.Fatalf("X-Emby-Authorization Token = %q, want newTok (full=%q)", got, req.Header.Get("X-Emby-Authorization"))
	}
	if got := embyAuthorizationAttribute(req.Header.Get("X-Emby-Authorization"), "DeviceId"); got != "d1" {
		t.Fatalf("X-Emby-Authorization DeviceId corrupted: %q", got)
	}
	if got := req.Header.Get("Authorization"); !strings.Contains(got, "Token=newTok") {
		t.Fatalf("Authorization = %q, want Token=newTok", got)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/emby/Users/u1/Items", nil)
	req2.Header.Set("Authorization", `Emby Client="Web", Token="oldTok", Device="Chrome"`)
	rewriteEmbyRequestToken(req2, "oldTok", "newTok")
	if got := embyAuthorizationAttribute(req2.Header.Get("Authorization"), "Token"); got != "newTok" {
		t.Fatalf("quoted Authorization Token = %q, want newTok", got)
	}
	q := req.URL.Query()
	if q.Get("api_key") != "newTok" || q.Get("X-Emby-Token") != "newTok" || q.Get("Other") != "keep" {
		t.Fatalf("rewritten query = %q", req.URL.RawQuery)
	}
}

func TestEdgeFullSeamlessHandoffAllDirectionsAndGuards(t *testing.T) {
	origCooldown := shareHandoffCooldown
	shareHandoffCooldown = 20 * time.Millisecond
	defer func() { shareHandoffCooldown = origCooldown }()

	dbPath := filepath.Join(t.TempDir(), "handoff.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Synthetic IP2Region DB:
	// 1.1.1.0/24 -> Unicom (cu) -> Master
	// 2.2.2.0/24 -> Mobile (cm) -> Edge-CM
	// 3.3.3.0/24 -> Telecom (ct) -> Edge-CT
	ipDir := filepath.Join(t.TempDir(), "ip2region")
	if err := os.MkdirAll(ipDir, 0o700); err != nil {
		t.Fatalf("mkdir ip2region: %v", err)
	}
	v4Raw := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("1.1.1.0"), end: netip.MustParseAddr("1.1.1.255"), region: "中国|北京市|北京市|联通|CN"},
		{start: netip.MustParseAddr("2.2.2.0"), end: netip.MustParseAddr("2.2.2.255"), region: "中国|上海市|上海市|移动|CN"},
		{start: netip.MustParseAddr("3.3.3.0"), end: netip.MustParseAddr("3.3.3.255"), region: "中国|广东省|广州市|电信|CN"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write v4 xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	type upstreamEvent struct {
		method string
		path   string
		edge   string // "" = master, "edge-cm", "edge-ct"
		token  string
	}
	var mu sync.Mutex
	var events []upstreamEvent
	var tokenSeq atomic.Int32
	activeUpstreamTokens := make(map[string]string) // token -> bound edge tag (verifies zero cross-IP token reuse!)
	var crossIPViolation string
	var forceUID atomic.Value
	forceUID.Store("uid-1")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edgeTag := r.Header.Get("X-Test-Edge")
		if edgeTag == "" {
			edgeTag = "master"
		}
		tok := r.Header.Get("X-Emby-Token")
		if tok == "" {
			tok = r.URL.Query().Get("api_key")
		}

		if isWebSocketUpgrade(r) {
			mu.Lock()
			events = append(events, upstreamEvent{r.Method, r.URL.Path, edgeTag, tok})
			if bound, ok := activeUpstreamTokens[tok]; ok && bound != edgeTag {
				crossIPViolation = fmt.Sprintf("WS token %s bound to %s used from %s", tok, bound, edgeTag)
			}
			mu.Unlock()
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: fake\r\n\r\n")
			line, _ := rw.ReadString('\n')
			_, _ = io.WriteString(conn, "ws:"+edgeTag+":"+tok+":"+line)
			return
		}

		mu.Lock()
		events = append(events, upstreamEvent{r.Method, r.URL.Path, edgeTag, tok})
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			seq := tokenSeq.Add(1)
			newTok := fmt.Sprintf("tok-%d", seq)
			activeUpstreamTokens[newTok] = edgeTag
			uid, _ := forceUID.Load().(string)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"AccessToken":%q,"User":{"Id":%q}}`, newTok, uid)
			return
		case "/Sessions/Logout":
			if bound, ok := activeUpstreamTokens[tok]; ok && bound != edgeTag {
				crossIPViolation = fmt.Sprintf("Logout token %s bound to %s called from %s", tok, bound, edgeTag)
			}
			delete(activeUpstreamTokens, tok)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			if tok != "" {
				if bound, ok := activeUpstreamTokens[tok]; ok && bound != edgeTag {
					crossIPViolation = fmt.Sprintf("Request %s token %s bound to %s used from %s", r.URL.Path, tok, bound, edgeTag)
				}
			}
			mu.Unlock()
		}

		switch r.URL.Path {
		case "/Users/uid-1/Items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"edge":%q,"token":%q}`, edgeTag, tok)
		case "/Videos/101/stream.mkv":
			w.Header().Set("Content-Type", "video/x-matroska")
			_, _ = fmt.Fprintf(w, "stream:%s:%s", edgeTag, tok)
		case "/Sessions/Playing", "/Sessions/Playing/Stopped":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()

	startTestEdgeAgent := func(nodeID int64, token, tag string) *httptest.Server {
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
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-Test-Edge", tag)
			baseHandler.ServeHTTP(w, r)
		}))
	}

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x55}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)
	app := &App{db: db, dbPath: dbPath, pm: pm, ip2region: ipMgr, trustedProxies: []*net.IPNet{loopbackCIDR}}

	createEdge := func(name, tag, version string) (int64, *httptest.Server) {
		rec := httptest.NewRecorder()
		app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"`+name+`","public_port":19090,"public_ip":"127.0.0.1","tls_mode":"http"}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("createEdge %s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		var n EdgeNode
		_ = json.Unmarshal(rec.Body.Bytes(), &n)
		edgeRT.mu.RLock()
		tok := edgeRT.nodes[n.ID].tokenPlain
		edgeRT.mu.RUnlock()
		srv := startTestEdgeAgent(n.ID, tok, tag)
		u, _ := url.Parse(srv.URL)
		port, _ := strconv.Atoi(u.Port())
		_, _ = db.db.Exec(`UPDATE edge_nodes SET public_ip=?, public_port=?, public_host='', tls_mode=? WHERE id=?`, u.Hostname(), port, edgeNodeTLSModeHTTP, n.ID)
		_ = edgeRT.reload()
		edgeRT.mu.Lock()
		edgeRT.nodes[n.ID].lastSeen = time.Now()
		edgeRT.nodes[n.ID].agentVersion = version
		edgeRT.nodes[n.ID].supportsEgress = agentVersionSupportsEgress(version)
		edgeRT.mu.Unlock()
		return n.ID, srv
	}

	edgeCMID, edgeCMSrv := createEdge("移动从机", "edge-cm", appVersion)
	defer edgeCMSrv.Close()
	edgeCTID, edgeCTSrv := createEdge("电信从机", "edge-ct", appVersion)
	defer edgeCTSrv.Close()

	setRoute := func(group string, strategy string, nodeIDs ...int64) {
		var nodesJSON []string
		for _, id := range nodeIDs {
			nodesJSON = append(nodesJSON, fmt.Sprintf(`{"node_id":%d,"enabled":true}`, id))
		}
		body := fmt.Sprintf(`{"group":%q,"strategy":%q,"nodes":[%s]}`, group, strategy, strings.Join(nodesJSON, ","))
		rec := httptest.NewRecorder()
		app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("setRoute %s status=%d body=%s", group, rec.Code, rec.Body.String())
		}
	}
	// CU -> Master first; CM -> Edge-CM first; CT -> Edge-CT first.
	setRoute("cu", ispStrategyPriority, 0, edgeCMID, edgeCTID)
	setRoute("cm", ispStrategyPriority, edgeCMID, 0, edgeCTID)
	setRoute("ct", ispStrategyPriority, edgeCTID, 0, edgeCMID)

	site, err := db.CreateSiteRecord(Site{
		Name:            "StrictHandoff",
		TargetURL:       upstream.URL,
		ListenPort:      18301,
		IngressMode:     ingressModePath,
		PathPrefix:      "/handoff",
		Enabled:         true,
		UAMode:          "passthrough",
		EdgeRoutingMode: edgeRoutingModeEdgeFull,
	}, SiteAccountInput{
		AccountName: "vip1",
		Username:    "upstream-user",
		Password:    "upstream-pass",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if err := pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	defer pm.GracefulShutdown(context.Background())

	shareRec := httptest.NewRecorder()
	app.handleShareUsers(shareRec, httptest.NewRequest(http.MethodPost, "/api/share-users",
		strings.NewReader(fmt.Sprintf(`{"username":"bob","password":"bob-pass-123","enabled":true,"sites":[{"site_id":%d,"site_account_id":0}]}`, site.ID))))
	if shareRec.Code != http.StatusCreated {
		t.Fatalf("create share user status=%d body=%s", shareRec.Code, shareRec.Body.String())
	}
	var bob ShareUser
	_ = json.Unmarshal(shareRec.Body.Bytes(), &bob)

	siteHandler, _ := pm.SiteHandler(site.ID)
	masterSrv := httptest.NewServer(siteHandler)
	defer masterSrv.Close()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	callItems := func(clientIP, clientToken string) (int, string, string) {
		req, _ := http.NewRequest(http.MethodGet, masterSrv.URL+"/Users/uid-1/Items", nil)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("X-Emby-Token", clientToken)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("callItems(%s): %v", clientIP, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var out struct {
			Edge  string `json:"edge"`
			Token string `json:"token"`
		}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out.Edge, out.Token
	}

	// 1. Initial login on Unicom (1.1.1.1) -> Master, client receives tok-1.
	loginReq, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"bob","Pw":"bob-pass-123"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("X-Real-IP", "1.1.1.1")
	loginReq.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Infuse", Device="iPhone", DeviceId="iphone-1", Version="7.7"`)
	loginResp, err := noRedirect.Do(loginReq)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var loginPayload struct {
		AccessToken string `json:"AccessToken"`
	}
	_ = json.NewDecoder(loginResp.Body).Decode(&loginPayload)
	_ = loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK || loginPayload.AccessToken != "tok-1" {
		t.Fatalf("initial login status=%d token=%q", loginResp.StatusCode, loginPayload.AccessToken)
	}
	clientToken := loginPayload.AccessToken

	// 2. Scenario 5 (Active playback protection): start playing on Unicom (Master), then switch phone to Mobile (2.2.2.2) mid-playback.
	// MUST stay on Master with tok-1, while live dashboard IP updates to 2.2.2.2 (Mobile)!
	playReq, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Sessions/Playing", nil)
	playReq.Header.Set("X-Real-IP", "1.1.1.1")
	playReq.Header.Set("X-Emby-Token", clientToken)
	playResp, _ := noRedirect.Do(playReq)
	_ = playResp.Body.Close()

	code, edge, tok := callItems("2.2.2.2", clientToken)
	if code != 200 || edge != "master" || tok != "tok-1" {
		t.Fatalf("mid-playback switch MUST stay on master with tok-1; got status=%d edge=%q tok=%q", code, edge, tok)
	}
	liveSessions, _ := pm.shares.listAllLiveSessions()
	if len(liveSessions) != 1 || liveSessions[0].ClientIP != "2.2.2.2" || !strings.Contains(liveSessions[0].ClientRegion, "移动") {
		t.Fatalf("live session should reflect updated Mobile IP immediately, got %+v", liveSessions)
	}

	// 3. Stop playback -> Scenario 1 & 6: Master -> Edge-CM handoff under 8 concurrent requests!
	stopReq, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Sessions/Playing/Stopped", nil)
	stopReq.Header.Set("X-Real-IP", "2.2.2.2")
	stopReq.Header.Set("X-Emby-Token", clientToken)
	stopResp, _ := noRedirect.Do(stopReq)
	_ = stopResp.Body.Close()

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, e, tk := callItems("2.2.2.2", clientToken)
			if st != 200 || e != "edge-cm" || tk != "tok-2" {
				errs <- fmt.Sprintf("got status=%d edge=%q tok=%q", st, e, tk)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatalf("concurrent Master->Edge-CM handoff failed: %s", msg)
	}

	// Verify 307 video stream & WebSocket BOTH use tok-2 via edge-cm while client still sends tok-1!
	vidReq, _ := http.NewRequest(http.MethodGet, masterSrv.URL+"/Videos/101/stream.mkv?Static=true&api_key="+clientToken, nil)
	vidReq.Header.Set("X-Real-IP", "2.2.2.2")
	vidResp, _ := noRedirect.Do(vidReq)
	_ = vidResp.Body.Close()
	if vidResp.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(vidResp.Header.Get("Location"), edgeCMSrv.URL+edgeTicketPathPrefix) {
		t.Fatalf("video 307 after handoff: status=%d loc=%q", vidResp.StatusCode, vidResp.Header.Get("Location"))
	}
	streamResp, _ := http.Get(vidResp.Header.Get("Location"))
	streamBody, _ := io.ReadAll(streamResp.Body)
	_ = streamResp.Body.Close()
	if string(streamBody) != "stream:edge-cm:tok-2" {
		t.Fatalf("stream body = %q, want stream:edge-cm:tok-2", string(streamBody))
	}

	// Clear playback state so subsequent handoffs can run.
	stopReq2, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Sessions/Playing/Stopped", nil)
	stopReq2.Header.Set("X-Real-IP", "2.2.2.2")
	stopReq2.Header.Set("X-Emby-Token", clientToken)
	stopResp2, _ := noRedirect.Do(stopReq2)
	_ = stopResp2.Body.Close()

	// WebSocket after handoff -> must rewrite api_key=tok-1 to tok-2 over edge-cm!
	masterURL, _ := url.Parse(masterSrv.URL)
	wsConn, err := net.DialTimeout("tcp", masterURL.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	_, _ = io.WriteString(wsConn, "GET /embywebsocket?api_key="+clientToken+" HTTP/1.1\r\nHost: "+masterURL.Host+"\r\nX-Real-IP: 2.2.2.2\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	wsReader := bufio.NewReader(wsConn)
	wsResp, err := http.ReadResponse(wsReader, nil)
	if err != nil || wsResp.StatusCode != http.StatusSwitchingProtocols {
		_ = wsConn.Close()
		t.Fatalf("ws handshake err=%v resp=%v", err, wsResp)
	}
	_, _ = io.WriteString(wsConn, "hi\n")
	wsLine, _ := wsReader.ReadString('\n')
	_ = wsConn.Close()
	if wsLine != "ws:edge-cm:tok-2:hi\n" {
		t.Fatalf("ws echo = %q, want ws:edge-cm:tok-2:hi", wsLine)
	}

	// Persist & simulate restart via reload() -> tok-1 -> tok-2 mapping MUST survive reload!
	if err := pm.shares.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	code, edge, tok = callItems("2.2.2.2", clientToken)
	if code != 200 || edge != "edge-cm" || tok != "tok-2" {
		t.Fatalf("post-reload session lost rewritten token: status=%d edge=%q tok=%q", code, edge, tok)
	}

	// 4. Scenario 3: Edge-CM -> Edge-CT (switch to Telecom 3.3.3.3 after cooldown)
	time.Sleep(30 * time.Millisecond)
	code, edge, tok = callItems("3.3.3.3", clientToken)
	if code != 200 || edge != "edge-ct" || tok != "tok-3" {
		t.Fatalf("Edge-CM -> Edge-CT handoff got status=%d edge=%q tok=%q", code, edge, tok)
	}

	// 5. Scenario 2 & 4: Edge-CT -> Master (switch back to Unicom 1.1.1.1 after cooldown)
	time.Sleep(30 * time.Millisecond)
	code, edge, tok = callItems("1.1.1.1", clientToken)
	if code != 200 || edge != "master" || tok != "tok-4" {
		t.Fatalf("Edge-CT -> Master handoff got status=%d edge=%q tok=%q", code, edge, tok)
	}

	// 6. Scenario 9: Outdated v1.4.7 edge node MUST NOT break working session!
	edgeRT.mu.Lock()
	edgeRT.nodes[edgeCMID].agentVersion = "v1.4.7"
	edgeRT.nodes[edgeCMID].supportsEgress = false
	edgeRT.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	// Mobile now has edge-cm (outdated, skipped) -> master (enabled backup) -> stays on master with tok-4!
	code, edge, tok = callItems("2.2.2.2", clientToken)
	if code != 200 || edge != "master" || tok != "tok-4" {
		t.Fatalf("outdated target node should not break session: status=%d edge=%q tok=%q", code, edge, tok)
	}
	// Restore edge-cm to v1.4.9 so we can test Scenario 8 (crash failover) & Scenario 7 (balance stability).
	edgeRT.mu.Lock()
	edgeRT.nodes[edgeCMID].agentVersion = appVersion
	edgeRT.nodes[edgeCMID].supportsEgress = true
	edgeRT.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	code, edge, tok = callItems("2.2.2.2", clientToken)
	if code != 200 || edge != "edge-cm" || tok != "tok-5" {
		t.Fatalf("hand back to upgraded edge-cm got status=%d edge=%q tok=%q", code, edge, tok)
	}

	// 7. Scenario 7: Balance strategy in same ISP MUST NOT thrash between edge-cm and edge-ct!
	setRoute("cm", ispStrategyBalance, edgeCMID, edgeCTID, 0)
	time.Sleep(30 * time.Millisecond)
	for i := 0; i < 4; i++ {
		code, edge, tok = callItems("2.2.2.2", clientToken)
		if code != 200 || edge != "edge-cm" || tok != "tok-5" {
			t.Fatalf("balance strategy same ISP thrashed on req %d: status=%d edge=%q tok=%q", i, code, edge, tok)
		}
	}

	// 8. Scenario 8: Bound edge-cm goes offline -> auto-ISP mode seamlessly fails over to online edge-ct!
	edgeRT.mu.Lock()
	edgeRT.nodes[edgeCMID].lastSeen = time.Now().Add(-5 * time.Minute)
	edgeRT.mu.Unlock()
	code, edge, tok = callItems("2.2.2.2", clientToken)
	if code != 200 || edge != "edge-ct" || tok != "tok-6" {
		t.Fatalf("offline edge-cm should fail over to edge-ct: status=%d edge=%q tok=%q", code, edge, tok)
	}

	// 9. Scenario 10: Admin kick logs out latest upstream token (tok-6) via latest node (edge-ct)!
	if _, err := pm.shares.revokeSessions(bob.ID, 0, nil); err != nil {
		t.Fatalf("revokeSessions: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, stillActive := activeUpstreamTokens["tok-6"]
		mu.Unlock()
		if !stillActive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Final invariant: ZERO cross-IP token violations across the entire test,
	// and every online token (tok-1, tok-2, tok-3, tok-4, tok-6) was logged out
	// from its exact bound node (only tok-5 on the deliberately crashed/offline
	// edge-cm was skipped to prevent master IP leak!).
	mu.Lock()
	if crossIPViolation != "" {
		mu.Unlock()
		t.Fatalf("SINGLE-IP VIOLATION DETECTED: %s", crossIPViolation)
	}
	for tk, bound := range activeUpstreamTokens {
		if tk != "tok-5" || bound != "edge-cm" {
			mu.Unlock()
			t.Fatalf("leaked active upstream token %s on %s (missing logout): %+v", tk, bound, activeUpstreamTokens)
		}
	}
	mu.Unlock()

	// 10. Scenario 10b & 11: Client-initiated POST /Sessions/Logout rewrites token & drops local session;
	// and User.Id mismatch on handoff aborts cleanly with 401.
	loginReq2, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"bob","Pw":"bob-pass-123"}`))
	loginReq2.Header.Set("Content-Type", "application/json")
	loginReq2.Header.Set("X-Real-IP", "1.1.1.1")
	loginResp2, _ := noRedirect.Do(loginReq2)
	var lp2 struct {
		AccessToken string `json:"AccessToken"`
	}
	_ = json.NewDecoder(loginResp2.Body).Decode(&lp2)
	_ = loginResp2.Body.Close()
	if loginResp2.StatusCode != http.StatusOK || lp2.AccessToken == "" {
		t.Fatalf("relogin2 failed: status=%d", loginResp2.StatusCode)
	}
	// Trigger User.Id mismatch on handoff (Unicom -> Telecom): MUST return 401 and drop session!
	forceUID.Store("uid-changed")
	code, _, _ = callItems("3.3.3.3", lp2.AccessToken)
	if code != http.StatusUnauthorized {
		t.Fatalf("User.Id mismatch handoff status = %d, want 401", code)
	}
	forceUID.Store("uid-1")

	// Login again and test client-initiated POST /Sessions/Logout -> drops local session without handoff!
	loginReq3, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"bob","Pw":"bob-pass-123"}`))
	loginReq3.Header.Set("Content-Type", "application/json")
	loginReq3.Header.Set("X-Real-IP", "1.1.1.1")
	loginResp3, _ := noRedirect.Do(loginReq3)
	var lp3 struct {
		AccessToken string `json:"AccessToken"`
	}
	_ = json.NewDecoder(loginResp3.Body).Decode(&lp3)
	_ = loginResp3.Body.Close()

	clientLogoutReq, _ := http.NewRequest(http.MethodPost, masterSrv.URL+"/Sessions/Logout", nil)
	clientLogoutReq.Header.Set("X-Real-IP", "3.3.3.3") // even from another ISP, client logout must NOT hand off!
	clientLogoutReq.Header.Set("X-Emby-Token", lp3.AccessToken)
	clientLogoutResp, _ := noRedirect.Do(clientLogoutReq)
	_ = clientLogoutResp.Body.Close()
	if clientLogoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("client logout status=%d, want 204", clientLogoutResp.StatusCode)
	}
	if _, exists := pm.shares.sessionEgressNodeID(shareTokenHash(lp3.AccessToken)); exists {
		t.Fatalf("client-initiated logout should remove local session")
	}
}
