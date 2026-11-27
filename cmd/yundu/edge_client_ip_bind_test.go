package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEdgePlaybackClientIPBindingAndMismatchAlert(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "edge-bind-ip.db")
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
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|0|广东省|深圳市|电信"},
		{start: netip.MustParseAddr("45.6.7.0"), end: netip.MustParseAddr("45.6.7.255"), region: "中国|0|香港|0|Cloudflare"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x61}, 32))
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)
	defer pm.GracefulShutdown(context.Background())

	app := &App{
		db:        db,
		dbPath:    dbPath,
		pm:        pm,
		ip2region: ipMgr,
		panelHost: "emby.example.com",
	}

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("VIDEO-DATA"))
	}))
	defer upstream.Close()

	site, err := db.CreateSiteRecord(Site{
		Name:        "MainEmby",
		IngressMode: ingressModePath,
		PathPrefix:  "/main",
		TargetURL:   upstream.URL,
		Enabled:     true,
		UAMode:      "passthrough",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}

	// Create share user
	user := createDanmakuTestShareUser(t, app, site.ID, "alice", "", true, 0, []int64{})

	// Enable block_non_mainland (edge_bind_client_ip defaults to true)
	putRec := httptest.NewRecorder()
	app.handleISPRoutes(putRec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(`{"block_non_mainland":true}`)))
	if putRec.Code != http.StatusOK {
		t.Fatalf("enable block_non_mainland status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	var routesPayload struct {
		BlockNonMainland bool `json:"block_non_mainland"`
		EdgeBindClientIP bool `json:"edge_bind_client_ip"`
	}
	_ = json.Unmarshal(putRec.Body.Bytes(), &routesPayload)
	if !routesPayload.BlockNonMainland || !routesPayload.EdgeBindClientIP {
		t.Fatalf("expected block_non_mainland=true and edge_bind_client_ip=true, got %+v", routesPayload)
	}

	// Create Edge Node and local edgeAgent
	masterMux := http.NewServeMux()
	masterMux.HandleFunc("/api/agent/heartbeat", app.handleAgentHeartbeat)
	masterSrv := httptest.NewServer(masterMux)
	defer masterSrv.Close()

	nodeRec := httptest.NewRecorder()
	app.handleEdgeNodes(nodeRec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"RFC-JPCO","public_port":19090}`)))
	if nodeRec.Code != http.StatusCreated {
		t.Fatalf("create node status=%d body=%s", nodeRec.Code, nodeRec.Body.String())
	}
	var node EdgeNode
	_ = json.Unmarshal(nodeRec.Body.Bytes(), &node)

	edgeRT.mu.RLock()
	nodeToken := edgeRT.nodes[node.ID].tokenPlain
	edgeRT.mu.RUnlock()

	var simulatedPeerIP string
	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: masterSrv.URL,
		NodeID:    node.ID,
		Token:     nodeToken,
		BindAddr:  "127.0.0.1",
	})
	agent.peerIPFunc = func(_ *http.Request) string { return simulatedPeerIP }
	agent.nodeEnabled.Store(true)

	// Issue a playback ticket bound to Mainland IP 1.2.3.4
	sealed, err := sealEdgePlaybackTicket(nodeToken, edgePlaybackTicket{
		NodeID:      node.ID,
		SiteID:      site.ID,
		ShareUserID: user.ID,
		ClientIP:    normalizeEdgeBindIP("1.2.3.4"),
		TargetURL:   upstream.URL + "/Videos/1/stream.mp4",
		ExpiresAt:   time.Now().Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("seal ticket: %v", err)
	}
	playPath := edgeTicketPathPrefix + sealed + "/stream.mp4"

	// 1. Matching client IP (IPv4-mapped IPv6 form) -> 200 OK
	simulatedPeerIP = "::ffff:1.2.3.4"
	okRec := httptest.NewRecorder()
	agent.handler().ServeHTTP(okRec, httptest.NewRequest(http.MethodGet, playPath, nil))
	if okRec.Code != http.StatusOK || okRec.Body.String() != "VIDEO-DATA" {
		t.Fatalf("expected 200 VIDEO-DATA for matching IP, got %d body=%s", okRec.Code, okRec.Body.String())
	}

	// 2. Mismatched proxy IP (45.6.7.8) -> 403 Forbidden + zero upstream hits
	hitsBefore := upstreamHits.Load()
	simulatedPeerIP = "45.6.7.8"
	badRec := httptest.NewRecorder()
	agent.handler().ServeHTTP(badRec, httptest.NewRequest(http.MethodGet, playPath, nil))
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for mismatched proxy IP, got %d body=%s", badRec.Code, badRec.Body.String())
	}
	if upstreamHits.Load() != hitsBefore {
		t.Fatalf("mismatched IP stream leaked to upstream: before=%d after=%d", hitsBefore, upstreamHits.Load())
	}

	// 3. Agent reports mismatch to Master via heartbeat -> /api/me surfaces edge_ip_alert
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("agent sendHeartbeat: %v", err)
	}
	storedUser, err := db.getShareUser(user.ID)
	if err != nil {
		t.Fatalf("getShareUser: %v", err)
	}
	meReq := httptest.NewRequest(http.MethodGet, "https://emby.example.com/api/me", nil)
	meReq.Host = "emby.example.com"
	meView := app.buildShareSelfView(storedUser, meReq)
	if meView.EdgeIPAlert == nil {
		t.Fatalf("expected edge_ip_alert in /api/me after mismatch heartbeat")
	}
	if meView.EdgeIPAlert.NodeName != "RFC-JPCO" ||
		meView.EdgeIPAlert.ExpectedIP != "1.2.3.4" ||
		meView.EdgeIPAlert.ActualIP != "45.6.7.8" ||
		!strings.Contains(meView.EdgeIPAlert.ActualRegion, "香港") ||
		meView.EdgeIPAlert.DirectRule != "emby.example.com" {
		t.Fatalf("unexpected edge_ip_alert payload: %+v", meView.EdgeIPAlert)
	}

	// 4. Disabling edge_bind_client_ip turns off binding
	offRec := httptest.NewRecorder()
	app.handleISPRoutes(offRec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(`{"edge_bind_client_ip":false}`)))
	if offRec.Code != http.StatusOK {
		t.Fatalf("disable edge_bind_client_ip status=%d body=%s", offRec.Code, offRec.Body.String())
	}
	if edgeRT.shouldBindEdgeClientIP() {
		t.Fatalf("expected shouldBindEdgeClientIP() to be false after disabling")
	}
}
