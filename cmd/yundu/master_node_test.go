package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func newMasterNodeTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "master_node.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}

	ipDir := filepath.Join(t.TempDir(), "ip2region")
	if err := os.MkdirAll(ipDir, 0o700); err != nil {
		t.Fatalf("mkdir ip2region: %v", err)
	}
	v4Raw := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|0|广东省|深圳市|电信"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x55}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)

	app := &App{
		db:             db,
		dbPath:         dbPath,
		pm:             pm,
		ip2region:      ipMgr,
		speedtest:      newSpeedtestManager(db, pm),
		trustedProxies: []*net.IPNet{loopbackCIDR},
	}
	cleanup := func() {
		pm.GracefulShutdown(context.Background())
		ipMgr.Close()
		db.Close()
	}
	return app, cleanup
}

func TestMasterNodeSettingsAndGlobalNameSync(t *testing.T) {
	app, cleanup := newMasterNodeTestApp(t)
	defer cleanup()

	// 1. Default GET /api/master-node returns default Master view
	getReq := httptest.NewRequest(http.MethodGet, "http://panel.example.com:9090/api/master-node", nil)
	getRec := httptest.NewRecorder()
	app.handleMasterNode(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /api/master-node status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var initial EdgeNode
	if err := json.Unmarshal(getRec.Body.Bytes(), &initial); err != nil {
		t.Fatalf("unmarshal initial master: %v", err)
	}
	if !initial.IsMaster || initial.ID != 0 || initial.Name != "主机" || !initial.Online || initial.Status != "online" {
		t.Fatalf("unexpected initial master view: %+v", initial)
	}

	// 2. Reject invalid reset day
	badReq := httptest.NewRequest(http.MethodPut, "/api/master-node", strings.NewReader(`{"name":"美国主机","traffic_reset_day":31}`))
	badRec := httptest.NewRecorder()
	app.handleMasterNode(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid reset day, got %d", badRec.Code)
	}

	// 3. Rename Master and configure quota/reset day/note
	putReq := httptest.NewRequest(http.MethodPut, "http://panel.example.com:9090/api/master-node", strings.NewReader(`{
		"name": "美国主机",
		"traffic_quota": 10737418240,
		"traffic_reset_day": 15,
		"note": "LAX 10Gbps"
	}`))
	putRec := httptest.NewRecorder()
	app.handleMasterNode(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/master-node status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	var updated EdgeNode
	if err := json.Unmarshal(putRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal updated master: %v", err)
	}
	if updated.Name != "美国主机" || updated.TrafficQuota != 10737418240 || updated.ResetDay != 15 || updated.Note != "LAX 10Gbps" {
		t.Fatalf("unexpected updated master: %+v", updated)
	}

	// 4. Verify global name sync across routing, ISP routes, dashboard analytics, speedtest, and share self network
	if d := app.pm.edges.selectNodeForIP("1.2.3.4"); d.NodeID != 0 || d.NodeName != "美国主机" {
		t.Fatalf("selectNodeForIP expected 美国主机, got %+v", d)
	}

	routes := app.buildISPRoutesPayload()
	groups, _ := routes["groups"].([]ispRouteGroupView)
	foundMasterRoute := false
	for _, g := range groups {
		for _, n := range g.Nodes {
			if n.IsMaster {
				foundMasterRoute = true
				if n.Name != "美国主机" {
					t.Fatalf("ISP route group %s master name = %q, want 美国主机", g.Group, n.Name)
				}
			}
		}
	}
	if !foundMasterRoute {
		t.Fatalf("expected master node in ISP routes payload")
	}

	dashReq := httptest.NewRequest(http.MethodGet, "/api/dashboard-analytics", nil)
	dashRec := httptest.NewRecorder()
	app.handleDashboardAnalytics(dashRec, dashReq)
	if dashRec.Code != http.StatusOK {
		t.Fatalf("dashboard analytics status=%d body=%s", dashRec.Code, dashRec.Body.String())
	}
	if !strings.Contains(dashRec.Body.String(), `"美国主机"`) {
		t.Fatalf("dashboard analytics missing synced master name: %s", dashRec.Body.String())
	}

	stReq := httptest.NewRequest(http.MethodGet, "/api/speedtest/targets", nil)
	stRec := httptest.NewRecorder()
	app.handleSpeedtestTargets(stRec, stReq)
	if stRec.Code != http.StatusOK || !strings.Contains(stRec.Body.String(), `"美国主机"`) {
		t.Fatalf("speedtest targets missing synced master name (code=%d): %s", stRec.Code, stRec.Body.String())
	}

	dirReq := httptest.NewRequest(http.MethodGet, "/api/speedtest/direct-targets", nil)
	dirRec := httptest.NewRecorder()
	app.handleSpeedtestDirectTargets(dirRec, dirReq)
	if dirRec.Code != http.StatusOK || !strings.Contains(dirRec.Body.String(), `"美国主机"`) {
		t.Fatalf("speedtest direct targets missing synced master name (code=%d): %s", dirRec.Code, dirRec.Body.String())
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReq.RemoteAddr = "127.0.0.1:12345"
	meReq.Header.Set("X-Real-IP", "1.2.3.4")
	if netView := app.buildShareSelfNetwork(meReq); netView == nil || netView.NodeName != "美国主机" {
		t.Fatalf("share self network expected 美国主机, got %+v", netView)
	}
}

func TestMasterNodeProbeTrafficResetAndQuotaRouting(t *testing.T) {
	app, cleanup := newMasterNodeTestApp(t)
	defer cleanup()

	// 1. Add a real local Emby upstream site and probe Master latency on demand
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer emby.Close()

	site, err := app.db.CreateSiteRecord(Site{
		Name:        "LocalEmby",
		ListenPort:  0,
		IngressMode: ingressModePath,
		PathPrefix:  "/localemby",
		TargetURL:   emby.URL,
		Enabled:     true,
		UAMode:      "passthrough",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}

	probeReq := httptest.NewRequest(http.MethodPost, "/api/master-node/probe", nil)
	probeRec := httptest.NewRecorder()
	app.handleMasterNodeAction(probeRec, probeReq)
	if probeRec.Code != http.StatusOK {
		t.Fatalf("POST /api/master-node/probe status=%d body=%s", probeRec.Code, probeRec.Body.String())
	}
	var probed EdgeNode
	if err := json.Unmarshal(probeRec.Body.Bytes(), &probed); err != nil {
		t.Fatalf("unmarshal probed master: %v", err)
	}
	if len(probed.UpstreamLatencies) != 1 || probed.UpstreamLatencies[0].SiteName != "LocalEmby" || probed.UpstreamLatencies[0].LatencyMs < 1 {
		t.Fatalf("expected valid LocalEmby probe latency >= 1ms, got %+v", probed.UpstreamLatencies)
	}

	// 2. Create an online Edge node and place Master #1, Edge #2 in ct priority routes
	createReq := httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"香港从机","public_port":19090,"public_ip":"203.0.113.10"}`))
	createRec := httptest.NewRecorder()
	app.handleEdgeNodes(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create edge node status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var edge EdgeNode
	if err := json.Unmarshal(createRec.Body.Bytes(), &edge); err != nil {
		t.Fatalf("unmarshal edge: %v", err)
	}

	// Mark edge online and configure ct priority: Master first, Edge second
	app.pm.edges.mu.Lock()
	if st := app.pm.edges.nodes[edge.ID]; st != nil {
		st.lastSeen = time.Now()
		st.supportsEgress = true
	}
	app.pm.edges.mu.Unlock()

	routeBody := `{"group":"ct","nodes":[{"node_id":0,"enabled":true},{"node_id":` + strconv.FormatInt(edge.ID, 10) + `,"enabled":true}]}`
	routeReq := httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(routeBody))
	routeRec := httptest.NewRecorder()
	app.handleISPRoutes(routeRec, routeReq)
	if routeRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/isp-routes status=%d body=%s", routeRec.Code, routeRec.Body.String())
	}

	// Keep edge online after reload()
	app.pm.edges.mu.Lock()
	if st := app.pm.edges.nodes[edge.ID]; st != nil {
		st.lastSeen = time.Now()
		st.supportsEgress = true
	}
	app.pm.edges.mu.Unlock()

	// Set Master quota = 1000 bytes
	putReq := httptest.NewRequest(http.MethodPut, "/api/master-node", strings.NewReader(`{"name":"主机","traffic_quota":1000}`))
	putRec := httptest.NewRecorder()
	app.handleMasterNode(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("set master quota status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	app.pm.edges.mu.Lock()
	if st := app.pm.edges.nodes[edge.ID]; st != nil {
		st.lastSeen = time.Now()
		st.supportsEgress = true
	}
	app.pm.edges.mu.Unlock()

	// Before exceeding quota, Master #1 is chosen
	if d := app.pm.edges.selectNodeForIP("1.2.3.4"); d.NodeID != 0 {
		t.Fatalf("expected Master before quota exceeded, got %+v", d)
	}

	// 3. Hit 85% -> TG 80% alert fires once; still under 100% so Master still chosen
	app.pm.edges.recordMasterTraffic(850, 850)
	alerts := app.pm.edges.collectAlerts(time.Now())
	if len(alerts) != 1 || !strings.Contains(alerts[0], "流量预警") {
		t.Fatalf("expected 1 master 80%% alert, got %v", alerts)
	}
	if again := app.pm.edges.collectAlerts(time.Now()); len(again) != 0 {
		t.Fatalf("expected deduplicated 80%% alert, got %v", again)
	}

	// 4. Cross 100% -> 100% alert fires, status becomes "quota", priority shifts to Edge node
	app.pm.edges.recordMasterTraffic(250, 250)
	app.pm.edges.flush()
	alerts = app.pm.edges.collectAlerts(time.Now())
	if len(alerts) != 1 || !strings.Contains(alerts[0], "流量已耗尽") {
		t.Fatalf("expected 1 master 100%% alert, got %v", alerts)
	}

	if d := app.pm.edges.selectNodeForIP("1.2.3.4"); d.NodeID != edge.ID {
		t.Fatalf("expected soft-deprioritized routing to shift to edge %d, got %+v", edge.ID, d)
	}
	if d, locked := app.pm.edges.selectEgressDecision("1.2.3.4", false); locked != edge.ID || d.NodeID != edge.ID {
		t.Fatalf("expected edge_full egress routing to shift to edge %d, got decision=%+v locked=%d", edge.ID, d, locked)
	}

	// 5. If all edge nodes go offline, over-quota Master still serves as fallback (never hard-blocked)
	app.pm.edges.mu.Lock()
	if st := app.pm.edges.nodes[edge.ID]; st != nil {
		st.lastSeen = time.Now().Add(-5 * time.Minute)
	}
	app.pm.edges.mu.Unlock()
	if d := app.pm.edges.selectNodeForIP("1.2.3.4"); d.NodeID != 0 {
		t.Fatalf("expected Master fallback when all edges offline, got %+v", d)
	}

	// 6. Reset Master traffic -> back to online & 0 bytes, and Master reclaims #1 even when edge is online
	app.pm.edges.mu.Lock()
	if st := app.pm.edges.nodes[edge.ID]; st != nil {
		st.lastSeen = time.Now()
	}
	app.pm.edges.mu.Unlock()

	resetReq := httptest.NewRequest(http.MethodPost, "/api/master-node/reset-traffic", nil)
	resetRec := httptest.NewRecorder()
	app.handleMasterNodeAction(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("POST /api/master-node/reset-traffic status=%d body=%s", resetRec.Code, resetRec.Body.String())
	}
	var afterReset EdgeNode
	if err := json.Unmarshal(resetRec.Body.Bytes(), &afterReset); err != nil {
		t.Fatalf("unmarshal afterReset: %v", err)
	}
	if afterReset.TrafficUsed != 0 || afterReset.Status != "online" {
		t.Fatalf("expected traffic_used=0 & online after reset, got %+v", afterReset)
	}
	if d := app.pm.edges.selectNodeForIP("1.2.3.4"); d.NodeID != 0 {
		t.Fatalf("expected Master to reclaim #1 after traffic reset, got %+v", d)
	}
}
