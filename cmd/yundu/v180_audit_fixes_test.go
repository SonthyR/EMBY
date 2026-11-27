package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #1: Dynamic media via Edge 307 hop-by-hop validation + capability negotiation

type staticDynamicResolver struct {
	mu      sync.Mutex
	records map[string][]net.IP
}

func (r *staticDynamicResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	ips := append([]net.IP(nil), r.records[strings.ToLower(strings.TrimSpace(host))]...)
	r.mu.Unlock()
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

func TestV180EdgeDynamicGuardHopByHopAndFallback(t *testing.T) {
	// Loopback servers simulating:
	//  - allowedMedia: normal public media supporting Range
	//  - redirectToPrivate: redirects to 127.0.0.1 private target (must be blocked)
	//  - forbiddenPrivate: private server that MUST receive 0 requests
	var forbiddenHits atomic.Int32
	forbiddenPrivate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forbiddenHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret-private-data"))
	}))
	defer forbiddenPrivate.Close()

	allowedPayload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	var allowedHits atomic.Int32
	allowedMedia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedHits.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		if rng := r.Header.Get("Range"); rng == "bytes=0-9" {
			w.Header().Set("Content-Range", "bytes 0-9/36")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(allowedPayload[:10])
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(allowedPayload)
	}))
	defer allowedMedia.Close()

	redirectToPrivate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, forbiddenPrivate.URL+"/internal", http.StatusFound)
	}))
	defer redirectToPrivate.Close()

	allowedAddr := allowedMedia.Listener.Addr().String()
	redirectAddr := redirectToPrivate.Listener.Addr().String()
	_, allowedPort, _ := net.SplitHostPort(allowedAddr)
	_, redirectPort, _ := net.SplitHostPort(redirectAddr)

	// Public-looking test IPs (outside RFC1918 & IANA reserved ranges in dynamicForbiddenPrefixes).
	const (
		fakePublicIPAllowed  = "93.184.216.34"
		fakePublicIPRedirect = "93.184.216.35"
	)
	resolver := &staticDynamicResolver{
		records: map[string][]net.IP{
			"cdn.example.com":  {net.ParseIP(fakePublicIPAllowed)},
			"hop.example.com":  {net.ParseIP(fakePublicIPRedirect)},
			"evil.example.com": {net.ParseIP("10.0.0.8")},
		},
	}
	dialMap := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var realAddr string
		switch host {
		case fakePublicIPAllowed:
			realAddr = allowedAddr
		case fakePublicIPRedirect:
			realAddr = redirectAddr
		default:
			return nil, &net.OpError{Op: "dial", Net: network, Err: io.EOF}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, realAddr)
	}
	noLocalIfaces := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("198.18.0.1"), Mask: net.CIDRMask(24, 32)}}, nil
	}

	app := newTestApp(t)
	db := app.db
	pm := app.pm
	edgeRT := newEdgeNodeRuntime(db, pm, nil)
	pm.SetEdgeRuntime(edgeRT)

	recNode := httptest.NewRecorder()
	app.handleEdgeNodes(recNode, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"edge-dyn-1","public_port":19090,"public_host":"edge1.example.com","tls_mode":"http"}`)))
	if recNode.Code != http.StatusCreated {
		t.Fatalf("createEdgeNode status=%d body=%s", recNode.Code, recNode.Body.String())
	}
	var nodeRec EdgeNode
	if err := json.Unmarshal(recNode.Body.Bytes(), &nodeRec); err != nil {
		t.Fatalf("decode edge node: %v", err)
	}
	edgeRT.mu.RLock()
	token := edgeRT.nodes[nodeRec.ID].tokenPlain
	edgeRT.mu.RUnlock()

	// Start real edgeAgent on loopback.
	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: "http://master.example.com:9090",
		NodeID:    nodeRec.ID,
		Token:     token,
		Port:      19090,
	})
	agent.dynamicResolver = resolver
	agent.dynamicDial = dialMap
	agent.dynamicInterfaceAddrs = noLocalIfaces
	agentSrv := httptest.NewServer(agent.handler())
	defer agentSrv.Close()

	// Register node as online & routable with DynamicGuard capability.
	agentURL, _ := url.Parse(agentSrv.URL)
	agentPort, _ := strconv.Atoi(agentURL.Port())
	edgeRT.mu.Lock()
	liveNode := edgeRT.nodes[nodeRec.ID]
	liveNode.publicHost = agentURL.Hostname()
	liveNode.publicPort = agentPort
	liveNode.tlsMode = edgeNodeTLSModeHTTP
	liveNode.lastSeen = time.Now()
	liveNode.supportsDynamicGuard = true
	liveNode.supportsEgress = true
	edgeRT.routes[ispGroupOther] = []ispRouteRule{{NodeID: nodeRec.ID, SortOrder: 1, Enabled: true}}
	edgeRT.mu.Unlock()

	policy, err := newDynamicRedirectPolicy(Site{
		DynamicDiscoveryEnabled:    true,
		DynamicProfile:             dynamicProfileCompatible,
		DynamicDiscoverySources:    allDynamicDiscoverySources(),
		DynamicAllowHTTPSDowngrade: true,
	}, true)
	if err != nil {
		t.Fatalf("newDynamicRedirectPolicy: %v", err)
	}
	selfTargets, err := newDynamicSelfTargetPolicy("panel.example.com", 9090, nil, noLocalIfaces)
	if err != nil {
		t.Fatalf("newDynamicSelfTargetPolicy: %v", err)
	}

	// 1. Allowed dynamic resource via edge 307 supports Range end-to-end.
	allowedTarget, _ := url.Parse("http://cdn.example.com:" + allowedPort + "/video.mp4")
	reqAllowed := httptest.NewRequest(http.MethodGet, "/_meridian/d/tok", nil)
	reqAllowed.Header.Set("Range", "bytes=0-9")
	recAllowed := httptest.NewRecorder()
	handled, denied := tryRedirectDynamicResourceToEdgeNode(
		recAllowed, reqAllowed, pm,
		Site{ID: 1, EdgeRoutingMode: edgeRoutingModeGlobal},
		allowedTarget, http.Header{"User-Agent": []string{"TestPlayer/1.0"}},
		dynamicCapabilityClaims{Trusted: false, Source: dynamicDiscoverySourceRedirect, Kind: dynamicCapabilityKindResource},
		policy, selfTargets,
	)
	if !handled || denied != "" || recAllowed.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected edge 307 redirect, got handled=%v denied=%q code=%d", handled, denied, recAllowed.Code)
	}
	edgeMediaReq, _ := http.NewRequest(http.MethodGet, recAllowed.Header().Get("Location"), nil)
	edgeMediaReq.Header.Set("Range", "bytes=0-9")
	edgeMediaResp, err := http.DefaultClient.Do(edgeMediaReq)
	if err != nil {
		t.Fatalf("edge range fetch failed: %v", err)
	}
	gotBody, _ := io.ReadAll(edgeMediaResp.Body)
	_ = edgeMediaResp.Body.Close()
	if edgeMediaResp.StatusCode != http.StatusPartialContent || string(gotBody) != "0123456789" {
		t.Fatalf("expected 206 Range '0123456789', got status=%d body=%q", edgeMediaResp.StatusCode, string(gotBody))
	}

	// 2. Subsequent redirect from public hop -> private 127.0.0.1 target MUST be blocked by edge agent!
	redirectTarget, _ := url.Parse("http://hop.example.com:" + redirectPort + "/redir")
	reqHop := httptest.NewRequest(http.MethodGet, "/_meridian/d/tok2", nil)
	recHop := httptest.NewRecorder()
	handled, denied = tryRedirectDynamicResourceToEdgeNode(
		recHop, reqHop, pm,
		Site{ID: 1, EdgeRoutingMode: edgeRoutingModeGlobal},
		redirectTarget, http.Header{},
		dynamicCapabilityClaims{Trusted: false, Source: dynamicDiscoverySourceRedirect, Kind: dynamicCapabilityKindResource},
		policy, selfTargets,
	)
	if !handled || denied != "" || recHop.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected edge 307 for hop target, got handled=%v denied=%q code=%d", handled, denied, recHop.Code)
	}
	hopResp, err := http.Get(recHop.Header().Get("Location"))
	if err != nil {
		t.Fatalf("hop request failed: %v", err)
	}
	_ = hopResp.Body.Close()
	if hopResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when edge follows redirect to private IP, got %d", hopResp.StatusCode)
	}
	if forbiddenHits.Load() != 0 {
		t.Fatalf("forbidden private target received %d hits, want 0", forbiddenHits.Load())
	}

	// 3. Direct DNS-rebind to private IP (evil.example.com -> 10.0.0.8) MUST be blocked by edge agent!
	evilTarget, _ := url.Parse("http://evil.example.com:8080/media.mp4")
	evilTicket, err := sealEdgePlaybackTicket(token, edgePlaybackTicket{
		NodeID:       nodeRec.ID,
		SiteID:       1,
		TargetURL:    evilTarget.String(),
		FollowRedir:  true,
		DynamicGuard: buildEdgeDynamicGuardPolicy(dynamicCapabilityClaims{Trusted: false}, policy, selfTargets),
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("seal evil ticket: %v", err)
	}
	evilResp, err := http.Get(agentSrv.URL + edgeTicketPathPrefix + evilTicket + "/media.mp4")
	if err != nil {
		t.Fatalf("evil request failed: %v", err)
	}
	_ = evilResp.Body.Close()
	if evilResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 for private DNS target on edge, got %d", evilResp.StatusCode)
	}

	// 4. Downgraded/legacy agent (heartbeat without X-Yundu-Dynamic-Guard) invalidates capability!
	hbPayload, _ := json.Marshal(agentHeartbeatRequest{Version: "v1.7.0", ListenPort: agentPort, ActiveStreams: 0})
	hbReq := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", bytes.NewReader(hbPayload))
	hbReq.Header.Set("X-Yundu-Node-ID", strconv.FormatInt(nodeRec.ID, 10))
	hbReq.Header.Set("X-Yundu-Node-Token", token)
	hbReq.Header.Set(edgeAgentInstanceHeader, "downgraded-proc-1")
	hbRec := httptest.NewRecorder()
	app.handleAgentHeartbeat(hbRec, hbReq)
	if hbRec.Code != http.StatusOK {
		t.Fatalf("downgraded heartbeat status=%d body=%s", hbRec.Code, hbRec.Body.String())
	}
	if edgeRT.nodeSupportsDynamicGuard(nodeRec.ID) {
		t.Fatalf("expected supportsDynamicGuard=false after downgraded heartbeat without header")
	}

	// Normal global mode falls back to master (handled=false, denied="").
	recFallback := httptest.NewRecorder()
	handled, denied = tryRedirectDynamicResourceToEdgeNode(
		recFallback, httptest.NewRequest(http.MethodGet, "/_meridian/d/tok3", nil), pm,
		Site{ID: 1, EdgeRoutingMode: edgeRoutingModeGlobal},
		allowedTarget, http.Header{},
		dynamicCapabilityClaims{Trusted: false, Source: dynamicDiscoverySourceRedirect, Kind: dynamicCapabilityKindResource},
		policy, selfTargets,
	)
	if handled || denied != "" {
		t.Fatalf("expected fallback to master for non-edge_full legacy agent, got handled=%v denied=%q", handled, denied)
	}

	// Bound edge_full session (including auto-selected & session-bound) MUST fail closed!
	decision, ok := edgeRT.nodeDecisionByID(nodeRec.ID)
	if !ok {
		t.Fatalf("expected node decision")
	}
	reqEdgeFull := httptest.NewRequest(http.MethodGet, "/_meridian/d/tok4", nil)
	reqEdgeFull = reqEdgeFull.WithContext(withEdgeRequestContext(reqEdgeFull.Context(), "1.2.3.4", nil, "tokhash", true, decision))
	recEdgeFull := httptest.NewRecorder()
	handled, denied = tryRedirectDynamicResourceToEdgeNode(
		recEdgeFull, reqEdgeFull, pm,
		Site{ID: 1, EdgeRoutingMode: edgeRoutingModeEdgeFull, EdgePinnedNodeID: 0}, // auto-selected session-bound edge_full
		allowedTarget, http.Header{},
		dynamicCapabilityClaims{Trusted: false, Source: dynamicDiscoverySourceRedirect, Kind: dynamicCapabilityKindResource},
		policy, selfTargets,
	)
	if handled || denied == "" {
		t.Fatalf("expected edge_full bound to legacy agent to fail closed, got handled=%v denied=%q", handled, denied)
	}
}

// ---------------------------------------------------------------------------
// #2: Danmaku proxy isolation, allowlist, compression, oversize, HEAD, 304, CORS

func TestV180DanmakuProxyIsolationAndLimits(t *testing.T) {
	app, site := setupDanmakuTestApp(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xml":
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.Header().Set("X-Evil-Header", "should-not-leak")
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write([]byte(`<i><d p="1,1,25,16777215">hello</d></i>`))
		case "/text":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("plain-danmaku"))
		case "/html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html><script>alert(1)</script></html>"))
		case "/svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write([]byte(`<svg onload="alert(1)"/>`))
		case "/gzip":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(`{"comments":[1,2,3]}`))
			_ = gz.Close()
		case "/oversize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte("a"), 2048))
		case "/notmod":
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()

	createBody, _ := json.Marshal(map[string]any{
		"name":         "DMTest",
		"upstream_url": upstream.URL,
		"enabled":      true,
	})
	cRec := httptest.NewRecorder()
	app.handleDanmakuSources(cRec, httptest.NewRequest(http.MethodPost, "/api/danmaku-sources", bytes.NewReader(createBody)))
	var src DanmakuSource
	_ = json.Unmarshal(cRec.Body.Bytes(), &src)

	user := createDanmakuTestShareUser(t, app, site.ID, "dm_user", time.Now().AddDate(0, 1, 0).Format("2006-01-02"), true, 0, []int64{src.ID})
	stored, err := app.db.getShareUser(user.ID)
	if err != nil {
		t.Fatalf("getShareUser: %v", err)
	}
	basePath := "/dm/" + strconv.FormatInt(src.ID, 10) + "/" + stored.danmakuToken

	assertIsolated := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") || !strings.Contains(got, "default-src 'none'") {
			t.Fatalf("missing strict sandbox CSP: %q", got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("missing nosniff: %q", got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("missing X-Frame-Options DENY: %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("missing CORS *: %q", got)
		}
	}

	// 1. XML allowed, non-allowlisted headers stripped.
	recXML := httptest.NewRecorder()
	app.handleDanmakuProxy(recXML, httptest.NewRequest(http.MethodGet, basePath+"/xml", nil))
	if recXML.Code != http.StatusOK || recXML.Header().Get("X-Evil-Header") != "" || recXML.Header().Get("ETag") != `"v1"` {
		t.Fatalf("XML response mismatch: code=%d evil=%q etag=%q", recXML.Code, recXML.Header().Get("X-Evil-Header"), recXML.Header().Get("ETag"))
	}
	assertIsolated(t, recXML)

	// 2. Plain text allowed.
	recText := httptest.NewRecorder()
	app.handleDanmakuProxy(recText, httptest.NewRequest(http.MethodGet, basePath+"/text", nil))
	if recText.Code != http.StatusOK || recText.Body.String() != "plain-danmaku" {
		t.Fatalf("text response mismatch: code=%d body=%q", recText.Code, recText.Body.String())
	}
	assertIsolated(t, recText)

	// 3. Active content (HTML & SVG) MUST be rejected with 502!
	for _, p := range []string{"/html", "/svg"} {
		rec := httptest.NewRecorder()
		app.handleDanmakuProxy(rec, httptest.NewRequest(http.MethodGet, basePath+p, nil))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 for %s, got %d body=%s", p, rec.Code, rec.Body.String())
		}
		assertIsolated(t, rec)
	}

	// 4. Compressed response preserves encoding and exact Content-Length.
	gzReq := httptest.NewRequest(http.MethodGet, basePath+"/gzip", nil)
	gzReq.Header.Set("Accept-Encoding", "gzip")
	recGZ := httptest.NewRecorder()
	app.handleDanmakuProxy(recGZ, gzReq)
	if recGZ.Code != http.StatusOK || recGZ.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response mismatch: code=%d ce=%q", recGZ.Code, recGZ.Header().Get("Content-Encoding"))
	}
	if recGZ.Header().Get("Content-Length") != strconv.Itoa(recGZ.Body.Len()) {
		t.Fatalf("gzip Content-Length=%q does not match actual wire len=%d", recGZ.Header().Get("Content-Length"), recGZ.Body.Len())
	}
	gr, err := gzip.NewReader(bytes.NewReader(recGZ.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}
	unpacked, _ := io.ReadAll(gr)
	_ = gr.Close()
	if string(unpacked) != `{"comments":[1,2,3]}` {
		t.Fatalf("unexpected unpacked gzip body: %q", string(unpacked))
	}

	// 5. Oversize response fails cleanly with 502 (never truncated 200!).
	prevMax := danmakuMaxRespBodyBytes
	danmakuMaxRespBodyBytes = 1024
	defer func() { danmakuMaxRespBodyBytes = prevMax }()
	recBig := httptest.NewRecorder()
	app.handleDanmakuProxy(recBig, httptest.NewRequest(http.MethodGet, basePath+"/oversize", nil))
	if recBig.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for oversized response, got %d", recBig.Code)
	}
	assertIsolated(t, recBig)

	// 6. HEAD, 304, and OPTIONS CORS preflight.
	recHead := httptest.NewRecorder()
	app.handleDanmakuProxy(recHead, httptest.NewRequest(http.MethodHead, basePath+"/text", nil))
	if recHead.Code != http.StatusOK || recHead.Body.Len() != 0 {
		t.Fatalf("HEAD mismatch: code=%d bodyLen=%d", recHead.Code, recHead.Body.Len())
	}
	assertIsolated(t, recHead)

	rec304 := httptest.NewRecorder()
	app.handleDanmakuProxy(rec304, httptest.NewRequest(http.MethodGet, basePath+"/notmod", nil))
	if rec304.Code != http.StatusNotModified || rec304.Body.Len() != 0 {
		t.Fatalf("304 mismatch: code=%d bodyLen=%d", rec304.Code, rec304.Body.Len())
	}
	assertIsolated(t, rec304)

	recOpt := httptest.NewRecorder()
	app.handleDanmakuProxy(recOpt, httptest.NewRequest(http.MethodOptions, basePath+"/text", nil))
	if recOpt.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS mismatch: code=%d", recOpt.Code)
	}
	assertIsolated(t, recOpt)
}

// ---------------------------------------------------------------------------
// #4: Seamless switch preserves old session on failure & prevents kick resurrection

func TestV180SeamlessSwitchPreservesOldSessionAndRespectsKick(t *testing.T) {
	app := newTestApp(t)
	db := app.db
	pm := app.pm
	rt := pm.shares

	siteA, err := db.CreateSiteRecord(Site{Name: "SiteA", IngressMode: ingressModePath, PathPrefix: "/a", ListenPort: 20101, TargetURL: "http://127.0.0.1:8096", Enabled: true})
	if err != nil {
		t.Fatalf("create siteA: %v", err)
	}
	siteB, err := db.CreateSiteRecord(Site{Name: "SiteB", IngressMode: ingressModePath, PathPrefix: "/b", ListenPort: 20102, TargetURL: "http://127.0.0.1:8097", Enabled: true})
	if err != nil {
		t.Fatalf("create siteB: %v", err)
	}
	if _, err := db.saveSiteAccount(siteB.ID, 0, SiteAccountInput{Username: "accB", Password: "pwB"}); err != nil {
		t.Fatalf("create account B: %v", err)
	}

	user := createDanmakuTestShareUser(t, app, siteA.ID, "switch_u", "", true, 0, nil)
	if _, err := db.db.Exec(`INSERT INTO share_user_sites(share_user_id, site_id, site_account_id, sort_order) VALUES(?, ?, 0, 2)`, user.ID, siteB.ID); err != nil {
		t.Fatalf("assign siteB: %v", err)
	}
	if err := rt.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	var oldLogoutCount atomic.Int32
	var newLogoutCount atomic.Int32
	var loginMode atomic.Int32 // 0=503 fail, 1=succeed, 2=succeed-but-kick-midflight

	clientToken := "client-tok-v180"
	tokenHash := shareTokenHash(clientToken)
	loginInFlight := make(chan struct{})
	releaseLogin := make(chan struct{})

	pm.mu.Lock()
	pm.proxies[siteA.ID] = &ProxyInstance{
		Site: *siteA,
		shareLogout: func(ctx context.Context, tok string) error {
			oldLogoutCount.Add(1)
			return nil
		},
	}
	pm.proxies[siteB.ID] = &ProxyInstance{
		Site: *siteB,
		shareLogin: func(ctx context.Context, u, p, devID, devName, clientName string) (string, string, int, error) {
			switch loginMode.Load() {
			case 0:
				return "", "", http.StatusBadGateway, nil
			case 2:
				close(loginInFlight)
				<-releaseLogin
				return "temp-new-tok-kicked", "uid-b-2", http.StatusOK, nil
			default:
				return "new-tok-b", "uid-b-1", http.StatusOK, nil
			}
		},
		shareLogout: func(ctx context.Context, tok string) error {
			newLogoutCount.Add(1)
			return nil
		},
	}
	pm.mu.Unlock()

	seedSession := func() {
		upCipher, _ := encryptSiteAccountSecret("old-tok-a")
		if _, err := db.db.Exec(
			`INSERT OR REPLACE INTO share_user_sessions(token_hash, share_user_id, site_id, site_account_id, egress_node_id, token_ciphertext, upstream_token_ciphertext, upstream_user_id, client_user_id, device_id, device_name, client, client_ip) VALUES(?, ?, ?, 0, 0, ?, ?, 'uid-a', 'uid-a', 'dev-1', 'TV', 'Capy', '1.2.3.4')`,
			tokenHash, user.ID, siteA.ID, upCipher, upCipher,
		); err != nil {
			t.Fatalf("seed session: %v", err)
		}
		rt.mu.Lock()
		delete(rt.revoked, tokenHash)
		rt.sessions[tokenHash] = shareSessionEntry{
			userID:        user.ID,
			siteID:        siteA.ID,
			clientToken:   clientToken,
			upstreamToken: "old-tok-a",
			clientUID:     "uid-a",
			upstreamUID:   "uid-a",
			deviceID:      "dev-1",
		}
		rt.mu.Unlock()
	}

	// Case 1: Target 502 MUST preserve old session (returns 503, does not revoke or logout old token).
	seedSession()
	loginMode.Store(0)
	status, _ := rt.handoffPersonalRouteSite(context.Background(), tokenHash, siteB.ID, "1.2.3.4")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on target 502, got %d", status)
	}
	rt.mu.RLock()
	cur, exists := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !exists || cur.siteID != siteA.ID || cur.upstreamToken != "old-tok-a" {
		t.Fatalf("old session was lost on transient target failure: exists=%v cur=%+v", exists, cur)
	}
	if oldLogoutCount.Load() != 0 {
		t.Fatalf("old upstream token should not be logged out on failure")
	}

	// Case 2: Target site unassigned while old site A still assigned -> preserve A!
	rt.mu.Lock()
	delete(rt.users[user.ID].sites, siteB.ID)
	rt.mu.Unlock()
	status, _ = rt.handoffPersonalRouteSite(context.Background(), tokenHash, siteB.ID, "1.2.3.4")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when target B unassigned but A remains, got %d", status)
	}
	rt.mu.RLock()
	_, exists = rt.sessions[tokenHash]
	rt.users[user.ID].sites[siteB.ID] = 0
	rt.mu.RUnlock()
	if !exists {
		t.Fatalf("session on A should remain alive when only B lost permission")
	}

	// Case 3: Mid-flight admin kick during slow target login MUST NOT resurrect session!
	seedSession()
	loginMode.Store(2)
	doneCh := make(chan int, 1)
	go func() {
		st, _ := rt.handoffPersonalRouteSite(context.Background(), tokenHash, siteB.ID, "1.2.3.4")
		doneCh <- st
	}()
	<-loginInFlight
	if err := rt.revokeByHash(tokenHash); err != nil {
		t.Fatalf("revokeByHash mid-flight: %v", err)
	}
	close(releaseLogin)
	if st := <-doneCh; st != http.StatusUnauthorized {
		t.Fatalf("expected 401 after mid-flight kick, got %d", st)
	}
	rt.mu.RLock()
	_, stillExists := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if stillExists {
		t.Fatalf("kicked session was resurrected by in-flight handoff")
	}
	time.Sleep(50 * time.Millisecond)
	if newLogoutCount.Load() == 0 {
		t.Fatalf("expected temporary new token to be logged out after mid-flight kick")
	}

	// Case 4: Successful switch commits to DB and survives reload!
	seedSession()
	loginMode.Store(1)
	status, msg := rt.handoffPersonalRouteSite(context.Background(), tokenHash, siteB.ID, "1.2.3.4")
	if status != 0 {
		t.Fatalf("expected successful handoff, got status=%d msg=%s", status, msg)
	}
	rt2 := newShareUserRuntime(db, pm)
	if err := rt2.reload(); err != nil {
		t.Fatalf("rt2 reload: %v", err)
	}
	rt2.mu.RLock()
	reloaded, ok := rt2.sessions[tokenHash]
	rt2.mu.RUnlock()
	if !ok || reloaded.siteID != siteB.ID || reloaded.upstreamToken != "new-tok-b" {
		t.Fatalf("expected persisted session on siteB after restart, got ok=%v entry=%+v", ok, reloaded)
	}
}

// ---------------------------------------------------------------------------
// #5: Draining state consistency (TV playing A, Phone idle on B)

func TestV180InPlayerSwitchDrainingStateConsistencyMultiDevice(t *testing.T) {
	app := newTestApp(t)
	db := app.db
	pm := app.pm
	rt := pm.shares

	siteA, err := db.CreateSiteRecord(Site{Name: "UHD", IngressMode: ingressModePath, PathPrefix: "/uhd", ListenPort: 20201, TargetURL: "http://127.0.0.1:8096", Enabled: true})
	if err != nil {
		t.Fatalf("create siteA: %v", err)
	}
	siteB, err := db.CreateSiteRecord(Site{Name: "SNTP", IngressMode: ingressModePath, PathPrefix: "/sntp", ListenPort: 20202, TargetURL: "http://127.0.0.1:8097", Enabled: true})
	if err != nil {
		t.Fatalf("create siteB: %v", err)
	}
	pm.mu.Lock()
	pm.proxies[siteA.ID] = &ProxyInstance{Site: *siteA}
	pm.proxies[siteB.ID] = &ProxyInstance{Site: *siteB}
	pm.mu.Unlock()

	// Give SiteA and SiteB distinct custom icons so covers differ byte-for-byte.
	pngA := renderInPlayerSwitchPNG("site", true, siteA.ID, false)
	pngB := renderInPlayerSwitchPNG("site", false, siteB.ID, false)
	if err := db.saveSiteCustomIcon(siteA.ID, pngA, "image/png"); err != nil {
		t.Fatalf("save icon A: %v", err)
	}
	if err := db.saveSiteCustomIcon(siteB.ID, pngB, "image/png"); err != nil {
		t.Fatalf("save icon B: %v", err)
	}

	user := createDanmakuTestShareUser(t, app, siteB.ID, "family_u", "", true, 0, nil)
	if _, err := db.db.Exec(`UPDATE share_users SET route_prefix='family' WHERE id=?`, user.ID); err != nil {
		t.Fatalf("set route_prefix: %v", err)
	}
	if _, err := db.db.Exec(`INSERT INTO share_user_sites(share_user_id, site_id, site_account_id, sort_order) VALUES(?, ?, 0, 2)`, user.ID, siteA.ID); err != nil {
		t.Fatalf("assign siteA: %v", err)
	}
	if err := rt.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	tvHash := shareTokenHash("tv-token")
	phoneHash := shareTokenHash("phone-token")
	rt.mu.Lock()
	shareState := rt.users[user.ID]
	rt.sessions[tvHash] = shareSessionEntry{userID: user.ID, siteID: siteA.ID, deviceID: "tv-1"}
	rt.activity[tvHash] = &shareSessionActivity{activeStreams: 1, playing: true, lastPlaybackAt: time.Now(), draining: true}
	rt.sessions[phoneHash] = shareSessionEntry{userID: user.ID, siteID: siteB.ID, deviceID: "phone-1"}
	rt.mu.Unlock()

	// 1. TV /Users/{uid}/Views shows "切服 · UHD（待切 SNTP）" and cover tag pointing to siteA.
	viewsRespTV := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"Items":[],"TotalRecordCount":0}`)),
		Request:    httptest.NewRequest(http.MethodGet, "http://emby/Users/u1/Views", nil),
	}
	if err := rt.injectInPlayerSwitchViewResponse(viewsRespTV, "", user.ID, siteA.ID, "vserver"); err != nil {
		t.Fatalf("inject TV views: %v", err)
	}
	var tvViews struct {
		Items []struct {
			Name      string            `json:"Name"`
			ImageTags map[string]string `json:"ImageTags"`
		} `json:"Items"`
	}
	_ = json.NewDecoder(viewsRespTV.Body).Decode(&tvViews)
	if len(tvViews.Items) != 1 || tvViews.Items[0].Name != "切服 · UHD（待切 SNTP）" {
		t.Fatalf("unexpected TV library view: %+v", tvViews.Items)
	}
	tvTag := tvViews.Items[0].ImageTags["Primary"]
	if !strings.HasPrefix(tvTag, "s"+strconv.FormatInt(siteA.ID, 10)+"-") {
		t.Fatalf("expected TV cover tag to encode siteA ID, got %q", tvTag)
	}

	// 2. TV cover request (even WITHOUT token, using ?tag=) gets A's cover; Phone gets B's cover.
	recCoverTV := httptest.NewRecorder()
	rt.handleInPlayerSiteSwitchRequest(recCoverTV, httptest.NewRequest(http.MethodGet, "/Items/"+shareSwitchLibraryID+"/Images/Primary?tag="+tvTag, nil), user.ID, nil, "", "vserver")
	if recCoverTV.Code != http.StatusOK || recCoverTV.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("TV cover mismatch: code=%d cc=%q", recCoverTV.Code, recCoverTV.Header().Get("Cache-Control"))
	}

	recCoverPhone := httptest.NewRecorder()
	rt.handleInPlayerSiteSwitchRequest(recCoverPhone, httptest.NewRequest(http.MethodGet, "/Items/"+shareSwitchLibraryID+"/Images/Primary", nil), user.ID, shareState, phoneHash, "vserver")
	if recCoverPhone.Code != http.StatusOK || bytes.Equal(recCoverTV.Body.Bytes(), recCoverPhone.Body.Bytes()) {
		t.Fatalf("expected distinct covers for TV (on A) vs Phone (on B)")
	}

	// 3. TV card list marks A as current (✅ UHD, IsFavorite=false) and B as pending (⏳ SNTP（待切）, IsFavorite=true).
	recItemsTV := httptest.NewRecorder()
	rt.handleInPlayerSiteSwitchRequest(recItemsTV, httptest.NewRequest(http.MethodGet, "/Users/u1/Items?ParentId="+shareSwitchLibraryID, nil), user.ID, shareState, tvHash, "vserver")
	var tvCards struct {
		Items []struct {
			Name          string `json:"Name"`
			OriginalTitle string `json:"OriginalTitle"`
			UserData      struct {
				IsFavorite bool `json:"IsFavorite"`
			} `json:"UserData"`
		} `json:"Items"`
	}
	_ = json.NewDecoder(recItemsTV.Body).Decode(&tvCards)
	if len(tvCards.Items) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(tvCards.Items))
	}
	for _, card := range tvCards.Items {
		switch card.OriginalTitle {
		case "UHD":
			if !strings.HasPrefix(card.Name, "✅") || card.UserData.IsFavorite {
				t.Fatalf("TV draining card A should be current & not favorite, got %+v", card)
			}
		case "SNTP":
			if !strings.Contains(card.Name, "待切") || !card.UserData.IsFavorite {
				t.Fatalf("TV draining card B should be pending & favorite, got %+v", card)
			}
		}
	}

	// 4. Phone card list marks B as current & favorite, A as normal non-favorite.
	recItemsPhone := httptest.NewRecorder()
	rt.handleInPlayerSiteSwitchRequest(recItemsPhone, httptest.NewRequest(http.MethodGet, "/Users/u1/Items?ParentId="+shareSwitchLibraryID, nil), user.ID, shareState, phoneHash, "vserver")
	var phoneCards struct {
		Items []struct {
			Name          string `json:"Name"`
			OriginalTitle string `json:"OriginalTitle"`
			UserData      struct {
				IsFavorite bool `json:"IsFavorite"`
			} `json:"UserData"`
		} `json:"Items"`
	}
	_ = json.NewDecoder(recItemsPhone.Body).Decode(&phoneCards)
	for _, card := range phoneCards.Items {
		if card.OriginalTitle == "SNTP" && (!strings.HasPrefix(card.Name, "✅") || !card.UserData.IsFavorite) {
			t.Fatalf("Phone card B should be active & favorite, got %+v", card)
		}
		if card.OriginalTitle == "UHD" && (strings.HasPrefix(card.Name, "✅") || card.UserData.IsFavorite) {
			t.Fatalf("Phone card A should be inactive, got %+v", card)
		}
	}
}

