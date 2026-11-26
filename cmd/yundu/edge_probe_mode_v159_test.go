package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEdgeProbeModePerNodeICMPAndOffAndPerCityBreakdown(t *testing.T) {
	prevHook := ispICMPProbeHook
	t.Cleanup(func() { ispICMPProbeHook = prevHook })

	mockLatency := map[string]int{
		"36.110.147.20":   47,
		"202.96.209.133":  24,
		"202.96.128.86":   55,
		"123.125.81.6":    52,
		"210.22.97.1":     31,
		"221.5.88.88":     -1,
		"221.179.155.161": 57,
		"211.136.112.200": 40,
		"211.136.192.6":   59,
	}
	ispICMPProbeHook = func(_ context.Context, host string) int {
		if ms, ok := mockLatency[host]; ok {
			return ms
		}
		return -1
	}

	// 1. Verify ICMP probing builds ordered per-city breakdown (北京 / 上海 / 广东).
	results := runISPProbeTargetsWithMode(context.Background(), edgeProbeModeICMP, defaultISPICMPTargets())
	ctRes, ok := results[ispGroupCT]
	if !ok || ctRes.ReachableCount != 3 || ctRes.TotalCount != 3 || ctRes.LatencyMs != 24 {
		t.Fatalf("unexpected CT ICMP result: %+v", ctRes)
	}
	if len(ctRes.Targets) != 3 ||
		ctRes.Targets[0].Label != "北京" || ctRes.Targets[0].LatencyMs != 47 || !ctRes.Targets[0].Reachable ||
		ctRes.Targets[1].Label != "上海" || ctRes.Targets[1].LatencyMs != 24 || !ctRes.Targets[1].Reachable ||
		ctRes.Targets[2].Label != "广东" || ctRes.Targets[2].LatencyMs != 55 || !ctRes.Targets[2].Reachable {
		t.Fatalf("unexpected CT per-city targets: %+v", ctRes.Targets)
	}
	cuRes := results[ispGroupCU]
	if cuRes.ReachableCount != 2 || cuRes.LatencyMs != 31 || len(cuRes.Targets) != 3 || cuRes.Targets[2].Reachable || cuRes.Targets[2].Label != "广东" {
		t.Fatalf("unexpected CU partial ICMP result: %+v", cuRes)
	}

	// 2. Verify full API CRUD + heartbeat config push + auto circuit breaker in ICMP mode + off mode cleanup.
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, nil)
	pm.edges = newEdgeNodeRuntime(db, pm, nil)
	app := &App{db: db, pm: pm}

	// Create node in ICMP mode.
	createBody := `{"name":"RFC-JPCO","public_port":19090,"probe_mode":"icmp"}`
	req := httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(createBody))
	rec := httptest.NewRecorder()
	app.handleEdgeNodes(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create node failed (%d): %s", rec.Code, rec.Body.String())
	}
	var created EdgeNode
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created node: %v", err)
	}
	if created.ProbeMode != edgeProbeModeICMP {
		t.Fatalf("expected probe_mode=icmp, got %q", created.ProbeMode)
	}
	if created.AutoCircuitBreaker == nil || !*created.AutoCircuitBreaker {
		t.Fatalf("expected auto_circuit_breaker=true by default in icmp mode")
	}
	if !strings.Contains(created.UninstallCommand, "uninstall-agent.sh | sh") {
		t.Fatalf("expected POSIX sh uninstall command, got %q", created.UninstallCommand)
	}

	// Retrieve node token for heartbeat simulation.
	pm.edges.mu.RLock()
	token := pm.edges.nodes[created.ID].tokenPlain
	pm.edges.mu.RUnlock()

	sendHeartbeat := func(seq uint64, rep map[string]agentISPProbeResult) agentHeartbeatResponse {
		t.Helper()
		payload := agentHeartbeatRequest{
			Version:         appVersion,
			ISPProbeSeq:     seq,
			ISPProbeResults: rep,
		}
		buf, _ := json.Marshal(payload)
		hbReq := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", bytes.NewReader(buf))
		hbReq.RemoteAddr = "127.0.0.1:40000"
		hbReq.Header.Set("X-Yundu-Node-ID", strconv.FormatInt(created.ID, 10))
		hbReq.Header.Set("X-Yundu-Node-Token", token)
		hbRec := httptest.NewRecorder()
		app.handleAgentHeartbeat(hbRec, hbReq)
		if hbRec.Code != http.StatusOK {
			t.Fatalf("heartbeat failed (%d): %s", hbRec.Code, hbRec.Body.String())
		}
		var resp agentHeartbeatResponse
		if err := json.Unmarshal(hbRec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode heartbeat resp: %v", err)
		}
		return resp
	}

	// Heartbeat 1: delivers ICMP probe config & per-city results.
	hbResp := sendHeartbeat(1, results)
	if hbResp.ISPProbeConfig == nil || !hbResp.ISPProbeConfig.Enabled || hbResp.ISPProbeConfig.Mode != edgeProbeModeICMP {
		t.Fatalf("expected heartbeat ISPProbeConfig mode=icmp enabled=true, got %+v", hbResp.ISPProbeConfig)
	}
	if len(hbResp.ISPProbeConfig.Targets[ispGroupCT]) != 3 || hbResp.ISPProbeConfig.Targets[ispGroupCT][1] != "202.96.209.133" {
		t.Fatalf("expected ICMP targets in heartbeat config, got %+v", hbResp.ISPProbeConfig.Targets)
	}

	view, ok := app.buildEdgeNodeView(created.ID, "https://panel.example.com", false)
	if !ok || len(view.ISPHealth) != 3 || len(view.ISPHealth[0].Targets) != 3 || view.ISPHealth[0].Targets[0].Label != "北京" {
		t.Fatalf("expected per-city targets in EdgeNode view, got %+v", view.ISPHealth)
	}

	// 3 consecutive CT failures in ICMP mode must trip CT circuit breaker automatically.
	failCT := map[string]agentISPProbeResult{
		ispGroupCT: {
			LatencyMs:      -1,
			ReachableCount: 0,
			TotalCount:     3,
			CheckedAtUnix:  time.Now().Unix(),
			Targets: []agentISPProbeTargetResult{
				{Target: "36.110.147.20", Label: "北京", Reachable: false, LatencyMs: -1},
				{Target: "202.96.209.133", Label: "上海", Reachable: false, LatencyMs: -1},
				{Target: "202.96.128.86", Label: "广东", Reachable: false, LatencyMs: -1},
			},
		},
	}
	sendHeartbeat(2, failCT)
	sendHeartbeat(3, failCT)
	sendHeartbeat(4, failCT)

	view, _ = app.buildEdgeNodeView(created.ID, "https://panel.example.com", false)
	if !view.AnyCircuitTripped {
		t.Fatalf("expected CT circuit to trip after 3 consecutive ICMP failures")
	}

	// Switch node to probe_mode="off": clears circuit & health, disables agent probe, preserves auto_circuit_breaker.
	offBody := `{"name":"RFC-JPCO","public_port":19090,"probe_mode":"off"}`
	putReq := httptest.NewRequest(http.MethodPut, "/api/edge-nodes/1", strings.NewReader(offBody))
	putRec := httptest.NewRecorder()
	app.handleEdgeNodeByID(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("update node to off failed (%d): %s", putRec.Code, putRec.Body.String())
	}
	var updated EdgeNode
	_ = json.Unmarshal(putRec.Body.Bytes(), &updated)
	if updated.ProbeMode != edgeProbeModeOff || updated.AnyCircuitTripped || len(updated.ISPHealth) != 0 {
		t.Fatalf("expected clean off state, got mode=%q tripped=%v health=%+v", updated.ProbeMode, updated.AnyCircuitTripped, updated.ISPHealth)
	}
	if updated.AutoCircuitBreaker == nil || !*updated.AutoCircuitBreaker {
		t.Fatalf("expected auto_circuit_breaker preference preserved as true when switching to off")
	}
	hbOff := sendHeartbeat(5, failCT)
	if hbOff.ISPProbeConfig == nil || hbOff.ISPProbeConfig.Enabled || hbOff.ISPProbeConfig.Mode != edgeProbeModeOff {
		t.Fatalf("expected disabled off config in heartbeat, got %+v", hbOff.ISPProbeConfig)
	}

	// 3. Verify /api/isp-probe-settings persists custom icmp_targets and supports reset_defaults.
	putCfg := httptest.NewRequest(http.MethodPut, "/api/isp-probe-settings", strings.NewReader(`{
		"enabled": true,
		"icmp_targets": {"ct":["202.96.209.133"],"cu":["210.22.97.1"],"cm":["211.136.112.200"]}
	}`))
	cfgRec := httptest.NewRecorder()
	app.handleISPProbeSettings(cfgRec, putCfg)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("save icmp_targets failed (%d): %s", cfgRec.Code, cfgRec.Body.String())
	}
	loaded := db.loadISPProbeSettings()
	if len(loaded.ICMPTargets[ispGroupCT]) != 1 || loaded.ICMPTargets[ispGroupCT][0] != "202.96.209.133" {
		t.Fatalf("expected persisted custom icmp_targets, got %+v", loaded.ICMPTargets)
	}
}
