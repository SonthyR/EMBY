package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func generateTestWildcardCertPEM(t *testing.T, baseDomain string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1001),
		Subject:      pkix.Name{CommonName: "*." + baseDomain},
		DNSNames:     []string{"*." + baseDomain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestEdgeAutoHTTPSEndToEnd(t *testing.T) {
	origEphemeral := jwtSecretEphemeral
	jwtSecretEphemeral = false
	defer func() { jwtSecretEphemeral = origEphemeral }()

	// 1. Mock Cloudflare API v4 server
	type cfRec struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
		TTL     int    `json:"ttl"`
	}
	var cfMu sync.Mutex
	records := map[string]cfRec{}
	nextRecID := 1
	deletedIDs := []string{}

	cfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf-test-token-1234567890abcdef" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"bad token"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"zone-123"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-123/dns_records":
			name := r.URL.Query().Get("name")
			typ := r.URL.Query().Get("type")
			cfMu.Lock()
			var matched []cfRec
			for _, rec := range records {
				if rec.Name == name && rec.Type == typ {
					matched = append(matched, rec)
				}
			}
			cfMu.Unlock()
			b, _ := json.Marshal(map[string]any{"success": true, "result": matched})
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-123/dns_records":
			var body cfRec
			_ = json.NewDecoder(r.Body).Decode(&body)
			cfMu.Lock()
			id := "rec-" + strconv.Itoa(nextRecID)
			nextRecID++
			body.ID = id
			records[id] = body
			cfMu.Unlock()
			b, _ := json.Marshal(map[string]any{"success": true, "result": body})
			_, _ = w.Write(b)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/zones/zone-123/dns_records/"):
			id := strings.TrimPrefix(r.URL.Path, "/zones/zone-123/dns_records/")
			var body cfRec
			_ = json.NewDecoder(r.Body).Decode(&body)
			cfMu.Lock()
			if _, ok := records[id]; !ok {
				cfMu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"not found"}]}`))
				return
			}
			body.ID = id
			records[id] = body
			cfMu.Unlock()
			b, _ := json.Marshal(map[string]any{"success": true, "result": body})
			_, _ = w.Write(b)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/zones/zone-123/dns_records/"):
			id := strings.TrimPrefix(r.URL.Path, "/zones/zone-123/dns_records/")
			cfMu.Lock()
			delete(records, id)
			deletedIDs = append(deletedIDs, id)
			cfMu.Unlock()
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"` + id + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false}`))
		}
	}))
	defer cfSrv.Close()

	// 2. Spin up Master App simulating OpenResty external proxy (panelBindLoopback=true, panelTLSEnabled=false)
	dbPath := filepath.Join(t.TempDir(), "edge_auto_https.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x66}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, nil)
	pm.SetEdgeRuntime(edgeRT)
	defer pm.GracefulShutdown(context.Background())

	autoMgr := newEdgeAutoHTTPSManager(db, dbPath, edgeRT)
	autoMgr.cfAPIBase = cfSrv.URL
	autoMgr.issuer = func(_ context.Context, _, _, baseDomain string) ([]byte, []byte, error) {
		cert, key := generateTestWildcardCertPEM(t, baseDomain)
		return cert, key, nil
	}

	app := &App{
		db:                db,
		dbPath:            dbPath,
		pm:                pm,
		edgeAutoHTTPS:     autoMgr,
		trustedProxies:    []*net.IPNet{loopbackCIDR},
		panelBindLoopback: true,
		panelTLSEnabled:   false,
	}

	// 3. Configure Edge Auto-HTTPS via PUT /api/edge-auto-https
	putReq := httptest.NewRequest(http.MethodPut, "/api/edge-auto-https", strings.NewReader(`{
		"enabled": true,
		"base_domain": "*.example.com",
		"subdomain_prefix": "nat",
		"acme_email": "ops@example.com",
		"cf_api_token": "cf-test-token-1234567890abcdef"
	}`))
	putRec := httptest.NewRecorder()
	app.handleEdgeAutoHTTPS(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/edge-auto-https status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	var st EdgeAutoHTTPSStatus
	if err := json.Unmarshal(putRec.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !st.Enabled || st.BaseDomain != "example.com" || st.WildcardDomain != "*.example.com" || st.SubdomainPrefix != "nat" || st.ZoneID != "zone-123" || !st.TokenConfigured {
		t.Fatalf("unexpected saved status: %+v", st)
	}

	// 4. Issue wildcard cert into isolated edge-tls/ dir — Master panel_settings.tls_enabled MUST remain false!
	issueReq := httptest.NewRequest(http.MethodPost, "/api/edge-auto-https/issue", strings.NewReader(`{}`))
	issueRec := httptest.NewRecorder()
	app.handleEdgeAutoHTTPSAction(issueRec, issueReq)
	if issueRec.Code != http.StatusOK {
		t.Fatalf("POST /api/edge-auto-https/issue status=%d body=%s", issueRec.Code, issueRec.Body.String())
	}
	if err := json.Unmarshal(issueRec.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal issued status: %v", err)
	}
	if !st.CertConfigured || !st.CertValid || !st.CertMatchesDomain || st.CertDaysRemaining < 80 {
		t.Fatalf("expected valid matching cert, got %+v", st)
	}
	if ps, err := db.PanelSettings(); err == nil && ps.TLSEnabled {
		t.Fatalf("expected Master panel TLSEnabled to remain false (OpenResty compatibility)")
	}

	// 5. Create an edge node with auto_https=true and empty public_host/public_ip
	createReq := httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{
		"name": "美西 NAT",
		"public_port": 19090,
		"auto_https": true
	}`))
	createRec := httptest.NewRecorder()
	app.handleEdgeNodes(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create auto_https edge node status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created EdgeNode
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created node: %v", err)
	}
	wantHost := "nat-" + strconv.FormatInt(created.ID, 10) + ".example.com"
	if !created.AutoHTTPS || created.PublicHost != wantHost || created.TLSMode != edgeNodeTLSModeAuto || created.CFDNSStatus != "pending_ip" {
		t.Fatalf("unexpected auto_https node: %+v", created)
	}

	// Grab plaintext node token for heartbeat
	edgeRT.mu.RLock()
	nodeToken := edgeRT.nodes[created.ID].tokenPlain
	edgeRT.mu.RUnlock()

	sendHeartbeat := func(clientIP string) agentHeartbeatResponse {
		hbReq := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", strings.NewReader(`{"version":"v1.5.1"}`))
		hbReq.RemoteAddr = "127.0.0.1:40000"
		hbReq.Header.Set("X-Real-IP", clientIP)
		hbReq.Header.Set("X-Yundu-Node-ID", strconv.FormatInt(created.ID, 10))
		hbReq.Header.Set("X-Yundu-Node-Token", nodeToken)
		hbRec := httptest.NewRecorder()
		app.handleAgentHeartbeat(hbRec, hbReq)
		if hbRec.Code != http.StatusOK {
			t.Fatalf("heartbeat status=%d body=%s", hbRec.Code, hbRec.Body.String())
		}
		var resp agentHeartbeatResponse
		if err := json.Unmarshal(hbRec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal heartbeat: %v", err)
		}
		return resp
	}

	// 6. First heartbeat from 203.0.113.55 -> receives wildcard cert PEM AND triggers CF A record upsert (proxied=false)
	hb1 := sendHeartbeat("203.0.113.55")
	if hb1.TLSCertPEM == "" || hb1.TLSKeyPEM == "" || hb1.CertFingerprint == "" {
		t.Fatalf("expected heartbeat to sync edge-tls wildcard cert, got fp=%q", hb1.CertFingerprint)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cfMu.Lock()
		n := len(records)
		cfMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cfMu.Lock()
	if len(records) != 1 {
		cfMu.Unlock()
		t.Fatalf("expected 1 CF DNS record after heartbeat, got %d", len(records))
	}
	var recID string
	for id, rec := range records {
		recID = id
		if rec.Type != "A" || rec.Name != wantHost || rec.Content != "203.0.113.55" || rec.Proxied || rec.TTL != 60 {
			cfMu.Unlock()
			t.Fatalf("unexpected CF record: %+v", rec)
		}
	}
	cfMu.Unlock()

	// 7. Second heartbeat from a changed IP (DDNS) -> updates the same CF record to 198.51.100.77
	_ = sendHeartbeat("198.51.100.77")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cfMu.Lock()
		cur := records[recID].Content
		cfMu.Unlock()
		if cur == "198.51.100.77" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cfMu.Lock()
	if records[recID].Content != "198.51.100.77" {
		cfMu.Unlock()
		t.Fatalf("expected DDNS update to 198.51.100.77, got %+v", records[recID])
	}
	cfMu.Unlock()

	// 8. Delete edge node -> cleans up CF DNS record automatically
	delReq := httptest.NewRequest(http.MethodDelete, "/api/edge-nodes/"+strconv.FormatInt(created.ID, 10), nil)
	delRec := httptest.NewRecorder()
	app.handleEdgeNodeByID(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE edge node status=%d body=%s", delRec.Code, delRec.Body.String())
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cfMu.Lock()
		n := len(deletedIDs)
		cfMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cfMu.Lock()
	if len(deletedIDs) != 1 || deletedIDs[0] != recID {
		cfMu.Unlock()
		t.Fatalf("expected CF record %s deleted on node removal, got %v", recID, deletedIDs)
	}
	cfMu.Unlock()
}
