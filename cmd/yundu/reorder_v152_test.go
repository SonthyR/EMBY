package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestShareUsersAndEdgeNodesReorder(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reorder.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x55}, 32))
	pm.SetEdgeRuntime(newEdgeNodeRuntime(db, pm, nil))
	app := &App{db: db, pm: pm}

	// Create a site so share users can be assigned to it.
	site, err := db.CreateSite("测试源站", 0, "https://emby.example.com", "", "proxy", "", "auto", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	// 1. Create 3 share users and verify default order is creation order.
	var userIDs []int64
	for _, name := range []string{"alice", "bob", "charlie"} {
		body, _ := json.Marshal(ShareUserInput{
			Username: name,
			Password: "password123",
			Sites:    []ShareUserSiteInput{{SiteID: site.ID}},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/share-users", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		app.handleShareUsers(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create share user %s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		var created ShareUser
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode share user: %v", err)
		}
		userIDs = append(userIDs, created.ID)
	}

	// Reorder share users to [charlie, alice, bob]
	wantUsers := []int64{userIDs[2], userIDs[0], userIDs[1]}
	reorderBody, _ := json.Marshal(map[string]any{"user_ids": wantUsers})
	rec := httptest.NewRecorder()
	app.handleShareUserReorder(rec, httptest.NewRequest(http.MethodPut, "/api/share-users/reorder", bytes.NewReader(reorderBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder share users status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Verify GET /api/share-users returns the new order.
	rec = httptest.NewRecorder()
	app.handleShareUsers(rec, httptest.NewRequest(http.MethodGet, "/api/share-users", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list share users status=%d", rec.Code)
	}
	var gotUsers []ShareUser
	if err := json.Unmarshal(rec.Body.Bytes(), &gotUsers); err != nil {
		t.Fatalf("decode share users: %v", err)
	}
	if len(gotUsers) != 3 || gotUsers[0].ID != wantUsers[0] || gotUsers[1].ID != wantUsers[1] || gotUsers[2].ID != wantUsers[2] {
		t.Fatalf("unexpected share user order: got [%d,%d,%d] want %v", gotUsers[0].ID, gotUsers[1].ID, gotUsers[2].ID, wantUsers)
	}

	// Validation: incomplete or duplicate user_ids must return 400.
	badBody, _ := json.Marshal(map[string]any{"user_ids": []int64{userIDs[0], userIDs[0], userIDs[1]}})
	rec = httptest.NewRecorder()
	app.handleShareUserReorder(rec, httptest.NewRequest(http.MethodPut, "/api/share-users/reorder", bytes.NewReader(badBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on duplicate user_ids, got %d", rec.Code)
	}

	// 2. Create 3 edge nodes and verify reorder persistence.
	var nodeIDs []int64
	for _, name := range []string{"HK", "JP", "US"} {
		body, _ := json.Marshal(EdgeNodeInput{
			Name:       name,
			PublicPort: 19090,
		})
		rec := httptest.NewRecorder()
		app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", bytes.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create edge node %s status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		var created EdgeNode
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode edge node: %v", err)
		}
		nodeIDs = append(nodeIDs, created.ID)
	}

	// Reorder edge nodes to [US, HK, JP]
	wantNodes := []int64{nodeIDs[2], nodeIDs[0], nodeIDs[1]}
	nodeReorderBody, _ := json.Marshal(map[string]any{"node_ids": wantNodes})
	rec = httptest.NewRecorder()
	app.handleEdgeNodeReorder(rec, httptest.NewRequest(http.MethodPut, "/api/edge-nodes/reorder", bytes.NewReader(nodeReorderBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder edge nodes status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Add a 4th node and ensure it appends at the end after custom order.
	body4, _ := json.Marshal(EdgeNodeInput{Name: "SG", PublicPort: 19090})
	rec = httptest.NewRecorder()
	app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", bytes.NewReader(body4)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create 4th edge node status=%d", rec.Code)
	}
	var node4 EdgeNode
	_ = json.Unmarshal(rec.Body.Bytes(), &node4)

	rec = httptest.NewRecorder()
	app.handleEdgeNodes(rec, httptest.NewRequest(http.MethodGet, "/api/edge-nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list edge nodes status=%d", rec.Code)
	}
	var gotNodes []EdgeNode
	if err := json.Unmarshal(rec.Body.Bytes(), &gotNodes); err != nil {
		t.Fatalf("decode edge nodes: %v", err)
	}
	wantAll := append(wantNodes, node4.ID)
	if len(gotNodes) != 4 || gotNodes[0].ID != wantAll[0] || gotNodes[1].ID != wantAll[1] || gotNodes[2].ID != wantAll[2] || gotNodes[3].ID != wantAll[3] {
		t.Fatalf("unexpected edge node order: got [%d,%d,%d,%d] want %v", gotNodes[0].ID, gotNodes[1].ID, gotNodes[2].ID, gotNodes[3].ID, wantAll)
	}

	// Validation: unknown node ID must return 400.
	badNodeBody, _ := json.Marshal(map[string]any{"node_ids": []int64{nodeIDs[0], nodeIDs[1], nodeIDs[2], 99999}})
	rec = httptest.NewRecorder()
	app.handleEdgeNodeReorder(rec, httptest.NewRequest(http.MethodPut, "/api/edge-nodes/reorder", bytes.NewReader(badNodeBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on unknown node_id, got %d", rec.Code)
	}
}