// ---------------------------------------------------------------------------
// #6: Site icon privacy, legacy auto-cache invalidation, and manual preview confirm

func TestV180SiteIconPrivacyLegacyCleanupAndPreviewConfirm(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v180-icons.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}

	site, err := db.CreateSiteRecord(Site{Name: "PrivSite", IngressMode: ingressModePath, PathPrefix: "/priv", ListenPort: 20301, TargetURL: "http://127.0.0.1:8096", Enabled: true})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	samplePNG := renderInPlayerSwitchPNG("site", true, site.ID, false)

	// 1. Simulate legacy v1.7.x auto icon row with empty auto_source, plus a second site with custom icon.
	siteCustom, err := db.CreateSiteRecord(Site{Name: "CustomSite", IngressMode: ingressModePath, PathPrefix: "/cust", ListenPort: 20302, TargetURL: "http://127.0.0.1:8097", Enabled: true})
	if err != nil {
		t.Fatalf("CreateSiteRecord(CustomSite): %v", err)
	}
	_, err = db.db.Exec(`INSERT INTO site_icons(site_id, auto_data, auto_content_type, auto_source) VALUES(?, ?, 'image/png', '')`, site.ID, samplePNG)
	if err != nil {
		t.Fatalf("insert legacy auto icon: %v", err)
	}
	if err := db.saveSiteCustomIcon(siteCustom.ID, samplePNG, "image/png"); err != nil {
		t.Fatalf("save custom icon: %v", err)
	}
	db.Close()

	// Re-open DB (runs migrations): legacy auto icon MUST be invalidated while custom icon survives!
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	defer db.Close()
	if got := db.getEffectiveSiteIcon(site.ID); got.Source != "default" || len(got.Data) != 0 {
		t.Fatalf("expected legacy unknown-provenance auto icon to be invalidated, got source=%s len=%d", got.Source, len(got.Data))
	}
	if got := db.getEffectiveSiteIcon(siteCustom.ID); got.Source != "custom" || len(got.Data) == 0 {
		t.Fatalf("expected admin custom icon to survive migration, got source=%s len=%d", got.Source, len(got.Data))
	}

	// 2. Passive capture MUST ignore /Users/{uid}/Images/Primary personal avatars, and accept /img/u/default!
	personalResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       io.NopCloser(bytes.NewReader(samplePNG)),
		Request:    httptest.NewRequest(http.MethodGet, "http://emby/Users/alice-uid/Images/Primary", nil),
	}
	captureSiteAvatarFromResponse(personalResp, "", site.ID, db)
	if db.hasAutoSiteIcon(site.ID) {
		t.Fatalf("passive capture must NOT cache /Users/{uid}/Images/Primary personal avatar")
	}

	// 3. Manual admin fetch: when upstream ONLY exposes account avatar (no public brand paths),
	//    it MUST NOT auto-save, and must return requires_confirm=true + preview_data_url!
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/admin-uid/Images/Primary" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(samplePNG)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x63}, 32))
	if _, err := db.saveSiteAccount(site.ID, 0, SiteAccountInput{Username: "adminAcc", Password: "pw"}); err != nil {
		t.Fatalf("saveSiteAccount: %v", err)
	}
	pm.mu.Lock()
	pm.proxies[site.ID] = &ProxyInstance{
		Site:          *site,
		fetchSiteIcon: newSiteIconFetchFunc(http.DefaultTransport, upURL, UAHeaderPolicy{}, upstreamHeaderPolicy{}),
		shareLogin: func(ctx context.Context, u, p, d, dn, cn string) (string, string, int, error) {
			return "tmp-tok", "admin-uid", http.StatusOK, nil
		},
		shareLogout: func(ctx context.Context, tok string) error { return nil },
	}
	pm.mu.Unlock()

	app := &App{db: db, dbPath: dbPath, pm: pm}
	fetchRec := httptest.NewRecorder()
	app.handleSiteIcon(fetchRec, httptest.NewRequest(http.MethodPost, "/api/sites/"+strconv.FormatInt(site.ID, 10)+"/icon/fetch", nil), site.ID, "fetch")
	if fetchRec.Code != http.StatusOK {
		t.Fatalf("manual fetch status=%d body=%s", fetchRec.Code, fetchRec.Body.String())
	}
	var fetchRes struct {
		OK              bool   `json:"ok"`
		RequiresConfirm bool   `json:"requires_confirm"`
		FetchedKind     string `json:"fetched_kind"`
		PreviewDataURL  string `json:"preview_data_url"`
	}
	if err := json.Unmarshal(fetchRec.Body.Bytes(), &fetchRes); err != nil {
		t.Fatalf("decode fetch response: %v", err)
	}
	if !fetchRes.RequiresConfirm || fetchRes.FetchedKind != "account_avatar" || !strings.HasPrefix(fetchRes.PreviewDataURL, "data:image/png;base64,") {
		t.Fatalf("expected preview confirmation for account avatar, got %+v", fetchRes)
	}
	if db.hasAutoSiteIcon(site.ID) {
		t.Fatalf("account avatar must NOT be saved before admin confirms preview")
	}
}

