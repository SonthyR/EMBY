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
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildSyntheticXDB builds a minimal valid ip2region v2 (IPv4) or v3 (IPv6) xdb
// file in memory for deterministic offline unit tests.
type testXDBSegment struct {
	start  netip.Addr
	end    netip.Addr
	region string
}

func buildSyntheticXDB(t *testing.T, ipVersion int, segments []testXDBSegment) []byte {
	t.Helper()
	var buf bytes.Buffer
	header := make([]byte, xdbHeaderLength)
	if ipVersion == xdbIPv4VersionNo {
		binary.LittleEndian.PutUint16(header[0:2], 2)
	} else {
		binary.LittleEndian.PutUint16(header[0:2], 3)
	}
	binary.LittleEndian.PutUint16(header[2:4], 1)
	binary.LittleEndian.PutUint32(header[4:8], uint32(time.Now().Unix()))
	binary.LittleEndian.PutUint16(header[16:18], uint16(ipVersion))
	if ipVersion == xdbIPv4VersionNo {
		binary.LittleEndian.PutUint16(header[18:20], 4)
	} else {
		binary.LittleEndian.PutUint16(header[18:20], 16)
	}
	buf.Write(header)

	// Vector index placeholder (256 * 256 * 8 = 512 KB)
	vecStart := buf.Len()
	vec := make([]byte, xdbVectorIndexBytes)
	buf.Write(vec)

	// Region strings block
	type regionRef struct {
		ptr uint32
		ln  uint16
	}
	refs := make([]regionRef, len(segments))
	for i, seg := range segments {
		ptr := uint32(buf.Len())
		b := []byte(seg.region)
		buf.Write(b)
		refs[i] = regionRef{ptr: ptr, ln: uint16(len(b))}
	}

	// Segment index block (grouped by first two bytes of start IP)
	segSize := xdbIPv4SegSize
	if ipVersion == xdbIPv6VersionNo {
		segSize = xdbIPv6SegSize
	}
	startIdxPtr := uint32(buf.Len())
	type bucketRange struct {
		sPtr uint32
		ePtr uint32
	}
	buckets := map[int]bucketRange{}
	for i, seg := range segments {
		offset := uint32(buf.Len())
		var sb, eb []byte
		if ipVersion == xdbIPv4VersionNo {
			a4 := seg.start.As4()
			b4 := seg.end.As4()
			sb = a4[:]
			eb = b4[:]
			// v2 xdb stores IPv4 addresses as little-endian uint32.
			var rec [14]byte
			binary.LittleEndian.PutUint32(rec[0:4], binary.BigEndian.Uint32(sb))
			binary.LittleEndian.PutUint32(rec[4:8], binary.BigEndian.Uint32(eb))
			binary.LittleEndian.PutUint16(rec[8:10], refs[i].ln)
			binary.LittleEndian.PutUint32(rec[10:14], refs[i].ptr)
			buf.Write(rec[:])
		} else {
			a16 := seg.start.As16()
			b16 := seg.end.As16()
			sb = a16[:]
			eb = b16[:]
			var rec [38]byte
			copy(rec[0:16], sb)
			copy(rec[16:32], eb)
			binary.LittleEndian.PutUint16(rec[32:34], refs[i].ln)
			binary.LittleEndian.PutUint32(rec[34:38], refs[i].ptr)
			buf.Write(rec[:])
		}
		key := int(sb[0])*256 + int(sb[1])
		r, ok := buckets[key]
		if !ok {
			buckets[key] = bucketRange{sPtr: offset, ePtr: offset}
		} else {
			r.ePtr = offset
			buckets[key] = r
		}
	}
	endIdxPtr := uint32(buf.Len() - segSize)

	raw := buf.Bytes()
	binary.LittleEndian.PutUint32(raw[8:12], startIdxPtr)
	binary.LittleEndian.PutUint32(raw[12:16], endIdxPtr)
	for key, r := range buckets {
		off := vecStart + key*8
		binary.LittleEndian.PutUint32(raw[off:off+4], r.sPtr)
		binary.LittleEndian.PutUint32(raw[off+4:off+8], r.ePtr)
	}
	return raw
}

