package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEdgeOfflineThresholdIs25Seconds(t *testing.T) {
	if edgeNodeOfflineAfter != 25*time.Second {
		t.Fatalf("edgeNodeOfflineAfter = %v, want 25s", edgeNodeOfflineAfter)
	}
	now := time.Now()
	node := &edgeNodeLiveState{lastSeen: now.Add(-24 * time.Second)}
	if !node.isOnline(now) {
		t.Fatalf("expected node seen 24s ago to be online")
	}
	node.lastSeen = now.Add(-26 * time.Second)
	if node.isOnline(now) {
		t.Fatalf("expected node seen 26s ago to be offline")
	}
}

func TestISPProbeCircuitBreakerAutoFailoverAndRecovery(t *testing.T) {
	app, edges, shares := setupWaterfallBalanceTestApp(t)
	nat1, tok1 := insertTestEdgeNode(t, edges, "RFC-JPCO", "203.0.113.31", 19090)
	nat2, _ := insertTestEdgeNode(t, edges, "懒猫美国NAT", "203.0.113.32", 19090)

	// Put nat1 #1, nat2 #2, Master #3 on ct & cu (priority mode on ct, balance on cu).
	for _, item := range []struct {
		group, strategy string
	}{
		{"ct", "priority"},
		{"cu", "balance"},
	} {
		body := fmt.Sprintf(
			`{"group":%q,"strategy":%q,"nodes":[{"node_id":%d,"enabled":true,"max_balance_streams":2},{"node_id":%d,"enabled":true,"max_balance_streams":2},{"node_id":0,"enabled":true,"max_balance_streams":2}]}`,
			item.group, item.strategy, nat1, nat2,
		)
		rec := httptest.NewRecorder()
		app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT isp-routes %s status=%d body=%s", item.group, rec.Code, rec.Body.String())
		}
	}

	sendHB := func(seq uint64, results map[string]agentISPProbeResult) agentHeartbeatResponse {
		payload, _ := json.Marshal(agentHeartbeatRequest{
			Version:         appVersion,
			ListenPort:      19090,
			ISPProbeSeq:     seq,
			ISPProbeResults: results,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", bytes.NewReader(payload))
		req.Header.Set("X-Yundu-Node-ID", fmt.Sprintf("%d", nat1))
		req.Header.Set("X-Yundu-Node-Token", tok1)
		rec := httptest.NewRecorder()
		app.handleAgentHeartbeat(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat seq=%d status=%d body=%s", seq, rec.Code, rec.Body.String())
		}
		var reply agentHeartbeatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("decode heartbeat reply: %v", err)
		}
		return reply
	}

	// 1. Partial failure (1/3 reachable on ct) MUST NOT trip breaker.
	reply := sendHB(1, map[string]agentISPProbeResult{
		"ct": {LatencyMs: 42, ReachableCount: 1, TotalCount: 3},
		"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
		"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
	})
	if reply.ISPProbeConfig == nil || reply.ISPProbeConfig.IntervalSec != 30 {
		t.Fatalf("expected normal interval 30s, got %+v", reply.ISPProbeConfig)
	}
	if d := edges.selectNodeForIP("1.2.3.4"); d.NodeID != nat1 {
		t.Fatalf("partial success should keep routing to nat1, got %d", d.NodeID)
	}

	// 2. Two consecutive total failures on ct (0/3) -> still below fail_threshold (3).
	for _, seq := range []uint64{2, 3} {
		sendHB(seq, map[string]agentISPProbeResult{
			"ct": {LatencyMs: -1, ReachableCount: 0, TotalCount: 3},
			"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
			"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
		})
	}
	if d := edges.selectNodeForIP("1.2.3.4"); d.NodeID != nat1 {
		t.Fatalf("2 fails (<3) should not trip yet, got %d", d.NodeID)
	}

	// 3. Re-sending duplicate seq=3 MUST NOT double-count and trip!
	sendHB(3, map[string]agentISPProbeResult{
		"ct": {LatencyMs: -1, ReachableCount: 0, TotalCount: 3},
		"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
		"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
	})
	if d := edges.selectNodeForIP("1.2.3.4"); d.NodeID != nat1 {
		t.Fatalf("duplicate seq=3 must not trip, got %d", d.NodeID)
	}

	// 4. Third consecutive failure (seq=4) trips ct!
	//    - ct immediately fails over to nat2
	//    - cu stays healthy on nat1 (per-ISP isolation)
	//    - Agent heartbeat automatically switches to RecoveryIntervalSec (180s)
	//    - Exactly 1 TG tripped alert queued!
	reply = sendHB(4, map[string]agentISPProbeResult{
		"ct": {LatencyMs: -1, ReachableCount: 0, TotalCount: 3},
		"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
		"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
	})
	if reply.ISPProbeConfig == nil || reply.ISPProbeConfig.IntervalSec != 180 {
		t.Fatalf("tripped node should slow down to 180s recovery interval, got %+v", reply.ISPProbeConfig)
	}
	if d := edges.selectNodeForIP("1.2.3.4"); d.NodeID != nat2 {
		t.Fatalf("ct after trip should failover to nat2 (%d), got %d", nat2, d.NodeID)
	}
	if d := edges.selectNodeForSessionDispatch("2.3.4.5", "cu-tok-1"); d.NodeID != nat1 {
		t.Fatalf("cu should remain unaffected on nat1 (%d), got %d", nat1, d.NodeID)
	}
	_ = shares

	alerts := edges.collectAlerts(time.Now())
	if len(alerts) != 1 || !strings.Contains(alerts[0], "疑似被阻断") || !strings.Contains(alerts[0], "电信") {
		t.Fatalf("expected 1 TG tripped alert for 电信, got %+v", alerts)
	}
	// Further failures while tripped MUST NOT spam TG alerts!
	sendHB(5, map[string]agentISPProbeResult{
		"ct": {LatencyMs: -1, ReachableCount: 0, TotalCount: 3},
		"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
		"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
	})
	if more := edges.collectAlerts(time.Now()); len(more) != 0 {
		t.Fatalf("expected 0 duplicate alerts while tripped, got %+v", more)
	}

	// 5. Recovery: 1 success (seq=6) stays tripped (< recover_threshold=2);
	//    2nd consecutive success (seq=7) auto-recovers ct, restores 30s interval & sends 1 TG recovery alert!
	sendHB(6, map[string]agentISPProbeResult{
		"ct": {LatencyMs: 41, ReachableCount: 2, TotalCount: 3},
		"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
		"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
	})
	if d := edges.selectNodeForIP("1.2.3.4"); d.NodeID != nat2 {
		t.Fatalf("1 success (<2) should stay tripped on nat2, got %d", d.NodeID)
	}

	reply = sendHB(7, map[string]agentISPProbeResult{
		"ct": {LatencyMs: 39, ReachableCount: 3, TotalCount: 3},
		"cu": {LatencyMs: 38, ReachableCount: 3, TotalCount: 3},
		"cm": {LatencyMs: 55, ReachableCount: 3, TotalCount: 3},
	})
	if reply.ISPProbeConfig == nil || reply.ISPProbeConfig.IntervalSec != 30 {
		t.Fatalf("recovered node should restore 30s interval, got %+v", reply.ISPProbeConfig)
	}
	if d := edges.selectNodeForIP("1.2.3.4"); d.NodeID != nat1 {
		t.Fatalf("recovered ct should return to nat1 (%d), got %d", nat1, d.NodeID)
	}
	recAlerts := edges.collectAlerts(time.Now())
	if len(recAlerts) != 1 || !strings.Contains(recAlerts[0], "连通已恢复") {
		t.Fatalf("expected 1 TG recovery alert, got %+v", recAlerts)
	}

	// 6. Test live concurrent TCP prober against deterministic loopback listener.
	ispProbeAllowLoopbackInTests = true
	t.Cleanup(func() { ispProbeAllowLoopbackInTests = false })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	res := runISPProbeTargets(context.Background(), map[string][]string{
		"ct": {ln.Addr().String()},
	})
	if res["ct"].ReachableCount != 1 || res["ct"].TotalCount != 1 || res["ct"].LatencyMs <= 0 {
		t.Fatalf("expected reachable loopback probe, got %+v", res["ct"])
	}

	// 7. Settings API: save custom settings and verify disabling clears tripped state.
	rec := httptest.NewRecorder()
	app.handleISPProbeSettings(rec, httptest.NewRequest(http.MethodGet, "/api/isp-probe-settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/isp-probe-settings status=%d", rec.Code)
	}
	var cfg ISPProbeSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if !cfg.Enabled || len(cfg.Targets["ct"]) != 3 || !strings.HasPrefix(cfg.Targets["ct"][0], "bj-ct-v4.ip.zstaticcdn.com:") {
		t.Fatalf("unexpected default ISP probe settings: %+v", cfg)
	}
}

