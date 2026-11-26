package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestISPProbeRestartKeepsFreshResultsAndCircuitState(t *testing.T) {
	app, edges, _ := setupWaterfallBalanceTestApp(t)
	nodeID, token := insertTestEdgeNode(t, edges, "restart agent", "203.0.113.41", 19090)
	fallbackID, legacyToken := insertTestEdgeNode(t, edges, "legacy agent", "203.0.113.42", 19090)
	route := fmt.Sprintf(`{"group":"ct","nodes":[{"node_id":%d,"enabled":true},{"node_id":%d,"enabled":true},{"node_id":0,"enabled":false}]}`,
		nodeID, fallbackID)
	rec := httptest.NewRecorder()
	app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(route)))
	if rec.Code != http.StatusOK {
		t.Fatalf("save routes: %d %s", rec.Code, rec.Body.String())
	}

	claims := make(map[string]string)
	var issuedClaim string
	var probeConfig *agentISPProbeConfig
	send := func(id int64, nodeToken, instance string, seq uint64, reachable bool) EdgeNode {
		t.Helper()
		request := agentHeartbeatRequest{Version: "v1.5.6", ListenPort: 19090, ISPProbeSeq: seq}
		if instance != "" {
			request.Version = appVersion
		}
		if seq > 0 {
			result := agentISPProbeResult{LatencyMs: -1, TotalCount: 3, CheckedAtUnix: 1700000000 + int64(seq)}
			if reachable {
				result.ReachableCount, result.LatencyMs = 3, 42
			}
			request.ISPProbeResults = map[string]agentISPProbeResult{ispGroupCT: result}
		}
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", bytes.NewReader(payload))
		req.Header.Set("X-Yundu-Node-ID", fmt.Sprint(id))
		req.Header.Set("X-Yundu-Node-Token", nodeToken)
		if instance != "" {
			req.Header.Set(edgeAgentInstanceHeader, instance)
			req.Header.Set(edgeAgentProbeClaimHeader, claims[instance])
		}
		rec := httptest.NewRecorder()
		app.handleAgentHeartbeat(rec, req)
		issuedClaim = rec.Header().Get(edgeAgentProbeClaimHeader)
		var response agentHeartbeatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("heartbeat %d: HTTP %d %s; err=%v", seq, rec.Code, rec.Body.String(), err)
		}
		probeConfig = response.ISPProbeConfig
		if probeConfig == nil {
			t.Fatal("heartbeat did not return the ISP probe configuration")
		}
		view, exists := app.buildEdgeNodeView(id, "", false)
		if !exists || !view.Online {
			t.Fatalf("heartbeat %d did not mark the node online", seq)
		}
		return view
	}
	prepare := func(instance string) EdgeNode {
		t.Helper()
		view := send(nodeID, token, instance, 0, false)
		if len(issuedClaim) != 64 {
			t.Fatalf("startup did not receive an ownership challenge: %q", issuedClaim)
		}
		claims[instance] = issuedClaim
		return view
	}
	assertHealth := func(view EdgeNode, seq int64, fails int, tripped bool) {
		t.Helper()
		if len(view.ISPHealth) != 1 {
			t.Fatalf("expected one ISP probe result, got %+v", view.ISPHealth)
		}
		health := view.ISPHealth[0]
		if health.LastCheckedAt != time.Unix(1700000000+seq, 0).UTC().Format(time.RFC3339) ||
			health.ConsecFails != fails || health.Tripped != tripped || view.AnyCircuitTripped != tripped {
			t.Fatalf("got %+v, want sample %d, consecutive failures %d, tripped %t", health, seq, fails, tripped)
		}
	}
	assertRoute := func(want int64) {
		t.Helper()
		if got := edges.selectNodeForIP("1.2.3.4").NodeID; got != want {
			t.Fatalf("route chose node %d, want %d", got, want)
		}
	}

	oldInstance := strings.Repeat("a", 32)
	newInstance := strings.Repeat("b", 32)
	recoveryInstance := strings.Repeat("c", 32)

	prepare(oldInstance)
	assertHealth(send(nodeID, token, oldInstance, 40, false), 40, 1, false)

	// This older process received a challenge but never established ownership.
	lateStartup := strings.Repeat("e", 32)
	assertHealth(prepare(lateStartup), 40, 1, false)
	assertHealth(prepare(newInstance), 40, 1, false)
	assertHealth(send(nodeID, token, newInstance, 1, false), 1, 1, false)
	assertHealth(send(nodeID, token, newInstance, 1, false), 1, 1, false)
	assertHealth(send(nodeID, token, oldInstance, 41, true), 1, 1, false)
	assertHealth(send(nodeID, token, oldInstance, 0, false), 1, 1, false)
	assertHealth(send(nodeID, token, lateStartup, 0, false), 1, 1, false)
	assertHealth(send(nodeID, token, lateStartup, 1, true), 1, 1, false)
	assertHealth(send(nodeID, token, "", 42, true), 1, 1, false)
	assertHealth(send(nodeID, token, "invalid-instance", 43, true), 1, 1, false)

	edges.mu.Lock()
	edges.nodes[nodeID].ispForceProbeID = "retest"
	edges.mu.Unlock()
	send(nodeID, token, oldInstance, 42, true)
	if probeConfig.ForceProbeID != "" {
		t.Fatal("a late heartbeat consumed the manual retest")
	}
	send(nodeID, token, newInstance, 1, false)
	if probeConfig.ForceProbeID != "retest" {
		t.Fatal("the current process did not receive the manual retest")
	}
	assertRoute(nodeID)
	assertHealth(send(fallbackID, legacyToken, "", 5, true), 5, 0, false)

	send(nodeID, token, newInstance, 2, false)
	assertHealth(send(nodeID, token, newInstance, 3, false), 3, 3, true)
	assertRoute(fallbackID)
	if alerts := edges.collectAlerts(time.Now()); len(alerts) != 1 || !strings.Contains(alerts[0], "已自动熔断") {
		t.Fatalf("expected one trip event, got %v", alerts)
	}

	// One pre-restart success must not shorten the next process's recovery window.
	assertHealth(send(nodeID, token, newInstance, 4, true), 4, 0, true)
	assertHealth(prepare(recoveryInstance), 4, 0, true)
	assertHealth(send(nodeID, token, recoveryInstance, 1, true), 1, 0, true)
	assertHealth(send(nodeID, token, newInstance, 5, true), 1, 0, true)
	assertHealth(send(nodeID, token, recoveryInstance, 1, true), 1, 0, true)
	assertRoute(fallbackID)
	if alerts := edges.collectAlerts(time.Now()); len(alerts) != 0 {
		t.Fatalf("unexpected alert during restart recovery: %v", alerts)
	}
	assertHealth(send(nodeID, token, recoveryInstance, 2, true), 2, 0, false)
	assertRoute(nodeID)
	if alerts := edges.collectAlerts(time.Now()); len(alerts) != 1 || !strings.Contains(alerts[0], "连通已恢复") {
		t.Fatalf("expected one recovery event, got %v", alerts)
	}

	// A monitored node still accepts post-restart samples without tripping.
	body := `{"name":"restart agent","public_port":19090,"public_ip":"203.0.113.41","tls_mode":"http","auto_circuit_breaker":false}`
	edit := httptest.NewRecorder()
	app.handleEdgeNodeByID(edit, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/edge-nodes/%d", nodeID), strings.NewReader(body)))
	if edit.Code != http.StatusOK {
		t.Fatalf("enable monitor-only mode: %d %s", edit.Code, edit.Body.String())
	}
	monitorInstance := strings.Repeat("d", 32)
	prepare(monitorInstance)
	for seq := uint64(1); seq <= 3; seq++ {
		assertHealth(send(nodeID, token, monitorInstance, seq, false), int64(seq), 0, false)
	}
	assertRoute(nodeID)
	if alerts := edges.collectAlerts(time.Now()); len(alerts) != 0 {
		t.Fatalf("monitor-only mode produced circuit alerts: %v", alerts)
	}

	// A downgraded Agent can send fresh untagged samples after the identified
	// process has stopped heartbeating. A stale claim still cannot return.
	edges.mu.Lock()
	edges.nodes[nodeID].ispProbeSeenAt = time.Now().Add(-2 * edgeNodeOfflineAfter)
	edges.mu.Unlock()
	assertHealth(send(nodeID, token, "", 1, true), 1, 0, false)
	assertHealth(send(nodeID, token, monitorInstance, 4, false), 1, 0, false)
	assertRoute(nodeID)
}