func TestIP2RegionXDBv4Andv6AndISPClassification(t *testing.T) {
	v4Data := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|0|广东省|深圳市|电信"},
		{start: netip.MustParseAddr("5.6.7.0"), end: netip.MustParseAddr("5.6.7.255"), region: "中国|0|上海市|上海市|联通"},
		{start: netip.MustParseAddr("9.10.11.0"), end: netip.MustParseAddr("9.10.11.255"), region: "中国|0|北京市|北京市|移动"},
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), region: "美国|0|0|0|Google"},
	})
	v6Data := buildSyntheticXDB(t, xdbIPv6VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("240e:100::"), end: netip.MustParseAddr("240e:100::ffff"), region: "中国|0|广东省|广州市|电信"},
		{start: netip.MustParseAddr("2408:200::"), end: netip.MustParseAddr("2408:200::ffff"), region: "中国|0|浙江省|杭州市|联通"},
		{start: netip.MustParseAddr("2409:300::"), end: netip.MustParseAddr("2409:300::ffff"), region: "中国|0|江苏省|南京市|移动"},
	})

	mirror1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mirror unavailable", http.StatusBadGateway)
	}))
	defer mirror1.Close()

	mirror2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "ip2region_v4.xdb"):
			_, _ = w.Write(v4Data)
		case strings.HasSuffix(r.URL.Path, "ip2region_v6.xdb"):
			_, _ = w.Write(v6Data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mirror2.Close()

	mgr := newIP2RegionManager(t.TempDir(), mirror2.Client())
	defer mgr.Close()
	mgr.mirrors = []string{mirror1.URL + "/", mirror2.URL + "/"}

	st := mgr.Status()
	if st.V4.Downloaded || st.V6.Downloaded {
		t.Fatalf("expected empty status initially, got %+v", st)
	}

	itemV4, err := mgr.Update(context.Background(), "v4")
	if err != nil {
		t.Fatalf("Update v4 failed: %v", err)
	}
	if !itemV4.Downloaded || itemV4.SizeBytes != int64(len(v4Data)) {
		t.Fatalf("unexpected v4 item: %+v", itemV4)
	}
	itemV6, err := mgr.Update(context.Background(), "v6")
	if err != nil {
		t.Fatalf("Update v6 failed: %v", err)
	}
	if !itemV6.Downloaded || itemV6.SizeBytes != int64(len(v6Data)) {
		t.Fatalf("unexpected v6 item: %+v", itemV6)
	}

	cases := []struct {
		ip       string
		wantGrp  string
		wantDisp string
	}{
		{"1.2.3.4", ispGroupCT, "中国 · 广东省 · 深圳市 · 电信"},
		{"5.6.7.8", ispGroupCU, "中国 · 上海市 · 联通"},
		{"9.10.11.12", ispGroupCM, "中国 · 北京市 · 移动"},
		{"8.8.8.8", ispGroupOther, "美国 · Google"},
		{"240e:100::42", ispGroupCT, "中国 · 广东省 · 广州市 · 电信"},
		{"2408:200::1", ispGroupCU, "中国 · 浙江省 · 杭州市 · 联通"},
		{"2409:300::9", ispGroupCM, "中国 · 江苏省 · 南京市 · 移动"},
	}
	for _, tc := range cases {
		res := mgr.Lookup(tc.ip)
		if !res.Found || res.ISPGroup != tc.wantGrp {
			t.Errorf("Lookup(%s) = %+v, want group %q", tc.ip, res, tc.wantGrp)
		}
		disp, ok := mgr.LookupDisplay(tc.ip)
		if !ok || disp != tc.wantDisp {
			t.Errorf("LookupDisplay(%s) = (%q, %v), want (%q, true)", tc.ip, disp, ok, tc.wantDisp)
		}
	}

	if err := mgr.Delete("v4"); err != nil {
		t.Fatalf("Delete v4 failed: %v", err)
	}
	if mgr.Status().V4.Downloaded {
		t.Fatalf("expected v4 deleted")
	}
	if got := mgr.Lookup("1.2.3.4"); got.Found || got.ISPGroup != ispGroupOther {
		t.Fatalf("expected fallback after v4 delete, got %+v", got)
	}
}