func TestEdgeNodeAutoCircuitBreakerCanMonitorWithoutFailover(t *testing.T) {
	app, edges, _ := setupWaterfallBalanceTestApp(t)
	nodeID, token := insertTestEdgeNode(t, edges, "monitor node", "203.0.113.41", 19090)
	fallbackID, _ := insertTestEdgeNode(t, edges, "fallback node", "203.0.113.42", 19090)

	if _, err := app.db.db.Exec(`ALTER TABLE edge_nodes DROP COLUMN auto_circuit_breaker`); err != nil {
		t.Fatal(err)
	}
	if err := app.db.migrate(); err != nil {
		t.Fatalf("migrate existing edge nodes: %v", err)
	}
	route := fmt.Sprintf(`{"group":"ct","nodes":[{"node_id":%d,"enabled":true},{"node_id":%d,"enabled":true},{"node_id":0,"enabled":false}]}`,
		nodeID, fallbackID)
	rec := httptest.NewRecorder()
	app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(route)))
	if rec.Code != http.StatusOK {
		t.Fatalf("save routes: %d %s", rec.Code, rec.Body.String())
	}

	assertCircuitSetting := func(id int64, want bool) EdgeNode {
		t.Helper()
		view, ok := app.buildEdgeNodeView(id, "", false)
		if !ok || view.AutoCircuitBreaker == nil || *view.AutoCircuitBreaker != want {
			t.Fatalf("node %d circuit setting = %v, want %t", id, view.AutoCircuitBreaker, want)
		}
		return view
	}
	assertRoute := func(want int64) {
		t.Helper()
		if g := edges.selectNodeForIP("1.2.3.4").NodeID; g != want {
			t.Fatalf("route chose node %d, want %d", g, want)
		}
	}
	sendFailure := func(seq uint64, wantInterval int) {
		t.Helper()
		payload, err := json.Marshal(agentHeartbeatRequest{
			Version: "v1.5.5", ListenPort: 19090, ISPProbeSeq: seq,
			ISPProbeResults: map[string]agentISPProbeResult{ispGroupCT: {LatencyMs: -1, TotalCount: 3}},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", bytes.NewReader(payload))
		req.Header.Set("X-Yundu-Node-ID", fmt.Sprint(nodeID))
		req.Header.Set("X-Yundu-Node-Token", token)
		rec := httptest.NewRecorder()
		app.handleAgentHeartbeat(rec, req)
		var reply agentHeartbeatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("heartbeat %d: status=%d %s; err=%v", seq, rec.Code, rec.Body.String(), err)
		}
		if reply.ISPProbeConfig == nil || !reply.ISPProbeConfig.Enabled || reply.ISPProbeConfig.IntervalSec != wantInterval {
			t.Fatalf("heartbeat %d probe config = %+v, want enabled with interval %d", seq, reply.ISPProbeConfig, wantInterval)
		}
	}
	update := func(extraJSON string) {
		t.Helper()
		body := `{"name":"monitor node","public_port":19090,"public_ip":"203.0.113.41","tls_mode":"http"` + extraJSON + "}"
		rec := httptest.NewRecorder()
		app.handleEdgeNodeByID(rec, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/edge-nodes/%d", nodeID), strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("update node: %d %s", rec.Code, rec.Body.String())
		}
	}

	assertCircuitSetting(nodeID, true)
	sendFailure(1, defaultISPProbeIntervalSec)
	sendFailure(2, defaultISPProbeIntervalSec)
	sendFailure(3, defaultISPProbeRecoveryIntervalSec)
	assertRoute(fallbackID)
	if !assertCircuitSetting(nodeID, true).AnyCircuitTripped {
		t.Fatal("expected node to be tripped before disabling the switch")
	}
	edges.mu.Lock()
	edges.queueISPAlertsLocked(fallbackID, []string{"another node alert"})
	edges.mu.Unlock()

	update(`,"auto_circuit_breaker":false`)
	if alerts := edges.collectAlerts(time.Now()); len(alerts) != 1 || alerts[0] != "another node alert" {
		t.Fatalf("disabling a circuit should drop only its pending probe alerts: %v", alerts)
	}
	assertRoute(nodeID)
	view := assertCircuitSetting(nodeID, false)
	if view.AnyCircuitTripped || len(view.ISPHealth) != 1 || view.ISPHealth[0].Tripped || view.ISPHealth[0].TotalCount != 3 {
		t.Fatalf("disabling should clear the circuit and preserve the last sample: %+v", view.ISPHealth)
	}
	update("")
	assertCircuitSetting(nodeID, false)
	assertCircuitSetting(fallbackID, true)

	for seq := uint64(4); seq <= 7; seq++ {
		sendFailure(seq, defaultISPProbeIntervalSec)
	}
	assertRoute(nodeID)
	view = assertCircuitSetting(nodeID, false)
	if view.AnyCircuitTripped || len(view.ISPHealth) != 1 || view.ISPHealth[0].ReachableCount != 0 ||
		view.ISPHealth[0].LastCheckedAt == "" || view.ISPHealth[0].ConsecFails != 0 {
		t.Fatalf("monitor-only mode should show new failed samples without tripping: %+v", view.ISPHealth)
	}
	if alerts := edges.collectAlerts(time.Now()); len(alerts) != 0 {
		t.Fatalf("monitor-only mode produced circuit alerts: %v", alerts)
	}

	update(`,"auto_circuit_breaker":true`)
	assertCircuitSetting(nodeID, true)
	sendFailure(7, defaultISPProbeIntervalSec)
	sendFailure(8, defaultISPProbeIntervalSec)
	sendFailure(9, defaultISPProbeIntervalSec)
	assertRoute(nodeID)
	sendFailure(10, defaultISPProbeRecoveryIntervalSec)
	assertRoute(fallbackID)
}
