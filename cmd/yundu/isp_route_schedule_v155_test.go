package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRouteScheduleNormalizationAndCrossMidnightWindow(t *testing.T) {
	mode, start, end, err := normalizeRouteSchedule("", "", "")
	if err != nil || mode != routeScheduleModeAlways || start != "" || end != "" {
		t.Fatalf("empty schedule should normalize to always, got mode=%q start=%q end=%q err=%v", mode, start, end, err)
	}

	mode, start, end, err = normalizeRouteSchedule("avoid", "19:30", "00:30")
	if err != nil || mode != routeScheduleModeAvoid || start != "19:30" || end != "00:30" {
		t.Fatalf("expected avoid 19:30-00:30, got mode=%q start=%q end=%q err=%v", mode, start, end, err)
	}

	if _, _, _, err := normalizeRouteSchedule("avoid", "20:00", "20:00"); err == nil {
		t.Fatalf("expected identical start/end to be rejected")
	}
	if _, _, _, err := normalizeRouteSchedule("only", "25:00", "01:00"); err == nil {
		t.Fatalf("expected invalid hour to be rejected")
	}
	if _, _, _, err := normalizeRouteSchedule("invalid", "19:30", "00:30"); err == nil {
		t.Fatalf("expected unknown schedule mode to be rejected")
	}

	bjTime := func(h, m int) time.Time {
		return time.Date(2026, 11, 25, h, m, 0, 0, routeScheduleBeijingLoc).UTC()
	}

	avoidPeak := ispRouteRule{
		NodeID:        1,
		Enabled:       true,
		ScheduleMode:  routeScheduleModeAvoid,
		ScheduleStart: "19:30",
		ScheduleEnd:   "00:30",
	}
	if !avoidPeak.isScheduledActiveAt(bjTime(19, 29)) {
		t.Fatalf("19:29 CST should still be active before 19:30 peak")
	}
	if avoidPeak.isScheduledActiveAt(bjTime(19, 30)) {
		t.Fatalf("19:30 CST should be dormant during avoid 19:30-00:30")
	}
	if avoidPeak.isScheduledActiveAt(bjTime(23, 59)) {
		t.Fatalf("23:59 CST should be dormant during avoid 19:30-00:30")
	}
	if avoidPeak.isScheduledActiveAt(bjTime(0, 29)) {
		t.Fatalf("00:29 CST should still be dormant across midnight")
	}
	if !avoidPeak.isScheduledActiveAt(bjTime(0, 30)) {
		t.Fatalf("00:30 CST should resume active right at end boundary")
	}
	if got := avoidPeak.scheduleResumeAt(bjTime(21, 0)); got != "00:30" {
		t.Fatalf("expected resume time 00:30, got %q", got)
	}

	onlyPeak := ispRouteRule{
		NodeID:        2,
		Enabled:       true,
		ScheduleMode:  routeScheduleModeOnly,
		ScheduleStart: "19:30",
		ScheduleEnd:   "00:30",
	}
	if onlyPeak.isScheduledActiveAt(bjTime(14, 0)) {
		t.Fatalf("14:00 CST should be dormant for only 19:30-00:30")
	}
	if got := onlyPeak.scheduleResumeAt(bjTime(14, 0)); got != "19:30" {
		t.Fatalf("expected only-peak resume time 19:30, got %q", got)
	}
	if !onlyPeak.isScheduledActiveAt(bjTime(21, 0)) || !onlyPeak.isScheduledActiveAt(bjTime(0, 15)) {
		t.Fatalf("21:00 and 00:15 CST should be active for only 19:30-00:30")
	}
}