// ---------------------------------------------------------------------------
// #8A: Atomic concurrent stream admission (1-stream cap under simultaneous start)

func TestV180AtomicConcurrentStreamAdmission(t *testing.T) {
	app := newTestApp(t)
	db := app.db
	rt := app.pm.shares

	site, _ := db.CreateSiteRecord(Site{Name: "ConcSite", IngressMode: ingressModePath, PathPrefix: "/c", TargetURL: "http://127.0.0.1:8096", Enabled: true})
	user := createDanmakuTestShareUser(t, app, site.ID, "conc_u", "", true, 0, nil)
	if _, err := db.db.Exec(`UPDATE share_users SET max_concurrent_streams=1 WHERE id=?`, user.ID); err != nil {
		t.Fatalf("set max_concurrent_streams: %v", err)
	}
	if err := rt.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	hash1 := shareTokenHash("dev1-tok")
	hash2 := shareTokenHash("dev2-tok")
	rt.mu.Lock()
	rt.sessions[hash1] = shareSessionEntry{userID: user.ID, siteID: site.ID, deviceID: "device-1"}
	rt.sessions[hash2] = shareSessionEntry{userID: user.ID, siteID: site.ID, deviceID: "device-2"}
	rt.mu.Unlock()

	// Synchronize 2 simultaneous stream starts from 2 different devices.
	startGate := make(chan struct{})
	var admitted atomic.Int32
	var rejected atomic.Int32
	var wg sync.WaitGroup
	finishes := make([]func(), 2)
	hashes := []string{hash1, hash2}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/Videos/123/stream.mkv", nil)
			<-startGate
			fin, blocked, _ := rt.acquireRequestActivity(user.ID, hashes[idx], req, time.Now(), true)
			if blocked {
				rejected.Add(1)
			} else {
				admitted.Add(1)
				finishes[idx] = fin
			}
		}(i)
	}
	close(startGate)
	wg.Wait()

	if admitted.Load() != 1 || rejected.Load() != 1 {
		t.Fatalf("expected exactly 1 admitted and 1 rejected under 1-stream cap, got admitted=%d rejected=%d", admitted.Load(), rejected.Load())
	}

	// Same device parallel Range request MUST still be allowed (counts as 1 stream).
	admittedIdx := 0
	if finishes[1] != nil {
		admittedIdx = 1
	}
	rangeReq := httptest.NewRequest(http.MethodGet, "/Videos/123/stream.mkv", nil)
	rangeReq.Header.Set("Range", "bytes=100-200")
	finRange, blockedRange, _ := rt.acquireRequestActivity(user.ID, hashes[admittedIdx], rangeReq, time.Now(), true)
	if blockedRange {
		t.Fatalf("parallel Range request on the same active device must not be blocked")
	}
	finRange()
	finishes[admittedIdx]()
}

