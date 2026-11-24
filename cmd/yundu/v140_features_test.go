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
	"fmt"
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
	"sync/atomic"
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

	// 1. Verify DoH ECS IPv4 answer parsing, DoH cache/resolution, error prefix stripping, and multi-IP Volcengine TOS failover.
	parsedDoH := parseDoHAnswerIPv4([]byte(`{
		"Answer":[
			{"type":5,"data":"tos20-cp-public.cn-beijing.tos.volces.com."},
			{"type":1,"data":"127.0.0.1"},
			{"type":1,"data":"10.0.0.1"},
			{"type":1,"data":"180.213.185.21"},
			{"type":1,"data":"180.213.185.21"},
			{"type":1,"data":"180.213.185.40"},
			{"type":28,"data":"240e:100::1"}
		]
	}`))
	if len(parsedDoH) != 2 || parsedDoH[0] != "180.213.185.21" || parsedDoH[1] != "180.213.185.40" {
		t.Fatalf("unexpected parsedDoH IPs: %+v", parsedDoH)
	}
	if gotErr := conciseSpeedtestError(`Put "https://probe-bucket-beijing.tos-cn-beijing.volces.com/upload/yundu-123": dial tcp4 180.213.185.21:443: i/o timeout`); gotErr != "dial tcp4 180.213.185.21:443: i/o timeout" {
		t.Fatalf("conciseSpeedtestError stripped unexpected result: %q", gotErr)
	}

	var dohHits atomic.Int32
	var catalogHits atomic.Int32
	dohSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/getNodes" {
			catalogHits.Add(1)
			if r.URL.Query().Get("format") != "tsv" || r.URL.Query().Get("scope") != "tos" {
				t.Error("unexpected TOS catalog query")
			}
			_, _ = io.WriteString(w, strings.Join([]string{
				"tos\t4\t北京\t电信\tnode.example\t127.0.0.1\t443\ttos-cn-beijing.volces.com",
				"tos\t4\t浦东新区\tCT\tnode.example\t180.213.185.50\t443\ttos-cn-shanghai.volces.com",
				"tos\t4\t北京市\tCT\tnode.example\t180.213.185.61\t443\ttos-cn-beijing.volces.com",
				"tos\t4\t北京\t电信\tnode.example\t180.213.185.62\t443\ttos-cn-beijing.volces.com",
				"tos\t4\t北京\t电信\tnode.example\t180.213.185.63\t443\ttos-cn-beijing.volces.com",
				"tos\t4\t北京\t电信\tnode.example\t180.213.185.21\t443\ttos-cn-beijing.volces.com",
			}, "\r\n"))
			return
		}
		dohHits.Add(1)
		w.Header().Set("Content-Type", "application/dns-json")
		_, _ = io.WriteString(w, `{"Answer":[{"type":1,"data":"180.213.185.21"}]}`)
	}))
	defer dohSrv.Close()
	app.speedtest.dohClient = dohSrv.Client()
	app.speedtest.dohEndpoints = []string{dohSrv.URL}
	app.speedtest.tosCatalogURL = dohSrv.URL + "/getNodes?format=tsv&scope=tos"
	if catalog := app.speedtest.loadTOSCatalog(context.Background()); len(catalog["bj-ct"]) != 4 || catalog["bj-ct"][0] != "180.213.185.61" || catalog["bj-ct"][3] != "180.213.185.21" {
		t.Fatalf("unexpected vetted TOS catalog: %+v", catalog)
	} else if sh := catalog["sh-ct"]; len(sh) != 1 || sh[0] != "180.213.185.50" {
		t.Fatalf("expected the Shanghai endpoint under Shanghai even with a display city label: %+v", catalog)
	}
	if firstIPs := app.speedtest.resolveVolcengineTOSIPs(context.Background(), volcengineTOSBeijingHost, "219.141.136.0/24"); len(firstIPs) != 1 || firstIPs[0] != "180.213.185.21" {
		t.Fatalf("resolveVolcengineTOSIPs returned %+v", firstIPs)
	}
	// Second call must hit in-memory TTL cache without re-querying DoH server.
	if secondIPs := app.speedtest.resolveVolcengineTOSIPs(context.Background(), volcengineTOSBeijingHost, "219.141.136.0/24"); len(secondIPs) != 1 || dohHits.Load() != 1 {
		t.Fatalf("expected cached DoH resolution (hits=%d), got %+v", dohHits.Load(), secondIPs)
	}

	var bjTOSAttempts, failedTOSDials, cleanedTOSUploads atomic.Int32
	var interruptTOSDownload atomic.Bool
	chunk := bytes.Repeat([]byte("SPEEDTEST-PAYLOAD-"), 4096)
	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/upload/yundu-") {
			if r.Method == http.MethodDelete {
				cleanedTOSUploads.Add(1)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method != http.MethodPut || r.ContentLength <= 0 {
				http.Error(w, "invalid TOS PUT upload", http.StatusBadRequest)
				return
			}
			// Reject the first reachable Beijing upload so the next candidate is used.
			if strings.EqualFold(r.Host, volcengineTOSBeijingHost) && bjTOSAttempts.Add(1) == 1 {
				http.Error(w, "upstream overloaded", http.StatusBadGateway)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		switch r.URL.Path {
		case "/download/test":
			if r.Header.Get("Range") == "bytes=0-0" {
				w.Header().Set("Content-Range", "bytes 0-0/1048576000")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte{0})
				return
			}
			if r.Header.Get("Range") != tosSpeedtestDownloadRange {
				http.Error(w, "missing expected TOS Range header", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/1048576000", 8*len(chunk)-1))
			w.Header().Set("Content-Length", strconv.Itoa(8*len(chunk)))
			w.WriteHeader(http.StatusPartialContent)
			if interruptTOSDownload.Load() {
				_, _ = w.Write(chunk)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
				return
			}
			for i := 0; i < 8; i++ {
				_, _ = w.Write(chunk)
			}
		case "/api/v1/gm/slurp":
			if r.Method != http.MethodPut || r.Header.Get("Upload-Draft-Interop-Version") != "6" {
				http.Error(w, "invalid apple cdn upload headers", http.StatusBadRequest)
				return
			}
			// Drain continuously for ~120ms (over >256 KiB) so upload reader timestamps reflect sustained transfer before 413.
			slurpDeadline := time.Now().Add(120 * time.Millisecond)
			for {
				_, readErr := io.CopyN(io.Discard, r.Body, 512<<10)
				if readErr != nil || !time.Now().Before(slurpDeadline) {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case "/instant413":
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case "/up":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case "/down", "/api/v1/gm/large":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 8; i++ {
				_, _ = w.Write(chunk)
			}
		default:
			http.NotFound(w, r)
		}
	})

	targetSrv := httptest.NewUnstartedServer(targetHandler)
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
		DNSNames: []string{
			appleSpeedtestHost,
			volcengineTOSBeijingHost,
			volcengineTOSShanghaiHost,
			volcengineTOSGuangzhouHost,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
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
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			switch host {
			case "180.213.185.61", "180.213.185.62", "180.213.185.63":
				failedTOSDials.Add(1)
				return nil, &net.OpError{Op: "dial", Net: network, Err: context.DeadlineExceeded}
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", targetSrv.Listener.Addr().String())
		},
	}
	mockTransport.TLSClientConfig.RootCAs = pool

	t.Run("TOS deadline keeps transferred bytes but parent cancellation rejects them", func(t *testing.T) {
		interruptTOSDownload.Store(true)
		defer interruptTOSDownload.Store(false)
		province, _ := lookupSpeedtestProvince("bj")
		downloadTarget := newTOSTarget(province, ispGroupCT, []string{"180.213.185.21"}, nil, time.Second)
		const duration = 300 * time.Millisecond
		started := time.Now()
		bps, _, _, err := measureSingleDownload(context.Background(), downloadTarget, duration, mockTransport)
		if err != nil || bps <= 0 || time.Since(started) > 2*time.Second {
			t.Fatalf("expected a partial sample at the TOS deadline, got %d B/s after %s, err=%v", bps, time.Since(started), err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), duration)
		defer cancel()
		bps, _, _, err = measureSingleDownload(ctx, downloadTarget, 5*time.Second, mockTransport)
		if err == nil || bps != 0 {
			t.Fatalf("parent cancellation produced a sample, got %d B/s, err=%v", bps, err)
		}
	})

	app.speedtest.dohResolver = func(_ context.Context, _ string, _ string) ([]string, error) {
		return []string{"180.213.185.40", "180.213.185.21"}, nil
	}
	app.speedtest.targetTransport = mockTransport

	// 2. Test SSRF rejection and custom target CRUD API.
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

	// 3. Start a job with three unreachable candidates, a rejected upload, and a healthy fallback.
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
	if startResp.Job.TotalTargets != 2 || len(startResp.Job.Results) != 2 {
		t.Fatalf("master job total_targets=%d, results=%d, want 2 (1 builtin + 1 custom)", startResp.Job.TotalTargets, len(startResp.Job.Results))
	}
	if catalogHits.Load() != 2 || startResp.Job.Results[0].ResolvedIP != "180.213.185.61" {
		t.Fatalf("expected the live catalog address before DoH candidates: %+v", startResp.Job.Results)
	}

	waitSpeedtestJob := func(jobID string) speedtestJobView {
		var out speedtestJobView
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			jobRec := httptest.NewRecorder()
			app.handleSpeedtestJobByID(jobRec, httptest.NewRequest(http.MethodGet, "/api/speedtest/jobs/"+jobID, nil))
			var jr struct {
				Job speedtestJobView `json:"job"`
			}
			if err := json.Unmarshal(jobRec.Body.Bytes(), &jr); err == nil {
				out = jr.Job
				if out.Status == "done" || out.Status == "failed" {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
		}
		return out
	}

	masterDone := waitSpeedtestJob(startResp.Job.ID)
	if masterDone.Status != "done" || masterDone.CompletedTargets != 2 {
		t.Fatalf("master job did not complete cleanly: %+v", masterDone)
	}
	for _, r := range masterDone.Results {
		if r.Status != "done" || r.ReturnBps <= 0 || r.OutboundBps <= 0 || r.ReturnRTTMs <= 0 {
			t.Fatalf("unexpected master result row: %+v", r)
		}
		if r.TargetKind == "builtin" {
			if r.ResolvedNode != "180.213.185.40" || r.ResolvedIP != "180.213.185.40" {
				t.Fatalf("expected failover to usable TOS IP (180.213.185.40), got %+v", r)
			}
		}
	}
	if failedTOSDials.Load() != 3 || bjTOSAttempts.Load() != 2 || cleanedTOSUploads.Load() != 1 {
		t.Fatalf("expected 3 probe failures, 2 measured uploads and 1 cleanup; got %d, %d, %d",
			failedTOSDials.Load(), bjTOSAttempts.Load(), cleanedTOSUploads.Load())
	}
	if metrics := rt.nodeSpeedMetricsCopy(0); len(metrics) != 2 {
		t.Fatalf("master persisted metrics = %+v, want ct and cu", metrics)
	}

	// Test Apple CDN default-route mode (PUT + HTTP 413 partial tolerance).
	appleStartRec := httptest.NewRecorder()
	app.handleSpeedtestStart(appleStartRec, httptest.NewRequest(http.MethodPost, "/api/speedtest/start", strings.NewReader(`{"node_id":0,"mode":"apple"}`)))
	if appleStartRec.Code != http.StatusCreated {
		t.Fatalf("start apple job status=%d body=%s", appleStartRec.Code, appleStartRec.Body.String())
	}
	var appleStartResp struct {
		Job speedtestJobView `json:"job"`
	}
	_ = json.Unmarshal(appleStartRec.Body.Bytes(), &appleStartResp)
	appleDone := waitSpeedtestJob(appleStartResp.Job.ID)
	if appleDone.Status != "done" || len(appleDone.Results) != 1 || appleDone.Results[0].ReturnBps <= 0 || appleDone.Results[0].OutboundBps <= 0 {
		t.Fatalf("apple mode job failed: %+v", appleDone)
	}

	// Delete custom target cleanly.
	delRec := httptest.NewRecorder()
	app.handleSpeedtestCustomTargetByID(delRec, httptest.NewRequest(http.MethodDelete, "/api/speedtest/custom-targets/"+itoa64(createResp.ID), nil))
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete custom target status=%d body=%s", delRec.Code, delRec.Body.String())
	}

	// 4. Test Edge Agent heartbeat speedtest task dispatch + execution + metrics persistence + direct HTTP popup.
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
	// Kick a heartbeat so agent receives task immediately without waiting 5s interval.
	if err := agent.sendHeartbeat(ctx); err != nil {
		t.Fatalf("initial agent heartbeat: %v", err)
	}

	edgeDone := waitSpeedtestJob(edgeStartResp.Job.ID)
	if edgeDone.Status != "done" || edgeDone.CompletedTargets != 1 {
		t.Fatalf("edge speedtest job failed: %+v", edgeDone)
	}
	if edgeMetrics := rt.nodeSpeedMetricsCopy(edgeID); len(edgeMetrics) != 1 || edgeMetrics[0].ISPGroup != "cm" {
		t.Fatalf("edge persisted metrics = %+v, want 1 cm row", edgeMetrics)
	}

	// Reject immediate HTTP 413 responses that did not sustain minimum bytes/elapsed duration.
	if _, _, _, _, upErr := measureSingleUpload(ctx, agentSpeedtestTarget{
		TargetKey: "instant413",
		Label:     "instant413",
		UploadURL: "https://" + appleSpeedtestHost + "/instant413",
		Method:    http.MethodPut,
	}, 200*time.Millisecond, mockTransport); upErr == nil {
		t.Fatalf("expected measureSingleUpload to reject immediate HTTP 413 with 0 reads")
	}

	// Verify /api/speedtest/targets exposes exactly the 3 Volcengine TOS regions (9 targets).
	targetsRec := httptest.NewRecorder()
	app.handleSpeedtestTargets(targetsRec, httptest.NewRequest(http.MethodGet, "/api/speedtest/targets", nil))
	if targetsRec.Code != http.StatusOK {
		t.Fatalf("targets status=%d body=%s", targetsRec.Code, targetsRec.Body.String())
	}
	var targetsResp struct {
		Provinces    []speedtestProvinceDef `json:"provinces"`
		CatalogCount int                    `json:"catalog_count"`
	}
	if err := json.Unmarshal(targetsRec.Body.Bytes(), &targetsResp); err != nil || len(targetsResp.Provinces) != 3 || targetsResp.CatalogCount != 9 {
		t.Fatalf("unexpected speedtest targets metadata: %s", targetsRec.Body.String())
	}

	// Verify SSRF guards reject NAT64, zoned link-local/site-local, loopback IPs and block dialing them in production client.
	for _, blockedIP := range []string{"127.0.0.1", "64:ff9b::1", "64:ff9b:1::1", "fe80::1%eth0", "fec0::1%eth0"} {
		addr := netip.MustParseAddr(blockedIP)
		if isPublicSpeedtestIP(addr) {
			t.Fatalf("isPublicSpeedtestIP(%q) = true, want false", blockedIP)
		}
	}
	for _, allowedIP := range []string{"1.1.1.1", "240e:100::1"} {
		if !isPublicSpeedtestIP(netip.MustParseAddr(allowedIP)) {
			t.Fatalf("isPublicSpeedtestIP(%q) = false, want true", allowedIP)
		}
	}
	for _, ssrfTarget := range []struct {
		ip   string
		host string
	}{
		{"127.0.0.1", "localhost"},
		{"64:ff9b:1::1", "nat64.example.com"},
	} {
		ssrfClient, _ := newSpeedtestSingleConnHTTPClient(ssrfTarget.ip, ssrfTarget.host, 500*time.Millisecond, nil)
		if resp, ssrfErr := ssrfClient.Get("http://" + ssrfTarget.host + ":8080/up"); ssrfErr == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected production single-conn client to block %s (%s)", ssrfTarget.ip, ssrfTarget.host)
		}
	}

	// Verify /api/speedtest/direct-targets exposes popup_url + report_id + supports_popup for HTTP edge node.
	dtReq := httptest.NewRequest(http.MethodGet, "https://panel.example/api/speedtest/direct-targets", nil)
	dtReq.Header.Set("X-Forwarded-Proto", "https")
	dtRec := httptest.NewRecorder()
	app.handleSpeedtestDirectTargets(dtRec, dtReq)
	if dtRec.Code != http.StatusOK {
		t.Fatalf("direct-targets status=%d body=%s", dtRec.Code, dtRec.Body.String())
	}
	var dtResp struct {
		Targets []struct {
			NodeID        int64  `json:"node_id"`
			Scheme        string `json:"scheme"`
			PopupURL      string `json:"popup_url"`
			ReportID      string `json:"report_id"`
			SupportsPopup bool   `json:"supports_popup"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(dtRec.Body.Bytes(), &dtResp); err != nil || len(dtResp.Targets) != 2 {
		t.Fatalf("unexpected direct-targets response: %s", dtRec.Body.String())
	}
	edgeDT := dtResp.Targets[1]
	if !edgeDT.SupportsPopup || edgeDT.PopupURL == "" || edgeDT.ReportID == "" || edgeDT.Scheme != "http" {
		t.Fatalf("expected HTTP edge popup metadata, got %+v", edgeDT)
	}

	// Verify node_id filter on /api/speedtest/direct-targets.
	dtFiltReq := httptest.NewRequest(http.MethodGet, "https://panel.example/api/speedtest/direct-targets?node_id="+itoa64(edgeID), nil)
	dtFiltReq.Header.Set("X-Forwarded-Proto", "https")
	dtFiltRec := httptest.NewRecorder()
	app.handleSpeedtestDirectTargets(dtFiltRec, dtFiltReq)
	if dtFiltRec.Code != http.StatusOK {
		t.Fatalf("filtered direct-targets status=%d body=%s", dtFiltRec.Code, dtFiltRec.Body.String())
	}
	var dtFiltResp struct {
		Targets []struct {
			NodeID int64 `json:"node_id"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(dtFiltRec.Body.Bytes(), &dtFiltResp); err != nil || len(dtFiltResp.Targets) != 1 || dtFiltResp.Targets[0].NodeID != edgeID {
		t.Fatalf("unexpected filtered direct-targets response: %s", dtFiltRec.Body.String())
	}

	// Verify rolling-upgrade heartbeat compatibility: new agent sends X-Yundu-Direct-Popup header without breaking legacy DisallowUnknownFields masters.
	var capturedHBHeaders http.Header
	var capturedHBBody []byte
	hbProbeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHBHeaders = r.Header.Clone()
		capturedHBBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "node_enabled": true})
	}))
	defer hbProbeSrv.Close()
	hbProbeAgent := newEdgeAgent(edgeAgentConfig{
		MasterURL: hbProbeSrv.URL,
		NodeID:    edgeID,
		Token:     edgeToken,
		Port:      19090,
	})
	if err := hbProbeAgent.sendHeartbeat(ctx); err != nil {
		t.Fatalf("hbProbeAgent.sendHeartbeat: %v", err)
	}
	if capturedHBHeaders.Get("X-Yundu-Direct-Popup") != "1" {
		t.Fatalf("missing X-Yundu-Direct-Popup: 1 header on agent heartbeat: %+v", capturedHBHeaders)
	}
	type legacyV140Heartbeat struct {
		Version           string          `json:"version"`
		ListenPort        int             `json:"listen_port"`
		TLSActive         bool            `json:"tls_active"`
		ActiveStreams     int             `json:"active_streams"`
		CurrentBps        int64           `json:"current_bps"`
		MemoryBytes       uint64          `json:"memory_bytes"`
		UptimeSec         int64           `json:"uptime_sec"`
		SupportsSpeedtest bool            `json:"supports_speedtest,omitempty"`
		UpstreamLatencies map[int64]int   `json:"upstream_latencies,omitempty"`
		SpeedtestProgress json.RawMessage `json:"speedtest_progress,omitempty"`
		UpgradeRequestID  string          `json:"upgrade_request_id,omitempty"`
		UpgradeStatus     string          `json:"upgrade_status,omitempty"`
		CertFingerprint   string          `json:"cert_fingerprint"`
		TrafficReports    []any           `json:"traffic_reports"`
	}
	var legacyHB legacyV140Heartbeat
	legacyDec := json.NewDecoder(bytes.NewReader(capturedHBBody))
	legacyDec.DisallowUnknownFields()
	if err := legacyDec.Decode(&legacyHB); err != nil {
		t.Fatalf("new agent heartbeat body rejected by legacy DisallowUnknownFields master: %v; body=%s", err, string(capturedHBBody))
	}
	legacyNodeID, legacyNodeToken := insertTestEdgeNode(t, rt, "LegacyEdge", "203.0.113.31", 19091)
	sendRawHeartbeat := func(withPopupHeader bool) bool {
		req := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", bytes.NewReader(capturedHBBody))
		req.Header.Set("X-Yundu-Node-ID", itoa64(legacyNodeID))
		req.Header.Set("X-Yundu-Node-Token", legacyNodeToken)
		if withPopupHeader {
			req.Header.Set("X-Yundu-Direct-Popup", "1")
		}
		rec := httptest.NewRecorder()
		app.handleAgentHeartbeat(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("handleAgentHeartbeat(%v) status=%d body=%s", withPopupHeader, rec.Code, rec.Body.String())
		}
		qReq := httptest.NewRequest(http.MethodGet, "https://panel.example/api/speedtest/direct-targets?node_id="+itoa64(legacyNodeID), nil)
		qReq.Header.Set("X-Forwarded-Proto", "https")
		qRec := httptest.NewRecorder()
		app.handleSpeedtestDirectTargets(qRec, qReq)
		var qResp struct {
			Targets []struct {
				SupportsPopup bool `json:"supports_popup"`
			} `json:"targets"`
		}
		if err := json.Unmarshal(qRec.Body.Bytes(), &qResp); err != nil || len(qResp.Targets) != 1 {
			t.Fatalf("unexpected direct-targets for legacy node: %s", qRec.Body.String())
		}
		return qResp.Targets[0].SupportsPopup
	}
	if sendRawHeartbeat(false) {
		t.Fatalf("expected supports_popup=false for heartbeat without X-Yundu-Direct-Popup")
	}
	if !sendRawHeartbeat(true) {
		t.Fatalf("expected supports_popup=true after heartbeat with X-Yundu-Direct-Popup: 1")
	}
	if sendRawHeartbeat(false) {
		t.Fatalf("expected supports_popup to reset to false on downgrade heartbeat")
	}

	// 5. Verify direct-stream auth on Master, direct HTTP popup HTML on Edge Agent, direct-report relay, and sealed ticket domain separation.
	unauthStream := httptest.NewRecorder()
	app.authMiddleware(app.handleSpeedtestDirectStream)(unauthStream, httptest.NewRequest(http.MethodGet, "/api/speedtest/direct-stream", nil))
	if unauthStream.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated master direct-stream status=%d, want 401", unauthStream.Code)
	}

	repID, repTok := app.speedtest.createDirectReportSession(edgeID)
	sealed, err := sealEdgeSpeedtestTicketWithReport(edgeToken, edgeID, 4<<20, "https://panel.example", repID, repTok)
	if err != nil {
		t.Fatalf("sealEdgeSpeedtestTicketWithReport: %v", err)
	}
	edgeHandler := agent.handler()

	popupUIRec := httptest.NewRecorder()
	edgeHandler.ServeHTTP(popupUIRec, httptest.NewRequest(http.MethodGet, edgeSpeedtestUIPathPrefix+sealed+"?bytes=1048576", nil))
	if popupUIRec.Code != http.StatusOK {
		t.Fatalf("edge popup UI status=%d body=%s", popupUIRec.Code, popupUIRec.Body.String())
	}
	if ct := popupUIRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("popup UI content-type=%q, want text/html", ct)
	}
	if uiCSP := popupUIRec.Header().Get("Content-Security-Policy"); !strings.Contains(uiCSP, "connect-src 'self' https://panel.example") {
		t.Fatalf("popup UI CSP=%q missing connect-src 'self' https://panel.example", uiCSP)
	}
	if uiBody := popupUIRec.Body.String(); !strings.Contains(uiBody, repID) || !strings.Contains(uiBody, "/api/speedtest/direct-report") {
		t.Fatalf("popup UI HTML missing embedded report ID or relay URL")
	}

	// Verify /api/speedtest/direct-report auth, progress->done transitions, terminal lock (409), and authenticated poll.
	postReport := func(token, phase string, ttfbMs int, mbps float64, receivedBytes int64) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{
			"id":             repID,
			"token":          token,
			"phase":          phase,
			"ttfb_ms":        ttfbMs,
			"mbps":           mbps,
			"received_bytes": receivedBytes,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/speedtest/direct-report", bytes.NewReader(payload))
		app.handleSpeedtestDirectReport(rec, req)
		return rec
	}
	if rec := postReport("wrong-token", "progress", 10, 12.5, 1024); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad report token status=%d, want 401", rec.Code)
	}
	if rec := postReport(repTok, "progress", 40, 98.5, 524288); rec.Code != http.StatusOK {
		t.Fatalf("progress report status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postReport(repTok, "done", 42, 123.4, 1048576); rec.Code != http.StatusOK {
		t.Fatalf("done report status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postReport(repTok, "progress", 50, 10, 1048576); rec.Code != http.StatusConflict {
		t.Fatalf("post-terminal report status=%d, want 409", rec.Code)
	}

	unauthPoll := httptest.NewRecorder()
	app.authMiddleware(app.handleSpeedtestDirectReportPoll)(unauthPoll, httptest.NewRequest(http.MethodGet, "/api/speedtest/direct-report/poll?id="+repID, nil))
	if unauthPoll.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated poll status=%d, want 401", unauthPoll.Code)
	}
	pollRec := httptest.NewRecorder()
	app.handleSpeedtestDirectReportPoll(pollRec, httptest.NewRequest(http.MethodGet, "/api/speedtest/direct-report/poll?id="+repID, nil))
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", pollRec.Code, pollRec.Body.String())
	}
	var pollResp struct {
		Report struct {
			ID       string  `json:"id"`
			Status   string  `json:"status"`
			TTFBMs   int     `json:"ttfb_ms"`
			Mbps     float64 `json:"mbps"`
			Terminal bool    `json:"terminal"`
		} `json:"report"`
	}
	if err := json.Unmarshal(pollRec.Body.Bytes(), &pollResp); err != nil || pollResp.Report.Status != "done" || !pollResp.Report.Terminal || pollResp.Report.Mbps != 123.4 {
		t.Fatalf("unexpected poll response: %s", pollRec.Body.String())
	}

	okDirectRec := httptest.NewRecorder()
	edgeHandler.ServeHTTP(okDirectRec, httptest.NewRequest(http.MethodGet, edgeSpeedtestPathPrefix+sealed+"?bytes=1048576", nil))
	if okDirectRec.Code != http.StatusOK || okDirectRec.Body.Len() != 1048576 {
		t.Fatalf("edge direct stream status=%d len=%d, want 200 and 1048576", okDirectRec.Code, okDirectRec.Body.Len())
	}
	if okDirectRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing CORS header on edge direct stream")
	}

	// Enforce cumulative JTI byte budget (2x MaxBytes = 8 MiB total).
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

	// Verify dynamic CSP allows edge origins while preserving strict Cross-Origin-Opener-Policy: same-origin.
	cspRec := httptest.NewRecorder()
	securityHeadersWithConnectSources(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), nil, rt.publicOrigins).ServeHTTP(cspRec, httptest.NewRequest(http.MethodGet, "https://panel.example/", nil))
	cspHeader := cspRec.Header().Get("Content-Security-Policy")
	if !strings.Contains(cspHeader, "connect-src 'self' http://203.0.113.30:19090") {
		t.Fatalf("CSP missing edge origin: %q", cspHeader)
	}
	if coop := cspRec.Header().Get("Cross-Origin-Opener-Policy"); coop != "same-origin" {
		t.Fatalf("COOP=%q, want same-origin", coop)
	}

	// Wrong endpoint domain or tampered token must fail with 401.
	playTicket, _ := sealEdgePlaybackTicket(edgeToken, edgePlaybackTicket{NodeID: edgeID, SiteID: 1, TargetURL: targetSrv.URL, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	crossRec1 := httptest.NewRecorder()
	edgeHandler.ServeHTTP(crossRec1, httptest.NewRequest(http.MethodGet, edgeSpeedtestPathPrefix+playTicket, nil))
	if crossRec1.Code != http.StatusUnauthorized {
		t.Fatalf("playback ticket accepted on speedtest route! status=%d", crossRec1.Code)
	}
	crossRecUI := httptest.NewRecorder()
	edgeHandler.ServeHTTP(crossRecUI, httptest.NewRequest(http.MethodGet, edgeSpeedtestUIPathPrefix+playTicket, nil))
	if crossRecUI.Code != http.StatusUnauthorized {
		t.Fatalf("playback ticket accepted on popup UI route! status=%d", crossRecUI.Code)
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
