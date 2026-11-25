package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
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
		{start: netip.MustParseAddr("9.10.11.0"), end: netip.MustParseAddr("9.10.11.255"), region: "中国|北京市|北京市|移动|CN"},
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), region: "美国|0|0|Google|US"},
	})
	v6Data := buildSyntheticXDB(t, xdbIPv6VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("240e:100::"), end: netip.MustParseAddr("240e:100::ffff"), region: "中国|广东省|广州市|电信|CN"},
		{start: netip.MustParseAddr("2408:200::"), end: netip.MustParseAddr("2408:200::ffff"), region: "中国|浙江省|杭州市|联通|CN"},
		{start: netip.MustParseAddr("2409:300::"), end: netip.MustParseAddr("2409:300::ffff"), region: "中国|江苏省|南京市|移动|CN"},
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
	if created.Status != "waiting" || created.InstallCommand == "" || !strings.Contains(created.UninstallCommand, "agent --uninstall") {
		t.Fatalf("expected waiting node with install_command and uninstall_command, got %+v", created)
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

func TestISPBlockNonMainlandAndPerGroupBlock(t *testing.T) {
	var upstreamHits int
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer emby.Close()

	dbPath := filepath.Join(t.TempDir(), "block.db")
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
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|广东省|深圳市|电信|CN"},
		{start: netip.MustParseAddr("5.6.7.0"), end: netip.MustParseAddr("5.6.7.255"), region: "中国|江苏省|南京市|联通|CN"},
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), region: "美国|0|0|Google|US"},
		{start: netip.MustParseAddr("11.12.13.0"), end: netip.MustParseAddr("11.12.13.255"), region: "中国|湖北省|武汉市|广电|CN"},
		{start: netip.MustParseAddr("45.46.47.0"), end: netip.MustParseAddr("45.46.47.255"), region: "中国|香港|香港|PCCW|HK"},
		{start: netip.MustParseAddr("60.61.62.0"), end: netip.MustParseAddr("60.61.62.255"), region: "中国|台湾|台北|中华电信|TW"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x33}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)
	app := &App{db: db, dbPath: dbPath, pm: pm, ip2region: ipMgr}

	site, err := db.CreateSiteRecord(Site{
		Name:        "BlockTest",
		ListenPort:  0,
		IngressMode: ingressModePath,
		PathPrefix:  "/blocktest",
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

	siteHandler, ok := pm.SiteHandler(site.ID)
	if !ok {
		t.Fatalf("site handler missing")
	}

	probe := func(ip, reqPath string) int {
		req := httptest.NewRequest(http.MethodGet, reqPath, nil)
		req.RemoteAddr = "127.0.0.1:41000"
		if ip != "" {
			req.Header.Set("X-Real-IP", ip)
		}
		rec := httptest.NewRecorder()
		siteHandler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 1. Enable block_non_mainland
	putRec := httptest.NewRecorder()
	app.handleISPRoutes(putRec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(`{"block_non_mainland":true}`)))
	if putRec.Code != http.StatusOK {
		t.Fatalf("enable block_non_mainland status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	upstreamHits = 0
	for _, overseasIP := range []string{"8.8.8.8", "45.46.47.48", "60.61.62.63"} {
		if code := probe(overseasIP, "/emby/System/Info/Public"); code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-mainland IP %s, got %d", overseasIP, code)
		}
		if code := probe(overseasIP, "/emby/Videos/1/stream.mkv"); code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-mainland video %s, got %d", overseasIP, code)
		}
	}
	if upstreamHits != 0 {
		t.Fatalf("blocked non-mainland requests must not touch upstream, got %d hits", upstreamHits)
	}

	// Mainland minor ISP (广电 -> other), Telecom, and Loopback MUST still be allowed
	for _, allowedIP := range []string{"11.12.13.14", "1.2.3.4", "127.0.0.1"} {
		if code := probe(allowedIP, "/emby/System/Info/Public"); code != http.StatusOK {
			t.Fatalf("expected 200 for allowed IP %s, got %d", allowedIP, code)
		}
	}

	// Lookup API reports blocked + non_mainland_cn accurately
	lookupRec := httptest.NewRecorder()
	app.handleIP2Region(lookupRec, httptest.NewRequest(http.MethodPost, "/api/ip2region/lookup", strings.NewReader(`{"ip":"45.46.47.48"}`)))
	var lookupResp struct {
		Blocked       bool   `json:"blocked"`
		BlockReason   string `json:"block_reason"`
		NonMainlandCN bool   `json:"non_mainland_cn"`
	}
	if err := json.Unmarshal(lookupRec.Body.Bytes(), &lookupResp); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if !lookupResp.Blocked || lookupResp.BlockReason != "non_mainland" || !lookupResp.NonMainlandCN {
		t.Fatalf("unexpected lookup response for HK IP: %+v", lookupResp)
	}

	// 2. Enable per-group block on Unicom (cu)
	putRec2 := httptest.NewRecorder()
	app.handleISPRoutes(putRec2, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(`{"group":"cu","block_connect":true}`)))
	if putRec2.Code != http.StatusOK {
		t.Fatalf("enable cu block status=%d body=%s", putRec2.Code, putRec2.Body.String())
	}
	if code := probe("5.6.7.8", "/emby/System/Info/Public"); code != http.StatusForbidden {
		t.Fatalf("expected 403 for blocked cu group IP 5.6.7.8, got %d", code)
	}
	if code := probe("1.2.3.4", "/emby/System/Info/Public"); code != http.StatusOK {
		t.Fatalf("expected 200 for unblocked ct group IP 1.2.3.4, got %d", code)
	}
	if code := probe("127.0.0.1", "/emby/System/Info/Public"); code != http.StatusOK {
		t.Fatalf("loopback IP must never be blocked, got %d", code)
	}
}