func TestRouteSchedulePeakAvoidanceRoutingAndReorderPersistence(t *testing.T) {
	origNow := routeScheduleNow
	defer func() { routeScheduleNow = origNow }()

	fixedNow := time.Date(2026, 11, 25, 14, 0, 0, 0, routeScheduleBeijingLoc)
	routeScheduleNow = func() time.Time { return fixedNow }

	db, err := openDB(t.TempDir() + "/schedule.db")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, nil)
	pm.edges = newEdgeNodeRuntime(db, pm, nil)
	app := &App{db: db, pm: pm}

	natID, _ := insertTestEdgeNode(t, pm.edges, "懒猫美国NAT", "1.2.3.4", 19091)
	optID, _ := insertTestEdgeNode(t, pm.edges, "优化美西", "5.6.7.8", 19092)
	if err := pm.edges.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	markOnline := func(now time.Time) {
		pm.edges.mu.Lock()
		for _, id := range []int64{natID, optID} {
			if n := pm.edges.nodes[id]; n != nil {
				n.lastSeen = now
				n.detectedIP = "1.2.3.4"
				n.supportsEgress = true
			}
		}
		pm.edges.mu.Unlock()
	}
	markOnline(fixedNow)

	putRoutes := func(body any) map[string]any {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPut, "/api/isp-routes", bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		app.handleISPRoutes(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /api/isp-routes failed (%d): %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return out
	}

	// Arrange order across ct/cu/cm: #1 NAT, #2 Opt, #3 Master
	for _, g := range []string{ispGroupCT, ispGroupCU, ispGroupCM} {
		putRoutes(map[string]any{
			"group": g,
			"nodes": []map[string]any{
				{"node_id": natID, "enabled": true},
				{"node_id": optID, "enabled": true},
				{"node_id": 0, "enabled": true},
			},
		})
	}

	// Configure NAT to avoid 19:30-00:30 on cu + cm in one call; ct stays all-day.
	putRoutes(map[string]any{
		"schedule_patch": map[string]any{
			"node_id":        natID,
			"groups":         []string{ispGroupCU, ispGroupCM},
			"schedule_mode":  "avoid",
			"schedule_start": "19:30",
			"schedule_end":   "00:30",
		},
	})

	// Daytime (14:00 CST): all 3 groups pick NAT (#1).
	pm.edges.mu.RLock()
	ctDay, _ := pm.edges.selectEgressTargetLocked(ispGroupCT, nil, false, fixedNow)
	cuDay, _ := pm.edges.selectEgressTargetLocked(ispGroupCU, nil, false, fixedNow)
	cmDay, _ := pm.edges.selectEgressTargetLocked(ispGroupCM, nil, false, fixedNow)
	pm.edges.mu.RUnlock()
	if ctDay.NodeID != natID || cuDay.NodeID != natID || cmDay.NodeID != natID {
		t.Fatalf("expected all groups to pick NAT during daytime, got ct=%d cu=%d cm=%d", ctDay.NodeID, cuDay.NodeID, cmDay.NodeID)
	}

	// Evening peak (21:00 CST): ct keeps NAT, cu + cm automatically switch to Opt!
	fixedNow = time.Date(2026, 11, 25, 21, 0, 0, 0, routeScheduleBeijingLoc)
	markOnline(fixedNow)

	pm.edges.mu.RLock()
	ctPeak, _ := pm.edges.selectEgressTargetLocked(ispGroupCT, nil, false, fixedNow)
	cuPeak, _ := pm.edges.selectEgressTargetLocked(ispGroupCU, nil, false, fixedNow)
	cmPeak, _ := pm.edges.selectEgressTargetLocked(ispGroupCM, nil, false, fixedNow)
	pm.edges.mu.RUnlock()
	if ctPeak.NodeID != natID {
		t.Fatalf("expected ct to remain on NAT during peak, got %d", ctPeak.NodeID)
	}
	if cuPeak.NodeID != optID || cmPeak.NodeID != optID {
		t.Fatalf("expected cu & cm to switch to Opt during peak, got cu=%d cm=%d", cuPeak.NodeID, cmPeak.NodeID)
	}

	// Verify GET /api/isp-routes view marks NAT dormant (resume 00:30) and Opt active in cm.
	views := app.buildISPRouteGroups()
	var foundCM bool
	for _, g := range views {
		if g.Group != ispGroupCM {
			continue
		}
		foundCM = true
		if len(g.Nodes) < 2 {
			t.Fatalf("expected at least 2 nodes in cm")
		}
		if g.Nodes[0].NodeID != natID || !g.Nodes[0].ScheduleDormant || g.Nodes[0].ScheduleResumeAt != "00:30" || g.Nodes[0].Active {
			t.Fatalf("expected NAT dormant in cm during peak, got %+v", g.Nodes[0])
		}
		if g.Nodes[1].NodeID != optID || !g.Nodes[1].Active {
			t.Fatalf("expected Opt active in cm during peak, got %+v", g.Nodes[1])
		}
	}
	if !foundCM {
		t.Fatalf("cm group missing from buildISPRouteGroups")
	}

	// Verify drag-reorder without schedule fields preserves existing schedule!
	putRoutes(map[string]any{
		"group": ispGroupCM,
		"nodes": []map[string]any{
			{"node_id": natID, "enabled": true},
			{"node_id": optID, "enabled": true},
			{"node_id": 0, "enabled": true},
		},
	})
	pm.edges.mu.RLock()
	cmStillPeak, _ := pm.edges.selectEgressTargetLocked(ispGroupCM, nil, false, fixedNow)
	pm.edges.mu.RUnlock()
	if cmStillPeak.NodeID != optID {
		t.Fatalf("expected schedule to survive reorder save and keep cm on Opt during peak, got %d", cmStillPeak.NodeID)
	}

	// Verify balance mode skips dormant NAT during peak.
	putRoutes(map[string]any{
		"group":    ispGroupCM,
		"strategy": "balance",
	})
	pm.edges.mu.RLock()
	cmBalancePeak, _ := pm.edges.selectEgressTargetLocked(ispGroupCM, map[int64]int{}, true, fixedNow)
	pm.edges.mu.RUnlock()
	if cmBalancePeak.NodeID != optID {
		t.Fatalf("expected balance strategy to skip dormant NAT and pick Opt, got %d", cmBalancePeak.NodeID)
	}

	// After midnight (00:30 CST): NAT automatically resumes as #1!
	fixedNow = time.Date(2026, 11, 26, 0, 30, 0, 0, routeScheduleBeijingLoc)
	markOnline(fixedNow)
	pm.edges.mu.RLock()
	cmResumed, _ := pm.edges.selectEgressTargetLocked(ispGroupCM, map[int64]int{}, true, fixedNow)
	pm.edges.mu.RUnlock()
	if cmResumed.NodeID != natID {
		t.Fatalf("expected NAT to automatically resume at 00:30 CST, got %d", cmResumed.NodeID)
	}

	// Safety fallback: if ALL nodes in cu (including Master) are dormant during peak, still falls back to Master (NodeID=0).
	fixedNow = time.Date(2026, 11, 25, 22, 0, 0, 0, routeScheduleBeijingLoc)
	markOnline(fixedNow)
	putRoutes(map[string]any{
		"schedule_patch": map[string]any{
			"node_id":        optID,
			"groups":         []string{ispGroupCU},
			"schedule_mode":  "avoid",
			"schedule_start": "19:30",
			"schedule_end":   "00:30",
		},
	})
	putRoutes(map[string]any{
		"schedule_patch": map[string]any{
			"node_id":        0,
			"groups":         []string{ispGroupCU},
			"schedule_mode":  "avoid",
			"schedule_start": "19:30",
			"schedule_end":   "00:30",
		},
	})
	pm.edges.mu.RLock()
	cuFallback, cuLocked := pm.edges.selectEgressTargetLocked(ispGroupCU, nil, false, fixedNow)
	pm.edges.mu.RUnlock()
	if cuFallback.NodeID != 0 || cuLocked != -1 {
		t.Fatalf("expected all-dormant group to fall back safely to Master, got node=%d locked=%d", cuFallback.NodeID, cuLocked)
	}
}
