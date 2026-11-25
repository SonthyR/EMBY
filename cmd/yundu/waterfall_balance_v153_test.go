package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupWaterfallBalanceTestApp(t *testing.T) (*App, *edgeNodeRuntime, *shareUserRuntime) {
	t.Helper()
	originalSecret := jwtSecret
	jwtSecret = []byte("v153-waterfall-test-secret-key-00000")
	t.Cleanup(func() { jwtSecret = originalSecret })

	dbPath := filepath.Join(t.TempDir(), "waterfall.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ipDir := filepath.Join(t.TempDir(), "ip2region")
	if err := os.MkdirAll(ipDir, 0o700); err != nil {
		t.Fatalf("mkdir ip2region: %v", err)
	}
	v4Raw := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|0|广东省|深圳市|电信"},
		{start: netip.MustParseAddr("2.3.4.0"), end: netip.MustParseAddr("2.3.4.255"), region: "中国|0|北京市|北京市|联通"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	pm := NewProxyManager(db, bytes.Repeat([]byte{0x42}, 32))
	edges := newEdgeNodeRuntime(db, pm, ipMgr)
	shares := newShareUserRuntime(db, pm)
	pm.SetEdgeRuntime(edges)
	pm.shares = shares

	app := &App{db: db, pm: pm, ip2region: ipMgr}
	return app, edges, shares
}

func TestWaterfallBalance10ViewersAndMasterPeer(t *testing.T) {
	app, edges, shares := setupWaterfallBalanceTestApp(t)
	nat1, _ := insertTestEdgeNode(t, edges, "优化NAT-1", "203.0.113.11", 19090)
	nat2, _ := insertTestEdgeNode(t, edges, "优化NAT-2", "203.0.113.12", 19090)

	// Configure ct group: #1 NAT-1 (cap=2), #2 NAT-2 (cap=2), #3 Master (cap=2), strategy=balance.
	body := fmt.Sprintf(
		`{"group":"ct","strategy":"balance","nodes":[{"node_id":%d,"enabled":true,"max_balance_streams":2},{"node_id":%d,"enabled":true,"max_balance_streams":2},{"node_id":0,"enabled":true,"max_balance_streams":2}]}`,
		nat1, nat2,
	)
	rec := httptest.NewRecorder()
	app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/isp-routes status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Simulate 10 distinct watching device sessions arriving sequentially.
	wantOrder := []int64{
		nat1, nat1, // 1, 2 -> #1 (2/2)
		nat2, nat2, // 3, 4 -> #2 (2/2)
		0, 0, // 5, 6 -> #3 Master (2/2)
		nat1, // 7 -> #1 (3/2)
		nat2, // 8 -> #2 (3/2)
		0,    // 9 -> #3 Master (3/2)
		nat1, // 10 -> #1 (4/2)
	}
	gotOrder := make([]int64, 0, 10)
	for i := 1; i <= 10; i++ {
		tok := fmt.Sprintf("session-tok-%d", i)
		d := edges.selectNodeForSessionDispatch("1.2.3.4", tok)
		gotOrder = append(gotOrder, d.NodeID)
		if d.NodeID == 0 {
			shares.markMasterPlayback(tok, d.NodeName)
		} else {
			shares.markEdgePlayback(tok, d.NodeID, d.NodeName)
		}
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("viewer #%d routed to %d, want %d (full got=%v want=%v)", i+1, gotOrder[i], wantOrder[i], gotOrder, wantOrder)
		}
	}

	// Sticky check: viewer #3 (on nat2) requesting another chunk stays on nat2!
	if d := edges.selectNodeForSessionDispatch("1.2.3.4", "session-tok-3"); d.NodeID != nat2 {
		t.Fatalf("sticky playback for session-tok-3 moved to %d, want %d", d.NodeID, nat2)
	}

	// Release 2 sessions from nat2 (sessions 3 & 4) so nat2 drops from 3/2 -> 1/2 (< cap 2).
	stopReq := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Stopped", nil)
	shares.beginRequestActivity("session-tok-3", stopReq)()
	shares.beginRequestActivity("session-tok-4", stopReq)()

	// Viewer #11 must refill nat2 (1/2 -> 2/2) rather than overloading nat1!
	if d := edges.selectNodeForSessionDispatch("1.2.3.4", "session-tok-11"); d.NodeID != nat2 {
		t.Fatalf("viewer #11 after nat2 freed slot routed to %d, want %d", d.NodeID, nat2)
	}
}

func TestWaterfallBalanceGlobalCapAcrossISPsAndValidation(t *testing.T) {
	app, edges, shares := setupWaterfallBalanceTestApp(t)
	nat1, _ := insertTestEdgeNode(t, edges, "共享NAT", "203.0.113.21", 19090)

	// Set both ct and cu to balance with #1 nat1 (cap=2), #2 Master (cap=2).
	for _, group := range []string{"ct", "cu"} {
		body := fmt.Sprintf(
			`{"group":%q,"strategy":"balance","nodes":[{"node_id":%d,"enabled":true,"max_balance_streams":2},{"node_id":0,"enabled":true,"max_balance_streams":2}]}`,
			group, nat1,
		)
		rec := httptest.NewRecorder()
		app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s status=%d body=%s", group, rec.Code, rec.Body.String())
		}
	}

	// 1 Telecom viewer + 1 Unicom viewer fill nat1 globally to 2/2.
	d1 := edges.selectNodeForSessionDispatch("1.2.3.4", "ct-user-1")
	if d1.NodeID != nat1 {
		t.Fatalf("ct-user-1 got %d, want %d", d1.NodeID, nat1)
	}
	shares.markEdgePlayback("ct-user-1", d1.NodeID, d1.NodeName)

	d2 := edges.selectNodeForSessionDispatch("2.3.4.5", "cu-user-1")
	if d2.NodeID != nat1 {
		t.Fatalf("cu-user-1 got %d, want %d", d2.NodeID, nat1)
	}
	shares.markEdgePlayback("cu-user-1", d2.NodeID, d2.NodeName)

	// Next Unicom viewer must see nat1 is globally 2/2 and overflow to Master (0).
	d3 := edges.selectNodeForSessionDispatch("2.3.4.6", "cu-user-2")
	if d3.NodeID != 0 {
		t.Fatalf("cu-user-2 expected overflow to Master (0), got %d", d3.NodeID)
	}

	// Validation: cap=0 or cap=100 in PUT /api/isp-routes must return 400.
	for _, badCap := range []int{0, 100} {
		badBody := fmt.Sprintf(`{"group":"ct","nodes":[{"node_id":%d,"enabled":true,"max_balance_streams":%d}]}`, nat1, badCap)
		rec := httptest.NewRecorder()
		app.handleISPRoutes(rec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(badBody)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cap=%d status=%d, want 400", badCap, rec.Code)
		}
	}

	// Verify GET /api/isp-routes returns max_balance_streams=2 and live active_sessions=2 on nat1.
	rec := httptest.NewRecorder()
	app.handleISPRoutes(rec, httptest.NewRequest(http.MethodGet, "/api/isp-routes", nil))
	var payload struct {
		Groups []ispRouteGroupView `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode isp-routes: %v", err)
	}
	found := false
	for _, g := range payload.Groups {
		if g.Group != "ct" {
			continue
		}
		for _, n := range g.Nodes {
			if n.NodeID == nat1 {
				found = true
				if n.MaxBalanceStreams != 2 || n.ActiveSessions != 2 {
					t.Fatalf("ct nat1 view = (cap=%d, active=%d), want (2, 2)", n.MaxBalanceStreams, n.ActiveSessions)
				}
			}
		}
	}
	if !found {
		t.Fatalf("nat1 not found in ct group")
	}
}