// ---------------------------------------------------------------------------
// #8B: Persistent idempotent edge traffic metering + mixed-version negotiation

func TestV180EdgeTrafficIdempotencyAcrossRestartAndMixedVersion(t *testing.T) {
	app := newTestApp(t)
	db := app.db
	pm := app.pm
	edgeRT := newEdgeNodeRuntime(db, pm, nil)
	pm.SetEdgeRuntime(edgeRT)

	site, _ := db.CreateSiteRecord(Site{Name: "MeterSite", IngressMode: ingressModePath, PathPrefix: "/m", TargetURL: "http://127.0.0.1:8096", Enabled: true})
	user := createDanmakuTestShareUser(t, app, site.ID, "meter_u", "", true, 0, nil)
	_ = pm.shares.reload()

	recNode := httptest.NewRecorder()
	app.handleEdgeNodes(recNode, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"edge-m1","public_port":19090,"public_host":"m1.example.com","tls_mode":"http"}`)))
	if recNode.Code != http.StatusCreated {
		t.Fatalf("createEdgeNode status=%d body=%s", recNode.Code, recNode.Body.String())
	}
	var nodeRec EdgeNode
	_ = json.Unmarshal(recNode.Body.Bytes(), &nodeRec)
	edgeRT.mu.RLock()
	token := edgeRT.nodes[nodeRec.ID].tokenPlain
	edgeRT.mu.RUnlock()

	// Simulate master committing traffic batch -> dropping response -> RESTARTING master -> agent retrying!
	var activeApp atomic.Pointer[App]
	activeApp.Store(app)
	var dropNextReply atomic.Bool

	masterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/heartbeat" {
			http.NotFound(w, r)
			return
		}
		if dropNextReply.Swap(false) {
			rec := httptest.NewRecorder()
			activeApp.Load().handleAgentHeartbeat(rec, r)
			// Master committed traffic, but network drops reply before agent sees it!
			hj, ok := w.(http.Hijacker)
			if ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		activeApp.Load().handleAgentHeartbeat(w, r)
	}))
	defer masterSrv.Close()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: masterSrv.URL,
		NodeID:    nodeRec.ID,
		Token:     token,
		Port:      19090,
	})

	// 1. Initial heartbeat learns master supports traffic_batch_id.
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("initial heartbeat failed: %v", err)
	}
	if !agent.masterSupportsTrafficBatch.Load() {
		t.Fatalf("expected agent to detect masterSupportsTrafficBatch=true")
	}

	// 2. Record 1000 bytes out on agent, drop master's reply after master commits!
	agent.recordTraffic(site.ID, user.ID, "tokhash", 0, 1000, 1, true)
	dropNextReply.Store(true)
	if err := agent.sendHeartbeat(context.Background()); err == nil {
		t.Fatalf("expected error when reply was dropped")
	}

	// 3. Simulate full MASTER RESTART on the same SQLite DB (empty in-memory batch cache!).
	pm2 := NewProxyManager(db, bytes.Repeat([]byte{0x65}, 32))
	edgeRT2 := newEdgeNodeRuntime(db, pm2, nil)
	pm2.SetEdgeRuntime(edgeRT2)
	_ = pm2.shares.reload()
	app2 := &App{db: db, dbPath: app.dbPath, pm: pm2}
	activeApp.Store(app2)

	// 4. Agent retries with the EXACT SAME traffic_batch_id -> must be deduplicated via SQLite!
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("retry heartbeat after master restart failed: %v", err)
	}

	// Verify site, share user, and edge node were each credited EXACTLY ONCE (1000 bytes).
	var siteUsed, userUsed, nodeUsed int64
	_ = db.db.QueryRow(`SELECT traffic_used FROM sites WHERE id=?`, site.ID).Scan(&siteUsed)
	_ = db.db.QueryRow(`SELECT traffic_used FROM share_users WHERE id=?`, user.ID).Scan(&userUsed)
	_ = db.db.QueryRow(`SELECT traffic_used FROM edge_nodes WHERE id=?`, nodeRec.ID).Scan(&nodeUsed)
	if siteUsed != 1000 || userUsed != 1000 || nodeUsed != 1000 {
		t.Fatalf("expected exactly 1000 bytes everywhere after retry+restart, got site=%d user=%d node=%d", siteUsed, userUsed, nodeUsed)
	}

	// 5. Mixed-version test: new v1.8.0 agent talking to strict v1.7.0 master (DisallowUnknownFields).
	var legacyHits atomic.Int32
	legacyMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyHits.Add(1)
		// Legacy v1.7.0 struct without TrafficBatchID + DisallowUnknownFields().
		var legacyReq struct {
			Version           string                         `json:"version"`
			ListenPort        int                            `json:"listen_port"`
			TLSActive         bool                           `json:"tls_active"`
			ActiveStreams     int                            `json:"active_streams"`
			CurrentBps        int64                          `json:"current_bps"`
			MemoryBytes       uint64                         `json:"memory_bytes"`
			UptimeSec         int64                          `json:"uptime_sec"`
			SupportsSpeedtest bool                           `json:"supports_speedtest,omitempty"`
			UpstreamLatencies map[int64]int                  `json:"upstream_latencies,omitempty"`
			ISPProbeSeq       uint64                         `json:"isp_probe_seq,omitempty"`
			ISPProbeResults   map[string]agentISPProbeResult `json:"isp_probe_results,omitempty"`
			SpeedtestProgress *agentSpeedtestProgress        `json:"speedtest_progress,omitempty"`
			UpgradeRequestID  string                         `json:"upgrade_request_id,omitempty"`
			UpgradeStatus     string                         `json:"upgrade_status,omitempty"`
			CertFingerprint   string                         `json:"cert_fingerprint"`
			TrafficReports    []agentTrafficReport           `json:"traffic_reports"`
			IPMismatchEvents  []agentIPMismatchEvent         `json:"ip_mismatch_events,omitempty"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&legacyReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"node_enabled":true,"blocked_users":[],"revoked_hashes":[],"user_speed_limits":{}}`))
	}))
	defer legacyMaster.Close()

	agentLegacy := newEdgeAgent(edgeAgentConfig{
		MasterURL: legacyMaster.URL,
		NodeID:    nodeRec.ID,
		Token:     token,
		Port:      19090,
	})
	agentLegacy.recordTraffic(site.ID, user.ID, "tokhash", 0, 500, 1, false)
	if err := agentLegacy.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("new agent heartbeat against strict v1.7.0 master failed: %v", err)
	}
	// Simulate downgrade from v1.8.0 master -> v1.7.0 master: first heartbeat gets 400 and disables flag, retry succeeds!
	agentLegacy.masterSupportsTrafficBatch.Store(true)
	agentLegacy.recordTraffic(site.ID, user.ID, "tokhash", 0, 250, 1, false)
	_ = agentLegacy.sendHeartbeat(context.Background()) // sees 400, flips masterSupportsTrafficBatch=false
	if err := agentLegacy.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("agent should recover automatically after master downgrade to v1.7.0: %v", err)
	}
}
