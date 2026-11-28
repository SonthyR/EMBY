package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEdgeCompatibleClientIPClassification(t *testing.T) {
	mobile := "中国|广东省|深圳市|移动|CN"
	for _, tc := range []struct {
		name, actual string
		want         edgeClientIPVerdict
	}{
		{"mobile", mobile, edgeIPSameCarrier},
		{"legacy mainland record", "中国|0|北京市|北京市|移动", edgeIPSameCarrier},
		{"other carrier", "中国|广东省|广州市|联通|CN", edgeIPDifferentCarrier},
		{"other group", "中国|广东省|广州市|教育网|CN", edgeIPUnknown},
		{"no country", "0|广东省|深圳市|移动", edgeIPUnknown},
		{"missing record", "", edgeIPUnknown},
		{"foreign mobile", "日本|东京|东京|移动|JP", edgeIPNonMainland},
		{"hong kong", "中国|香港|香港|移动|HK", edgeIPNonMainland},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareEdgeClientIPRegions(parseIP2RegionRaw(mobile), parseIP2RegionRaw(tc.actual)); got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEdgeClientIPCheckUsesAuthenticatedMasterAndStrictFallback(t *testing.T) {
	const nodeID int64 = 1
	const token = "test-edge-token"
	const expectedIP = "39.144.109.117"
	const peerIP = "39.144.109.101"
	data := buildSyntheticXDB(t, xdbIPv4VersionNo, []testXDBSegment{
		{start: netip.MustParseAddr("39.144.109.0"), end: netip.MustParseAddr("39.144.109.255"), region: "中国|广东省|深圳市|移动|CN"},
	})
	path := filepath.Join(t.TempDir(), "v4.xdb")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	xdb, err := openXDBReader(path, xdbIPv4VersionNo, true)
	if err != nil {
		t.Fatal(err)
	}
	defer xdb.close()
	rt := &edgeNodeRuntime{
		nodes: map[int64]*edgeNodeLiveState{
			nodeID: {enabled: true, lastSeen: time.Now(), tokenHash: edgeNodeTokenHash(token), supportsClientIPCompat: true},
			2:      {enabled: true, lastSeen: time.Now()},
		},
		blockNonMainland:     true,
		edgeBindClientIP:     true,
		edgeBindClientIPMode: edgeIPModeCompatible,
		ip2region:            &ip2regionManager{v4: xdb},
	}
	app := &App{pm: &ProxyManager{edges: rt}}
	srv := httptest.NewServer(http.HandlerFunc(app.handleAgentClientIPCheck))
	defer srv.Close()
	agent := newEdgeAgent(edgeAgentConfig{MasterURL: srv.URL, NodeID: nodeID, Token: token})
	mode := rt.edgeClientIPCheckMode(nodeID)
	if mode != edgeIPModeCompatible || rt.edgeClientIPCheckMode(2) != edgeIPModeStrict {
		t.Fatalf("unexpected modes for upgraded and legacy node")
	}
	ticket := edgePlaybackTicket{NodeID: nodeID, ClientIP: expectedIP, IPCheckMode: mode, TargetURL: "https://example.com/media.mkv", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	sealed, err := sealEdgePlaybackTicket(token, ticket)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peerIP + ":12345"
	if got := agent.checkPlaybackClientIP(req, sealed, ticket); got != edgeIPSameCarrier {
		t.Fatalf("compatible verdict = %q", got)
	}
	invalidAgent := newEdgeAgent(edgeAgentConfig{MasterURL: srv.URL, NodeID: nodeID, Token: "another-token"})
	if got := invalidAgent.askMasterClientIPCheck(context.Background(), sealed, peerIP); got != edgeIPCheckUnavailable {
		t.Fatalf("wrong token verdict = %q", got)
	}
	expired := ticket
	expired.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	expiredTicket, err := sealEdgePlaybackTicket(token, expired)
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.askMasterClientIPCheck(context.Background(), expiredTicket, peerIP); got != edgeIPCheckUnavailable {
		t.Fatalf("expired ticket verdict = %q", got)
	}
	req.RemoteAddr = "8.8.8.8:12345"
	if got := agent.checkPlaybackClientIP(req, sealed, ticket); got != edgeIPUnknown {
		t.Fatalf("missing region verdict = %q", got)
	}
	req.RemoteAddr = peerIP + ":12345"
	rt.edgeBindClientIPMode = edgeIPModeStrict
	if got := agent.checkPlaybackClientIP(req, sealed, ticket); got != edgeIPStrictMismatch {
		t.Fatalf("strict mode with a previously issued ticket verdict = %q", got)
	}
	ticket.IPCheckMode = ""
	if got := agent.checkPlaybackClientIP(req, sealed, ticket); got != edgeIPStrictMismatch {
		t.Fatalf("old ticket verdict = %q", got)
	}
	req.RemoteAddr = expectedIP + ":12345"
	if got := agent.checkPlaybackClientIP(req, sealed, ticket); got != edgeIPExactMatch {
		t.Fatalf("exact match verdict = %q", got)
	}
}