func TestAgentHeartbeatInstanceHeaderIsStableAndJSONRemainsCompatible(t *testing.T) {
	type sentProbeHeaders struct{ instance, claim string }
	headers := make(chan sentProbeHeaders, 3)
	expectedClaim := strings.Repeat("f", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An older Master strictly decodes the existing JSON schema.
		var req agentHeartbeatRequest
		if err := decodeJSONBodyLimit(w, r, &req, edgeHeartbeatBodyLimit); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		headers <- sentProbeHeaders{
			instance: r.Header.Get(edgeAgentInstanceHeader),
			claim:    r.Header.Get(edgeAgentProbeClaimHeader),
		}
		w.Header().Set(edgeAgentProbeClaimHeader, expectedClaim)
		_, _ = w.Write([]byte(`{"ok":true,"node_enabled":true}`))
	}))
	defer server.Close()
	cfg := edgeAgentConfig{MasterURL: server.URL, NodeID: 1, Token: "test-token", Port: 19090}
	agent := newEdgeAgent(cfg)
	for _, runner := range []*edgeAgent{agent, agent, newEdgeAgent(cfg)} {
		if err := runner.sendHeartbeat(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	first, second, restarted := <-headers, <-headers, <-headers
	if len(first.instance) != 32 || len(restarted.instance) != 32 ||
		first.instance != second.instance || first.instance == restarted.instance {
		t.Fatalf("wanted a stable unique process ID, got %q %q %q", first.instance, second.instance, restarted.instance)
	}
	if first.claim != "" || second.claim != expectedClaim || restarted.claim != "" {
		t.Fatalf("only the original process should echo its challenge, got %q %q %q", first.claim, second.claim, restarted.claim)
	}
}