func jsonInt(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEdgeRoutingModeMasterSkipsEdgeNode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
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
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|广东省|广州市|电信|CN"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write v4 xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "video/x-matroska")
		_, _ = w.Write([]byte("master-video-bytes"))
	}))
	defer upstream.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x34}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)
	app := &App{db: db, dbPath: dbPath, pm: pm, ip2region: ipMgr, trustedProxies: []*net.IPNet{loopbackCIDR}}

	// Create an online edge node and assign it as #1 for Telecom (ct)
	createRec := httptest.NewRecorder()
	app.handleEdgeNodes(createRec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"HK-Edge","public_port":19090,"public_ip":"198.51.100.10","tls_mode":"http"}`)))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("createEdgeNode status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var node EdgeNode
	if err := json.Unmarshal(createRec.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode edge node: %v", err)
	}
	routeRec := httptest.NewRecorder()
	routeBody := `{"group":"ct","nodes":[{"node_id":` + jsonInt(int(node.ID)) + `,"enabled":true},{"node_id":0,"enabled":true}]}`
	app.handleISPRoutes(routeRec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(routeBody)))
	if routeRec.Code != http.StatusOK {
		t.Fatalf("update isp routes status=%d body=%s", routeRec.Code, routeRec.Body.String())
	}
	edgeRT.mu.Lock()
	if live := edgeRT.nodes[node.ID]; live != nil {
		live.lastSeen = time.Now()
		live.agentVersion = appVersion
	}
	edgeRT.mu.Unlock()

	masterSite, err := db.CreateSiteRecord(Site{
		Name:            "MasterOnly",
		TargetURL:       upstream.URL,
		ListenPort:      18101,
		IngressMode:     ingressModePath,
		PathPrefix:      "/masteronly",
		Enabled:         true,
		UAMode:          "passthrough",
		EdgeRoutingMode: edgeRoutingModeMaster,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord master: %v", err)
	}
	globalSite, err := db.CreateSiteRecord(Site{
		Name:            "GlobalEdge",
		TargetURL:       upstream.URL,
		ListenPort:      18102,
		IngressMode:     ingressModePath,
		PathPrefix:      "/globaledge",
		Enabled:         true,
		UAMode:          "passthrough",
		EdgeRoutingMode: edgeRoutingModeGlobal,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord global: %v", err)
	}
	if err := pm.StartSite(*masterSite); err != nil {
		t.Fatalf("StartSite master: %v", err)
	}
	if err := pm.StartSite(*globalSite); err != nil {
		t.Fatalf("StartSite global: %v", err)
	}
	defer pm.GracefulShutdown(context.Background())

	// 1. masterSite MUST NOT redirect to edge node; it proxies directly from master
	hMaster, _ := pm.SiteHandler(masterSite.ID)
	req1 := httptest.NewRequest(http.MethodGet, "/emby/Videos/101/stream.mkv?Static=true", nil)
	req1.RemoteAddr = "127.0.0.1:41000"
	req1.Header.Set("X-Real-IP", "1.2.3.4")
	rec1 := httptest.NewRecorder()
	hMaster.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK || rec1.Body.String() != "master-video-bytes" || upstreamHits != 1 {
		t.Fatalf("master-only site should serve directly from master, got status=%d loc=%q body=%q hits=%d",
			rec1.Code, rec1.Header().Get("Location"), rec1.Body.String(), upstreamHits)
	}

	// 2. globalSite MUST 307 redirect to edge node
	hGlobal, _ := pm.SiteHandler(globalSite.ID)
	req2 := httptest.NewRequest(http.MethodGet, "/emby/Videos/101/stream.mkv?Static=true", nil)
	req2.RemoteAddr = "127.0.0.1:41000"
	req2.Header.Set("X-Real-IP", "1.2.3.4")
	rec2 := httptest.NewRecorder()
	hGlobal.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTemporaryRedirect || !strings.HasPrefix(rec2.Header().Get("Location"), "http://198.51.100.10:19090"+edgeTicketPathPrefix) {
		t.Fatalf("global site should 307 to edge node, got status=%d loc=%q", rec2.Code, rec2.Header().Get("Location"))
	}
}

func TestLiveSessionsSingleDeviceKickAndSelfNetwork(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
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
		{start: netip.MustParseAddr("1.2.3.0"), end: netip.MustParseAddr("1.2.3.255"), region: "中国|广东省|广州市|电信|CN"},
		{start: netip.MustParseAddr("45.46.47.0"), end: netip.MustParseAddr("45.46.47.255"), region: "中国|香港|0|PCCW|HK"},
	})
	if err := os.WriteFile(filepath.Join(ipDir, "ip2region_v4.xdb"), v4Raw, 0o600); err != nil {
		t.Fatalf("write v4 xdb: %v", err)
	}
	ipMgr := newIP2RegionManager(ipDir, nil)
	defer ipMgr.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x35}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, ipMgr)
	pm.SetEdgeRuntime(edgeRT)
	app := &App{db: db, dbPath: dbPath, pm: pm, ip2region: ipMgr, trustedProxies: []*net.IPNet{loopbackCIDR}}

	site, err := db.CreateSiteRecord(Site{
		Name:        "TestEmby",
		TargetURL:   "https://emby.example.com",
		IngressMode: ingressModePath,
		PathPrefix:  "/testemby",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	createUserBody := fmt.Sprintf(`{"username":"alice","password":"secret-password","enabled":true,"sites":[{"site_id":%d}]}`, site.ID)
	createUserRec := httptest.NewRecorder()
	app.handleShareUsers(createUserRec, httptest.NewRequest(http.MethodPost, "/api/share-users", strings.NewReader(createUserBody)))
	if createUserRec.Code != http.StatusCreated {
		t.Fatalf("createShareUser status=%d body=%s", createUserRec.Code, createUserRec.Body.String())
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(createUserRec.Body.Bytes(), &user); err != nil {
		t.Fatalf("decode share user: %v", err)
	}
	pm.shares.mu.RLock()
	userState := pm.shares.users[user.ID]
	pm.shares.mu.RUnlock()

	// Register two distinct device sessions
	tok1 := "emby-token-device-1"
	tok2 := "emby-token-device-2"
	if err := pm.shares.registerSession(&shareLoginPending{
		rt: pm.shares, user: userState, siteID: site.ID,
		deviceID: "dev-1", device: "Apple TV", client: "Infuse", clientIP: "1.2.3.4",
	}, tok1); err != nil {
		t.Fatalf("registerSession 1: %v", err)
	}
	if err := pm.shares.registerSession(&shareLoginPending{
		rt: pm.shares, user: userState, siteID: site.ID,
		deviceID: "dev-2", device: "iPhone", client: "SenPlayer", clientIP: "1.2.3.4",
	}, tok2); err != nil {
		t.Fatalf("registerSession 2: %v", err)
	}
	hash1 := shareTokenHash(tok1)
	hash2 := shareTokenHash(tok2)
	pm.shares.markEdgePlayback(hash2, 1, "香港从机")

	// 1. GET /api/live-sessions should return both sessions with playing session first + region + node_name
	liveRec := httptest.NewRecorder()
	app.handleLiveSessions(liveRec, httptest.NewRequest(http.MethodGet, "/api/live-sessions", nil))
	if liveRec.Code != http.StatusOK {
		t.Fatalf("live-sessions status=%d body=%s", liveRec.Code, liveRec.Body.String())
	}
	var liveList []liveShareSessionView
	if err := json.Unmarshal(liveRec.Body.Bytes(), &liveList); err != nil {
		t.Fatalf("decode live-sessions: %v", err)
	}
	if len(liveList) != 2 {
		t.Fatalf("expected 2 live sessions, got %d", len(liveList))
	}
	if liveList[0].SessionID != hash2 || !liveList[0].Playing || liveList[0].NodeName != "香港从机" {
		t.Fatalf("expected playing edge session first, got %+v", liveList[0])
	}
	if !strings.Contains(liveList[0].ClientRegion, "广东") || !strings.Contains(liveList[0].ClientRegion, "电信") {
		t.Fatalf("expected formatted client_region, got %q", liveList[0].ClientRegion)
	}

	// 2. Admin single-device kick revokes ONLY hash1
	kickRec := httptest.NewRecorder()
	kickPath := fmt.Sprintf("/api/share-users/%d/sessions/%s/kick", user.ID, hash1)
	app.handleShareUserByID(kickRec, httptest.NewRequest(http.MethodPost, kickPath, nil))
	if kickRec.Code != http.StatusOK {
		t.Fatalf("admin kick session status=%d body=%s", kickRec.Code, kickRec.Body.String())
	}
	remaining, _ := pm.shares.listSessions(user.ID)
	if len(remaining) != 1 || remaining[0].SessionID != hash2 {
		t.Fatalf("expected only session 2 to remain, got %+v", remaining)
	}

	// 3. Friend /api/me returns network diagnosis (normal mainland vs blocked HK) + self single-device kick
	var pwHash string
	if err := db.db.QueryRow(`SELECT password_hash FROM share_users WHERE id=?`, user.ID).Scan(&pwHash); err != nil {
		t.Fatalf("load pwHash: %v", err)
	}
	cookieVal := signShareSession(user.ID, pwHash, time.Now())

	putRec := httptest.NewRecorder()
	app.handleISPRoutes(putRec, httptest.NewRequest(http.MethodPut, "/api/isp-routes", strings.NewReader(`{"block_non_mainland":true}`)))
	if putRec.Code != http.StatusOK {
		t.Fatalf("enable block_non_mainland: %d", putRec.Code)
	}

	meReqHK := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReqHK.RemoteAddr = "127.0.0.1:42000"
	meReqHK.Header.Set("X-Real-IP", "45.46.47.48")
	meReqHK.AddCookie(&http.Cookie{Name: shareSessionCookie, Value: cookieVal})
	meRecHK := httptest.NewRecorder()
	app.handleShareSelf(meRecHK, meReqHK)
	if meRecHK.Code != http.StatusOK {
		t.Fatalf("GET /api/me (HK) status=%d body=%s", meRecHK.Code, meRecHK.Body.String())
	}
	var meHK shareSelfView
	if err := json.Unmarshal(meRecHK.Body.Bytes(), &meHK); err != nil {
		t.Fatalf("decode /api/me HK: %v", err)
	}
	if meHK.Network == nil || !meHK.Network.Blocked || meHK.Network.BlockReason != "non_mainland" || !meHK.Network.NonMainlandCN {
		t.Fatalf("expected blocked non_mainland network diagnosis for HK IP, got %+v", meHK.Network)
	}

	meReqCN := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReqCN.RemoteAddr = "127.0.0.1:42001"
	meReqCN.Header.Set("X-Real-IP", "1.2.3.4")
	meReqCN.AddCookie(&http.Cookie{Name: shareSessionCookie, Value: cookieVal})
	meRecCN := httptest.NewRecorder()
	app.handleShareSelf(meRecCN, meReqCN)
	var meCN shareSelfView
	if err := json.Unmarshal(meRecCN.Body.Bytes(), &meCN); err != nil {
		t.Fatalf("decode /api/me CN: %v", err)
	}
	if meCN.Network == nil || meCN.Network.Blocked || meCN.Network.NodeName != "主机" || meCN.Network.ISPGroup != ispGroupCT {
		t.Fatalf("expected allowed ct/主机 network diagnosis, got %+v", meCN.Network)
	}

	// Self single-device kick revokes hash2
	selfKickReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/me/sessions/"+hash2+"/kick", nil)
	selfKickReq.Header.Set("Origin", "http://example.com")
	selfKickReq.AddCookie(&http.Cookie{Name: shareSessionCookie, Value: cookieVal})
	selfKickRec := httptest.NewRecorder()
	app.handleShareSelf(selfKickRec, selfKickReq)
	if selfKickRec.Code != http.StatusOK {
		t.Fatalf("self kick session status=%d body=%s", selfKickRec.Code, selfKickRec.Body.String())
	}
	remainingAfter, _ := pm.shares.listSessions(user.ID)
	if len(remainingAfter) != 0 {
		t.Fatalf("expected 0 remaining sessions after self kick, got %d", len(remainingAfter))
	}
}

func TestEdgeAgentHealthAndRemoteUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	site, err := db.CreateSiteRecord(Site{
		Name:        "ProbeSite",
		TargetURL:   upstream.URL,
		IngressMode: ingressModePath,
		PathPrefix:  "/probesite",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x36}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	edgeRT := newEdgeNodeRuntime(db, pm, nil)
	pm.SetEdgeRuntime(edgeRT)
	if err := pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	defer pm.GracefulShutdown(context.Background())
	app := &App{db: db, dbPath: dbPath, pm: pm, trustedProxies: []*net.IPNet{loopbackCIDR}}

	createRec := httptest.NewRecorder()
	app.handleEdgeNodes(createRec, httptest.NewRequest(http.MethodPost, "/api/edge-nodes", strings.NewReader(`{"name":"UpgradeNode","public_port":19090}`)))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("createEdgeNode status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var node EdgeNode
	if err := json.Unmarshal(createRec.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode edge node: %v", err)
	}
	edgeRT.mu.RLock()
	nodeToken := edgeRT.nodes[node.ID].tokenPlain
	edgeRT.mu.RUnlock()

	masterServer := httptest.NewServer(http.HandlerFunc(app.handleAgentHeartbeat))
	defer masterServer.Close()

	assetName, err := edgeAgentAssetName()
	if err != nil {
		t.Fatalf("edgeAgentAssetName: %v", err)
	}
	newBinaryPayload := []byte("meridian-binary-v9.9.9-test-payload")
	sumBytes := sha256.Sum256(newBinaryPayload)
	validSum := hex.EncodeToString(sumBytes[:])
	badSum := strings.Repeat("0", 64)
	serveBadSum := false

	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v9.9.9/SHA256SUMS":
			if serveBadSum {
				_, _ = fmt.Fprintf(w, "%s  %s\n", badSum, assetName)
			} else {
				_, _ = fmt.Fprintf(w, "%s  %s\n", validSum, assetName)
			}
		case "/v9.9.9/" + assetName:
			_, _ = w.Write(newBinaryPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer releaseServer.Close()

	exePath := filepath.Join(t.TempDir(), "meridian-agent-test")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write fake exe: %v", err)
	}

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL:      masterServer.URL,
		NodeID:         node.ID,
		Token:          nodeToken,
		Port:           19090,
		ReleaseBaseURL: releaseServer.URL,
		ReleaseClient:  releaseServer.Client(),
		ExecutablePath: exePath,
	})
	agent.masterClient = masterServer.Client()

	// 1. First heartbeat receives probe targets; run probe once; second heartbeat reports health + latency
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	agent.runProbeOnce(context.Background())
	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}

	listRec := httptest.NewRecorder()
	app.handleEdgeNodes(listRec, httptest.NewRequest(http.MethodGet, "/api/edge-nodes", nil))
	var nodesResp []EdgeNode
	if err := json.Unmarshal(listRec.Body.Bytes(), &nodesResp); err != nil {
		t.Fatalf("decode edge-nodes: %v", err)
	}
	if len(nodesResp) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodesResp))
	}
	if nodesResp[0].MemoryBytes == 0 {
		t.Fatalf("expected non-zero memory_bytes in node view")
	}
	if len(nodesResp[0].UpstreamLatencies) != 1 || nodesResp[0].UpstreamLatencies[0].SiteName != site.Name || nodesResp[0].UpstreamLatencies[0].LatencyMs <= 0 {
		t.Fatalf("expected resolved ProbeSite latency > 0, got %+v", nodesResp[0].UpstreamLatencies)
	}

	// 2. SHA-256 mismatch must fail cleanly and preserve old binary
	serveBadSum = true
	if err := agent.performSelfUpgrade(context.Background(), "v9.9.9"); err == nil {
		t.Fatalf("expected SHA-256 mismatch error")
	}
	gotOld, _ := os.ReadFile(exePath)
	if string(gotOld) != "old-binary" {
		t.Fatalf("binary must remain untouched on checksum mismatch, got %q", string(gotOld))
	}

	// 3. Queue remote upgrade via Master API -> Heartbeat delivers -> Agent swaps binary & signals restartCh
	serveBadSum = false
	upgRec := httptest.NewRecorder()
	app.handleEdgeNodeByID(upgRec, httptest.NewRequest(
		http.MethodPost,
		fmt.Sprintf("/api/edge-nodes/%d/upgrade", node.ID),
		strings.NewReader(`{"version":"v9.9.9"}`),
	))
	if upgRec.Code != http.StatusOK {
		t.Fatalf("queue upgrade status=%d body=%s", upgRec.Code, upgRec.Body.String())
	}

	if err := agent.sendHeartbeat(context.Background()); err != nil {
		t.Fatalf("upgrade heartbeat: %v", err)
	}

	select {
	case <-agent.restartCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for agent self-upgrade restart signal")
	}
	gotNew, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if !bytes.Equal(gotNew, newBinaryPayload) {
		t.Fatalf("upgraded binary mismatch: got %q", string(gotNew))
	}
}

func TestEdgeNodeAlertMonitorTransitions(t *testing.T) {
	rt := &edgeNodeRuntime{
		nodes: map[int64]*edgeNodeLiveState{
			1: {
				id:           1,
				name:         "HK-Alert",
				enabled:      true,
				publicPort:   19090,
				publicIP:     "198.51.100.20",
				tlsMode:      "http",
				agentVersion: "v1.2.0",
				quota:        1000 * 1024 * 1024, // ~1000 MiB
				resetPeriod:  "2026-11",
			},
		},
	}
	t0 := time.Now()
	rt.nodes[1].lastSeen = t0

	// 1. Initial online observation: no alert yet, marks alertSeenOnline=true
	if msgs := rt.collectAlerts(t0); len(msgs) != 0 {
		t.Fatalf("expected 0 alerts on initial online, got %v", msgs)
	}

	// 2. Node goes offline -> 1 offline alert
	tOffline := t0.Add(edgeNodeOfflineAfter + 5*time.Second)
	msgs := rt.collectAlerts(tOffline)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "已离线") {
		t.Fatalf("expected 1 offline alert, got %v", msgs)
	}
	// Repeat check while still offline -> deduplicated (0 alerts)
	if msgs2 := rt.collectAlerts(tOffline.Add(15 * time.Second)); len(msgs2) != 0 {
		t.Fatalf("expected deduplicated offline alert, got %v", msgs2)
	}

	// 3. Node recovers online -> 1 recovery alert
	tRecover := tOffline.Add(30 * time.Second)
	rt.nodes[1].lastSeen = tRecover
	msgs = rt.collectAlerts(tRecover)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "恢复上线") {
		t.Fatalf("expected 1 recovery alert, got %v", msgs)
	}

	// 4. Quota crosses 80% -> 1 warning alert, deduplicated
	rt.nodes[1].persistedUsed = 820 * 1024 * 1024
	msgs = rt.collectAlerts(tRecover)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "流量预警") {
		t.Fatalf("expected 1 80%% quota alert, got %v", msgs)
	}
	if msgs2 := rt.collectAlerts(tRecover); len(msgs2) != 0 {
		t.Fatalf("expected deduplicated 80%% alert, got %v", msgs2)
	}

	// 5. Quota crosses 100% -> 1 exhausted alert, deduplicated
	rt.nodes[1].persistedUsed = 1000 * 1024 * 1024
	msgs = rt.collectAlerts(tRecover)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "已耗尽") {
		t.Fatalf("expected 1 100%% quota alert, got %v", msgs)
	}
	if msgs2 := rt.collectAlerts(tRecover); len(msgs2) != 0 {
		t.Fatalf("expected deduplicated 100%% alert, got %v", msgs2)
	}
}