func TestEdgeNodeMasterAgentEndToEnd(t *testing.T) {
	// 1. Mock upstream Emby + external CDN (302 target)
	payload := bytes.Repeat([]byte("YUNDU-VIDEO-CHUNK-"), 1024)
	var cdnSawToken string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnSawToken = r.Header.Get("X-Emby-Token")
		http.ServeContent(w, r, "movie.mkv", time.Now(), bytes.NewReader(payload))
	}))
	defer cdn.Close()

	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/emby/Items/1/Images/"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("POSTER-BYTES"))
		case strings.HasPrefix(r.URL.Path, "/emby/Videos/42/stream.mkv"):
			http.Redirect(w, r, cdn.URL+"/movie.mkv", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer emby.Close()

	// 2. Initialize Master App + synthetic ip2region DB
	dbPath := filepath.Join(t.TempDir(), "edge.db")
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
		{start: netip.MustParseAddr("5.6.7.0"), end: netip.MustParseAddr("5.6.7.255"), region: "中国|0|北京市|北京市|联通"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x42}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)

	app := &App{
		db:        db,
		dbPath:    dbPath,
		pm:        pm,
		ip2region: ipMgr,
	}

	site, err := db.CreateSiteRecord(Site{
		Name:        "TestEmby",
		ListenPort:  0,
		IngressMode: ingressModePath,
		PathPrefix:  "/embytest",
		TargetURL:   emby.URL,
		Enabled:     true,
		UAMode:      "passthrough",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if err := pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	defer pm.GracefulShutdown(context.Background())

	// Expose Master heartbeat + site handler via httptest server
	masterMux := http.NewServeMux()
	masterMux.HandleFunc("/api/agent/heartbeat", app.handleAgentHeartbeat)
	masterSrv := httptest.NewServer(masterMux)
	defer masterSrv.Close()

	// 3. Create a minimal Slave node (only name + public_port, no domain/IP!)
	createReq := httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"香港 CN2","public_port":19090}`))
	createRec := httptest.NewRecorder()
	app.handleEdgeNodes(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create edge node status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created EdgeNode
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created node: %v", err)
	}
	if created.Status != "waiting" || created.InstallCommand == "" {
		t.Fatalf("expected waiting node with install_command, got %+v", created)
	}

	// Read node plaintext token from runtime to launch real edgeAgent
	edgeRT.mu.RLock()
	nodeToken := edgeRT.nodes[created.ID].tokenPlain
	edgeRT.mu.RUnlock()

	// 4. Launch real Edge Agent on an ephemeral local port
	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: masterSrv.URL,
		NodeID:    created.ID,
		Token:     nodeToken,
		BindAddr:  "127.0.0.1",
	})
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen agent: %v", err)
	}
	agentPort := rawLn.Addr().(*net.TCPAddr).Port
	agent.cfg.Port = agentPort
	agentSrv := &http.Server{Handler: agent.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = agentSrv.Serve(&autoTLSListener{Listener: rawLn, getCert: agent.getCertificate}) }()
	defer agentSrv.Close()

	// Update node's public_port to match our ephemeral agent port
	updateReq := httptest.NewRequest(http.MethodPut, "/api/edge-nodes/1", strings.NewReader(`{"name":"香港 CN2","public_port":`+jsonInt(agentPort)+`}`))
	updateRec := httptest.NewRecorder()
	app.handleEdgeNodeByID(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update edge node status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	// Send initial heartbeat from Agent -> Master
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("agent sendHeartbeat: %v", err)
	}

	// Verify Master auto-detected 127.0.0.1 and node is online
	listRec := httptest.NewRecorder()
	app.handleEdgeNodes(listRec, httptest.NewRequest(http.MethodGet, "/api/edge-nodes", nil))
	var listed []EdgeNode
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("list edge nodes failed: %v %s", err, listRec.Body.String())
	}
	if !listed[0].Online || listed[0].Status != "online" || !strings.HasPrefix(listed[0].PublicURL, "http://127.0.0.1:") {
		t.Fatalf("expected online auto-IP node, got %+v", listed[0])
	}

	// 5. Configure ISP routes: ct -> [Slave, Master], cu -> [Master, Slave]
	routeBody := `{"group":"ct","nodes":[{"node_id":` + jsonInt(int(created.ID)) + `,"enabled":true},{"node_id":0,"enabled":true}]}`
	routeRec := httptest.NewRecorder()
	app.handleISPRoutes(routeRec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(routeBody)))
	if routeRec.Code != http.StatusOK {
		t.Fatalf("update isp route status=%d body=%s", routeRec.Code, routeRec.Body.String())
	}

	siteHandler, ok := pm.SiteHandler(site.ID)
	if !ok {
		t.Fatalf("site handler missing")
	}

	// 5a. Poster image from Telecom IP 1.2.3.4 MUST stay on Master (no 307)
	imgReq := httptest.NewRequest(http.MethodGet, "/emby/Items/1/Images/Primary", nil)
	imgReq.RemoteAddr = "127.0.0.1:40000"
	imgReq.Header.Set("X-Real-IP", "1.2.3.4")
	imgRec := httptest.NewRecorder()
	siteHandler.ServeHTTP(imgRec, imgReq)
	if imgRec.Code != http.StatusOK || imgRec.Body.String() != "POSTER-BYTES" {
		t.Fatalf("expected master poster 200, got %d %q", imgRec.Code, imgRec.Body.String())
	}

	// 5b. Video stream from Unicom IP 5.6.7.8 MUST stay on Master (Master is #1 for cu)
	cuReq := httptest.NewRequest(http.MethodGet, "/emby/Videos/42/stream.mkv", nil)
	cuReq.RemoteAddr = "127.0.0.1:40001"
	cuReq.Header.Set("X-Real-IP", "5.6.7.8")
	cuRec := httptest.NewRecorder()
	siteHandler.ServeHTTP(cuRec, cuReq)
	if cuRec.Code == http.StatusTemporaryRedirect && strings.Contains(cuRec.Header().Get("Location"), edgeTicketPathPrefix) {
		t.Fatalf("unicom video should not redirect to edge node, got location %q", cuRec.Header().Get("Location"))
	}

	// 5c. Video stream from Telecom IP 1.2.3.4 MUST 307 redirect to Slave Agent!
	ctReq := httptest.NewRequest(http.MethodGet, "/emby/Videos/42/stream.mkv", nil)
	ctReq.RemoteAddr = "127.0.0.1:40002"
	ctReq.Header.Set("X-Real-IP", "1.2.3.4")
	ctReq.Header.Set("X-Emby-Token", "secret-emby-token")
	ctRec := httptest.NewRecorder()
	siteHandler.ServeHTTP(ctRec, ctReq)
	if ctRec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect for telecom video, got %d body=%s", ctRec.Code, ctRec.Body.String())
	}
	loc := ctRec.Header().Get("Location")
	if !strings.HasPrefix(loc, listed[0].PublicURL+edgeTicketPathPrefix) || !strings.HasSuffix(loc, "/stream.mkv") {
		t.Fatalf("unexpected edge redirect location: %q", loc)
	}

	// 5d. Follow 307 to real running Edge Agent with Range request
	playReq, err := http.NewRequest(http.MethodGet, loc, nil)
	if err != nil {
		t.Fatalf("new play req: %v", err)
	}
	playReq.Header.Set("Range", "bytes=0-63")
	playResp, err := http.DefaultClient.Do(playReq)
	if err != nil {
		t.Fatalf("edge agent playback request failed: %v", err)
	}
	gotBytes, _ := io.ReadAll(playResp.Body)
	_ = playResp.Body.Close()
	if playResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content from agent, got %d", playResp.StatusCode)
	}
	if !bytes.Equal(gotBytes, payload[:64]) {
		t.Fatalf("agent returned wrong payload bytes (len=%d)", len(gotBytes))
	}
	if cdnSawToken != "" {
		t.Fatalf("sensitive X-Emby-Token leaked across redirect to external CDN: %q", cdnSawToken)
	}

	// 5e. Report traffic back to Master via heartbeat and confirm accounting
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("second heartbeat failed: %v", err)
	}
	edgeRT.flush()
	listRec2 := httptest.NewRecorder()
	app.handleEdgeNodes(listRec2, httptest.NewRequest(http.MethodGet, "/api/edge-nodes", nil))
	var listed2 []EdgeNode
	_ = json.Unmarshal(listRec2.Body.Bytes(), &listed2)
	if listed2[0].TrafficUsed < 64 {
		t.Fatalf("expected edge node traffic_used >= 64, got %d", listed2[0].TrafficUsed)
	}

	// 6. Simulate slave going offline -> Telecom video falls back to Master!
	edgeRT.mu.Lock()
	edgeRT.nodes[created.ID].lastSeen = time.Now().Add(-time.Minute)
	edgeRT.mu.Unlock()

	fallbackReq := httptest.NewRequest(http.MethodGet, "/emby/Videos/42/stream.mkv", nil)
	fallbackReq.RemoteAddr = "127.0.0.1:40003"
	fallbackReq.Header.Set("X-Real-IP", "1.2.3.4")
	fallbackRec := httptest.NewRecorder()
	siteHandler.ServeHTTP(fallbackRec, fallbackReq)
	if fallbackRec.Code == http.StatusTemporaryRedirect && strings.Contains(fallbackRec.Header().Get("Location"), edgeTicketPathPrefix) {
		t.Fatalf("offline slave should not receive redirect, got %q", fallbackRec.Header().Get("Location"))
	}
}

func TestEdgeAgentAutoTLSAndMasterCertSync(t *testing.T) {
	// Generate a self-signed wildcard cert for *.example.com
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"*.example.com", "example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tls.db")
	certPath, keyPath := panelTLSPaths(dbPath)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		t.Fatalf("mkdir cert dir: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	certMgr := newPanelCertificateManager(dbPath, nil)

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x11}, 32))
	edgeRT := newEdgeNodeRuntime(db, pm, nil)
	pm.SetEdgeRuntime(edgeRT)
	app := &App{
		db:                db,
		dbPath:            dbPath,
		pm:                pm,
		panelCertificates: certMgr,
	}

	masterMux := http.NewServeMux()
	masterMux.HandleFunc("/api/agent/heartbeat", app.handleAgentHeartbeat)
	masterSrv := httptest.NewServer(masterMux)
	defer masterSrv.Close()

	// Create node with wildcard subdomain + tls_mode=auto
	createRec := httptest.NewRecorder()
	app.handleEdgeNodes(createRec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"HK TLS","public_port":19443,"public_host":"hk.example.com","tls_mode":"auto"}`)))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create tls node status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created EdgeNode
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	edgeRT.mu.RLock()
	token := edgeRT.nodes[created.ID].tokenPlain
	edgeRT.mu.RUnlock()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: masterSrv.URL,
		NodeID:    created.ID,
		Token:     token,
	})
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}
	if !agent.hasTLS() {
		t.Fatalf("expected agent to receive and load wildcard TLS certificate from master")
	}

	// Start dual-protocol autoTLSListener and verify BOTH HTTP and HTTPS work on the same port!
	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: agent.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(&autoTLSListener{Listener: rawLn, getCert: agent.getCertificate}) }()
	defer srv.Close()

	addr := rawLn.Addr().String()
	// Plain HTTP check
	httpResp, err := http.Get("http://" + addr + "/_yundu/health")
	if err != nil {
		t.Fatalf("plain HTTP health failed: %v", err)
	}
	_ = httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP health status=%d", httpResp.StatusCode)
	}

	// HTTPS check on the exact same port with SNI hk.example.com
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	tlsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
				ServerName: "hk.example.com",
			},
		},
	}
	httpsResp, err := tlsClient.Get("https://" + addr + "/_yundu/health")
	if err != nil {
		t.Fatalf("HTTPS health on autoTLSListener failed: %v", err)
	}
	_ = httpsResp.Body.Close()
	if httpsResp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS health status=%d", httpsResp.StatusCode)
	}
}

func TestShareLoginPasswordFallback(t *testing.T) {
	parsed, ok := parseShareLoginBody("application/json", []byte(`{"Username":"alice","Password":"secret123"}`))
	if !ok || parsed.username != "alice" || parsed.password != "secret123" {
		t.Fatalf("expected Password fallback to work, got ok=%v user=%q pass=%q", ok, parsed.username, parsed.password)
	}
	parsed2, ok2 := parseShareLoginBody("application/json", []byte(`{"Username":"alice","Pw":"primary","Password":"ignored"}`))
	if !ok2 || parsed2.password != "primary" {
		t.Fatalf("expected Pw to take precedence over Password, got %q", parsed2.password)
	}
}

func jsonInt(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}
