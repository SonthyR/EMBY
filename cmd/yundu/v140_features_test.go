package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setupEdgeRoutingTestApp(t *testing.T) (*App, *edgeNodeRuntime) {
	t.Helper()
	app := newTestApp(t)

	ipDir := filepath.Join(t.TempDir(), "ip2region")
	if err := os.MkdirAll(ipDir, 0o700); err != nil {
		t.Fatalf("mkdir ip2region: %v", err)
	}
	v4Raw := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|0|广东省|深圳市|电信"},
		{start: netip.MustParseAddr("5.6.7.0"), end: netip.MustParseAddr("5.6.7.255"), region: "中国|0|北京市|北京市|联通"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	t.Cleanup(func() { ipMgr.Close() })

	rt := newEdgeNodeRuntime(app.db, app.pm, ipMgr)
	app.pm.SetEdgeRuntime(rt)
	app.ip2region = ipMgr
	app.speedtest = newSpeedtestManager(app.db, app.pm)
	return app, rt
}

func insertTestEdgeNode(t *testing.T, rt *edgeNodeRuntime, name, ip string, port int) (int64, string) {
	t.Helper()
	token := "tok-" + name + "-" + time.Now().Format("150405.000")
	cipher, err := encryptSiteAccountSecret(token)
	if err != nil {
		t.Fatalf("encrypt edge token: %v", err)
	}
	res, err := rt.db.db.Exec(
		`INSERT INTO edge_nodes(name, public_port, listen_port, public_ip, tls_mode, token_hash, token_ciphertext, enabled) VALUES(?,?,?,?,?,?,?,1)`,
		name, port, port, ip, edgeNodeTLSModeHTTP, edgeNodeTokenHash(token), cipher,
	)
	if err != nil {
		t.Fatalf("insert edge node: %v", err)
	}
	nodeID, _ := res.LastInsertId()
	if err := rt.reload(); err != nil {
		t.Fatalf("reload edge runtime: %v", err)
	}
	rt.mu.Lock()
	if state := rt.nodes[nodeID]; state != nil {
		state.lastSeen = time.Now()
	}
	rt.mu.Unlock()
	return nodeID, token
}

func TestISPRoutingAutoAndBalanceStrategies(t *testing.T) {
	originalSecret := jwtSecret
	jwtSecret = []byte("v140-test-secret-key-000000000000000")
	t.Cleanup(func() { jwtSecret = originalSecret })

	app, rt := setupEdgeRoutingTestApp(t)
	edgeA, _ := insertTestEdgeNode(t, rt, "HK-CT-Fast", "203.0.113.10", 19090)
	edgeB, _ := insertTestEdgeNode(t, rt, "JP-CT-Clean", "203.0.113.20", 19090)

	// Persist explicit priority order where Master (0) is #1 so we prove auto/balance never select Master while edges are healthy.
	body := `{"group":"ct","strategy":"auto","nodes":[{"node_id":0,"enabled":true},{"node_id":1,"enabled":true},{"node_id":2,"enabled":true}]}`
	req := httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.handleISPRoutes(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/isp-routes status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Insert measured speed metrics: edgeA has 800 Mbps, 0 retrans; edgeB has 400 Mbps, 10 retrans.
	for _, item := range []struct {
		id      int64
		bps     int64
		retrans int
	}{
		{id: edgeA, bps: 100_000_000, retrans: 0},
		{id: edgeB, bps: 50_000_000, retrans: 10},
	} {
		if _, err := app.db.db.Exec(
			`INSERT INTO edge_node_speed_metrics(node_id, isp_group, avg_return_bps, avg_outbound_bps, avg_return_rtt_ms, avg_outbound_rtt_ms, total_retrans, sample_count, tested_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			item.id, "ct", item.bps, item.bps/2, 45, 50, item.retrans, 3, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert speed metric: %v", err)
		}
	}
	rt.reloadSpeedMetrics()

	// 1. Auto mode picks highest score (edgeA), ignoring Master at priority #1.
	dec := rt.selectNodeForIP("1.2.3.4")
	if dec.ISPGroup != "ct" || dec.NodeID != edgeA {
		t.Fatalf("auto pick = %+v, want NodeID=%d", dec, edgeA)
	}

	// 2. When edgeA gets heavily loaded (activeStreams=20 -> 800/(1+1.6)=307 < 384), auto shifts to edgeB.
	rt.mu.Lock()
	rt.nodes[edgeA].activeStreams = 20
	rt.mu.Unlock()
	dec = rt.selectNodeForIP("1.2.3.4")
	if dec.NodeID != edgeB {
		t.Fatalf("auto under load picked NodeID=%d, want %d", dec.NodeID, edgeB)
	}

	// 3. When all enabled edge nodes go offline/disabled, auto falls back to Master (0).
	rt.mu.Lock()
	rt.nodes[edgeA].activeStreams = 0
	rt.nodes[edgeA].lastSeen = time.Now().Add(-2 * time.Minute)
	rt.nodes[edgeB].enabled = false
	rt.mu.Unlock()
	dec = rt.selectNodeForIP("1.2.3.4")
	if dec.NodeID != 0 {
		t.Fatalf("auto all-edge-unavailable fallback = NodeID=%d, want 0 (Master)", dec.NodeID)
	}

	// 4. Switch ct to balance mode via PUT API and restore both edge nodes.
	balReq := httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(`{"group":"ct","strategy":"balance"}`))
	balRec := httptest.NewRecorder()
	app.handleISPRoutes(balRec, balReq)
	if balRec.Code != http.StatusOK {
		t.Fatalf("PUT strategy=balance status=%d", balRec.Code)
	}
	rt.mu.Lock()
	rt.nodes[edgeA].lastSeen = time.Now()
	rt.nodes[edgeA].activeStreams = 0
	rt.nodes[edgeB].enabled = true
	rt.nodes[edgeB].lastSeen = time.Now()
	rt.nodes[edgeB].activeStreams = 0
	// Enable an unrelated group block so isClientBlocked runs selectNodeForIP before selectNodeForDispatch.
	rt.blocked["cm"] = true
	rt.mu.Unlock()

	seen := map[int64]bool{}
	for i := 0; i < 4; i++ {
		if blocked, _ := rt.isClientBlocked("1.2.3.4"); blocked {
			t.Fatalf("unexpected block on ct client")
		}
		d := rt.selectNodeForDispatch("1.2.3.4")
		if d.NodeID == 0 {
			t.Fatalf("balance picked Master while edge nodes were healthy")
		}
		seen[d.NodeID] = true
	}
	if !seen[edgeA] || !seen[edgeB] {
		t.Fatalf("balance should distribute across both edge nodes even with block policy active, got %+v", seen)
	}

	// With unequal activeStreams, balance prefers least activeStreams edge node.
	rt.mu.Lock()
	rt.nodes[edgeA].activeStreams = 6
	rt.nodes[edgeB].activeStreams = 1
	rt.mu.Unlock()
	for i := 0; i < 3; i++ {
		if d := rt.selectNodeForDispatch("1.2.3.4"); d.NodeID != edgeB {
			t.Fatalf("balance with unequal streams picked %d, want %d", d.NodeID, edgeB)
		}
	}

	// Reload from DB confirms strategy survived reload.
	if err := rt.reload(); err != nil {
		t.Fatalf("rt.reload(): %v", err)
	}
	rt.mu.RLock()
	gotStrategy := rt.strategyForGroupLocked("ct")
	rt.mu.RUnlock()
	if gotStrategy != ispStrategyBalance {
		t.Fatalf("reloaded strategy = %q, want %q", gotStrategy, ispStrategyBalance)
	}
}

func TestSpeedtestMasterAndAgentEndToEnd(t *testing.T) {
	app, rt := setupEdgeRoutingTestApp(t)
	app.speedtest.phaseDuration = 200 * time.Millisecond

	// 1. Mock DoH server returning valid public IPv4 93.184.216.34 + mock TLS target server mimicking Apple CDN.
	dohSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Answer":[{"type":1,"data":"93.184.216.34","TTL":60}]}`))
	}))
	defer dohSrv.Close()

	chunk := bytes.Repeat([]byte("SPEEDTEST-PAYLOAD-"), 4096)
	targetSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/gm/slurp" || r.URL.Path == "/up" {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v1/gm/large" || r.URL.Path == "/down" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 8; i++ {
				_, _ = w.Write(chunk)
			}
			return
		}
		http.NotFound(w, r)
	}))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: appleSpeedtestHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{appleSpeedtestHost},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leafCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	targetSrv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        leafCert,
		}},
	}
	targetSrv.StartTLS()
	defer targetSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(leafCert)
	mockTransport := &http.Transport{
		TLSClientConfig:       secureTLSConfig(""),
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", targetSrv.Listener.Addr().String())
		},
	}
	mockTransport.TLSClientConfig.RootCAs = pool

	app.speedtest.dohClient = dohSrv.Client()
	app.speedtest.dohEndpoints = []string{dohSrv.URL}
	app.speedtest.targetTransport = mockTransport

	// 2. Test SSRF rejection (loopback, link-local, benchmarking, doc, site-local IPv6) and custom target CRUD API.
	for _, badURL := range []string{"http://127.0.0.1:8080/up", "http://169.254.169.254/latest", "http://198.18.0.1/up", "http://[fec0::1]/up"} {
		ssrfRec := httptest.NewRecorder()
		app.handleSpeedtestCustomTargets(ssrfRec, httptest.NewRequest(http.MethodPost, "/api/speedtest/custom-targets", strings.NewReader(`{"name":"BadSSRF","isp_group":"ct","upload_url":"`+badURL+`"}`)))
		if ssrfRec.Code != http.StatusBadRequest {
			t.Fatalf("SSRF custom URL %s status=%d, want 400", badURL, ssrfRec.Code)
		}
	}
	createRec := httptest.NewRecorder()
	app.handleSpeedtestCustomTargets(createRec, httptest.NewRequest(http.MethodPost, "/api/speedtest/custom-targets", strings.NewReader(`{"name":"LocalCustom","isp_group":"cu","province":"测试","upload_url":"https://mensura.cdn-apple.com/up","download_url":"https://mensura.cdn-apple.com/down","method":"POST"}`)))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create custom target status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var createResp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil || createResp.ID <= 0 {
		t.Fatalf("unexpected custom target response: %s", createRec.Body.String())
	}

	// 3. Start Master builtin+custom job (with duplicate ISPs) and verify deduplication & completion.
	startRec := httptest.NewRecorder()
	app.handleSpeedtestStart(startRec, httptest.NewRequest(http.MethodPost, "/api/speedtest/start", strings.NewReader(`{"node_id":0,"mode":"all","provinces":["bj","bj"],"isps":["ct","ct","ct"]}`)))
	if startRec.Code != http.StatusCreated {
		t.Fatalf("start master job status=%d body=%s", startRec.Code, startRec.Body.String())
	}
	var startResp struct {
		Job speedtestJobView `json:"job"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("decode start resp: %v", err)
	}
	if startResp.Job.TotalTargets != 2 {
		t.Fatalf("master job total_targets=%d, want 2 (1 builtin + 1 custom)", startResp.Job.TotalTargets)
	}

	var masterDone speedtestJobView
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		jobRec := httptest.NewRecorder()
		app.handleSpeedtestJobByID(jobRec, httptest.NewRequest(http.MethodGet, "/api/speedtest/jobs/"+startResp.Job.ID, nil))
		var jr struct {
			Job speedtestJobView `json:"job"`
		}
		if err := json.Unmarshal(jobRec.Body.Bytes(), &jr); err == nil {
			masterDone = jr.Job
			if masterDone.Status == "done" || masterDone.Status == "failed" {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	if masterDone.Status != "done" || masterDone.CompletedTargets != 2 {
		t.Fatalf("master job did not complete cleanly: %+v", masterDone)
	}
	for _, r := range masterDone.Results {
		if r.Status != "done" || r.ReturnBps <= 0 || r.OutboundBps <= 0 || r.ReturnRTTMs <= 0 {
			t.Fatalf("unexpected master result row: %+v", r)
		}
	}
	if metrics := rt.nodeSpeedMetricsCopy(0); len(metrics) != 2 {
		t.Fatalf("master persisted metrics = %+v, want ct and cu", metrics)
	}

	// Delete custom target cleanly.
	delRec := httptest.NewRecorder()
	app.handleSpeedtestCustomTargetByID(delRec, httptest.NewRequest(http.MethodDelete, "/api/speedtest/custom-targets/"+itoa64(createResp.ID), nil))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete custom target status=%d body=%s", delRec.Code, delRec.Body.String())
	}

	// 4. Test Edge Agent heartbeat speedtest task dispatch + execution + metrics persistence + direct stream tickets.
	edgeID, edgeToken := insertTestEdgeNode(t, rt, "EdgeST", "203.0.113.30", 19090)

	masterMux := http.NewServeMux()
	masterMux.HandleFunc("/api/agent/heartbeat", app.handleAgentHeartbeat)
	masterSrv := httptest.NewServer(masterMux)
	defer masterSrv.Close()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: masterSrv.URL,
		NodeID:    edgeID,
		Token:     edgeToken,
		Port:      19090,
	})
	agent.speedtestTransport = mockTransport

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.heartbeatLoop(ctx)

	edgeStartRec := httptest.NewRecorder()
	app.handleSpeedtestStart(edgeStartRec, httptest.NewRequest(http.MethodPost, "/api/speedtest/start", strings.NewReader(`{"node_id":`+itoa64(edgeID)+`,"mode":"builtin","provinces":["gd"],"isps":["cm"]}`)))
	if edgeStartRec.Code != http.StatusCreated {
		t.Fatalf("start edge job status=%d body=%s", edgeStartRec.Code, edgeStartRec.Body.String())
	}
	var edgeStartResp struct {
		Job speedtestJobView `json:"job"`
	}
	_ = json.Unmarshal(edgeStartRec.Body.Bytes(), &edgeStartResp)
	// Kick a heartbeat so agent receives task immediately without waiting 10s interval.
	if err := agent.sendHeartbeat(ctx); err != nil {
		t.Fatalf("initial agent heartbeat: %v", err)
	}

	var edgeDone speedtestJobView
	edgeDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(edgeDeadline) {
		jobRec := httptest.NewRecorder()
		app.handleSpeedtestJobByID(jobRec, httptest.NewRequest(http.MethodGet, "/api/speedtest/jobs/"+edgeStartResp.Job.ID, nil))
		var jr struct {
			Job speedtestJobView `json:"job"`
		}
		if err := json.Unmarshal(jobRec.Body.Bytes(), &jr); err == nil {
			edgeDone = jr.Job
			if edgeDone.Status == "done" || edgeDone.Status == "failed" {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	if edgeDone.Status != "done" || edgeDone.CompletedTargets != 1 {
		t.Fatalf("edge speedtest job failed: %+v", edgeDone)
	}
	if edgeMetrics := rt.nodeSpeedMetricsCopy(edgeID); len(edgeMetrics) != 1 || edgeMetrics[0].ISPGroup != "cm" {
		t.Fatalf("edge persisted metrics = %+v, want 1 cm row", edgeMetrics)
	}

	// 5. Verify direct-stream auth on Master and sealed ticket domain separation on Edge Agent.
	unauthStream := httptest.NewRecorder()
	app.authMiddleware(app.handleSpeedtestDirectStream)(unauthStream, httptest.NewRequest(http.MethodGet, "/api/speedtest/direct-stream", nil))
	if unauthStream.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated master direct-stream status=%d, want 401", unauthStream.Code)
	}

	sealed, err := sealEdgeSpeedtestTicket(edgeToken, edgeID, 4<<20)
	if err != nil {
		t.Fatalf("sealEdgeSpeedtestTicket: %v", err)
	}
	edgeHandler := agent.handler()

	okDirectRec := httptest.NewRecorder()
	edgeHandler.ServeHTTP(okDirectRec, httptest.NewRequest(http.MethodGet, edgeSpeedtestPathPrefix+sealed+"?bytes=1048576", nil))
	if okDirectRec.Code != http.StatusOK || okDirectRec.Body.Len() != 1048576 {
		t.Fatalf("edge direct stream status=%d len=%d, want 200 and 1048576", okDirectRec.Code, okDirectRec.Body.Len())
	}
	if okDirectRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing CORS header on edge direct stream")
	}

	// Enforce cumulative JTI byte budget (2x MaxBytes = 8 MiB total; 1 MiB above + 3 MiB + 3 MiB allowed, + 2 MiB rejected).
	for i := 0; i < 2; i++ {
		budgetRec := httptest.NewRecorder()
		edgeHandler.ServeHTTP(budgetRec, httptest.NewRequest(http.MethodGet, edgeSpeedtestPathPrefix+sealed+"?bytes=3145728", nil))
		if budgetRec.Code != http.StatusOK {
			t.Fatalf("budget request #%d status=%d, want 200", i+1, budgetRec.Code)
		}
	}
	overBudgetRec := httptest.NewRecorder()
	edgeHandler.ServeHTTP(overBudgetRec, httptest.NewRequest(http.MethodGet, edgeSpeedtestPathPrefix+sealed+"?bytes=2097152", nil))
	if overBudgetRec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget direct stream status=%d, want 429", overBudgetRec.Code)
	}

	// Verify dynamic CSP header allows edge node origins for cross-origin browser direct tests.
	cspRec := httptest.NewRecorder()
	securityHeadersWithConnectSources(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), nil, rt.publicOrigins).ServeHTTP(cspRec, httptest.NewRequest(http.MethodGet, "https://panel.example/", nil))
	cspHeader := cspRec.Header().Get("Content-Security-Policy")
	if !strings.Contains(cspHeader, "connect-src 'self' http://203.0.113.30:19090") {
		t.Fatalf("CSP missing edge origin: %q", cspHeader)
	}

	// Wrong endpoint domain: using playback ticket on /_yundu/st/ and speedtest ticket on /_yundu/p/ must fail.
	playTicket, _ := sealEdgePlaybackTicket(edgeToken, edgePlaybackTicket{NodeID: edgeID, SiteID: 1, TargetURL: targetSrv.URL, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	crossRec1 := httptest.NewRecorder()
	edgeHandler.ServeHTTP(crossRec1, httptest.NewRequest(http.MethodGet, edgeSpeedtestPathPrefix+playTicket, nil))
	if crossRec1.Code != http.StatusUnauthorized {
		t.Fatalf("playback ticket accepted on speedtest route! status=%d", crossRec1.Code)
	}
	crossRec2 := httptest.NewRecorder()
	edgeHandler.ServeHTTP(crossRec2, httptest.NewRequest(http.MethodGet, edgeTicketPathPrefix+sealed, nil))
	if crossRec2.Code != http.StatusUnauthorized {
		t.Fatalf("speedtest ticket accepted on playback route! status=%d", crossRec2.Code)
	}
}

func TestDashboardAnalyticsEndpoint(t *testing.T) {
	app, rt := setupEdgeRoutingTestApp(t)

	site, err := app.db.CreateSite("MainEmby", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	// Persist 3000 inbound + 7000 outbound (= 10000 bidirectional) on site + live traffic log.
	if err := app.db.addTrafficWithRequestsAt(site.ID, 3000, 7000, 12, time.Now().In(time.Local)); err != nil {
		t.Fatalf("addTrafficWithRequestsAt: %v", err)
	}
	if _, err := app.db.db.Exec(
		`UPDATE sites SET traffic_used=10000, traffic_used_in=3000, traffic_used_out=7000 WHERE id=?`,
		site.ID,
	); err != nil {
		t.Fatalf("update site traffic: %v", err)
	}

	// Attach live ProxyInstance with lifetime reqCount=50 but pendingRequests=0 to verify requests are not double-counted.
	inst := &ProxyInstance{Site: *site}
	inst.Site.TrafficUsedIn = 3000
	inst.Site.TrafficUsedOut = 7000
	inst.Site.TrafficUsed = 10000
	inst.persistedBytesIn.Store(3000)
	inst.persistedBytesOut.Store(7000)
	inst.persistedTraffic.Store(10000)
	inst.reqCount.Store(50)
	app.pm.proxies[site.ID] = inst

	edgeID, _ := insertTestEdgeNode(t, rt, "SG-Edge", "203.0.113.50", 19090)
	rt.mu.Lock()
	rt.nodes[edgeID].persistedUsed = 0 // Simulate monthly quota reset while monotonic totals remain 1000 + 3000 = 4000.
	rt.nodes[edgeID].persistedTotalIn = 1000
	rt.nodes[edgeID].persistedTotalOut = 3000
	rt.nodes[edgeID].activeStreams = 3
	rt.nodes[edgeID].currentBps = 25_000_000
	rt.mu.Unlock()

	if _, err := app.db.db.Exec(
		`INSERT INTO share_users(username, password_hash, enabled, traffic_used) VALUES('alice', 'h1', 1, 7500), ('bob', 'h2', 1, 2500)`,
	); err != nil {
		t.Fatalf("insert share users: %v", err)
	}

	rec := httptest.NewRecorder()
	app.handleDashboardAnalytics(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard-analytics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/dashboard-analytics status=%d body=%s", rec.Code, rec.Body.String())
	}

	var got dashboardAnalyticsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode analytics response: %v", err)
	}
	if got.Summary.TotalTraffic != 10000 || got.Summary.MasterTraffic != 6000 || got.Summary.EdgeTraffic != 4000 || got.Summary.OffloadPercent != 40.0 {
		t.Fatalf("unexpected analytics summary: %+v", got.Summary)
	}
	if len(got.NodeBreakdown) != 2 || got.NodeBreakdown[0].SharePercent != 60.0 || got.NodeBreakdown[1].SharePercent != 40.0 {
		t.Fatalf("unexpected node breakdown: %+v", got.NodeBreakdown)
	}
	if len(got.TopShareUsers) != 2 || got.TopShareUsers[0].Username != "alice" || got.TopShareUsers[1].Username != "bob" {
		t.Fatalf("unexpected share user ranking: %+v", got.TopShareUsers)
	}
	if len(got.SiteBreakdown) != 1 || got.SiteBreakdown[0].SharePercent != 100.0 {
		t.Fatalf("unexpected site breakdown: %+v", got.SiteBreakdown)
	}
	if len(got.DailyActivity) != 14 || got.DailyActivity[13].TrafficBytes != 10000 || got.DailyActivity[13].Requests != 12 {
		t.Fatalf("unexpected daily activity tail: %+v", got.DailyActivity[len(got.DailyActivity)-1])
	}
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
