package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestV181GuardedTicketsRequireNewEnvelope(t *testing.T) {
	const token = "edge-test-key"
	target := "https://cdn.example.com/media"
	guard := &edgeDynamicGuardPolicy{Profile: dynamicProfileCompatible}
	for _, wantVersion := range []int{1, 2} {
		var policy *edgeDynamicGuardPolicy
		if wantVersion == 2 {
			policy = guard
		}
		sealed, err := sealEdgePlaybackTicket(token, edgePlaybackTicket{TargetURL: target, DynamicGuard: policy})
		if err != nil {
			t.Fatal(err)
		}
		playback, err := openEdgePlaybackTicket(token, sealed)
		if err != nil || playback.Version != wantVersion {
			t.Fatalf("playback ticket version = %d, want %d, error = %v", playback.Version, wantVersion, err)
		}
		sealed, err = sealEdgeEgressTicket(token, edgeEgressTicket{TargetURL: target, DynamicGuard: policy})
		if err != nil {
			t.Fatal(err)
		}
		egress, err := openEdgeEgressTicket(token, sealed)
		if err != nil || egress.Version != wantVersion {
			t.Fatalf("egress ticket version = %d, want %d, error = %v", egress.Version, wantVersion, err)
		}
	}
	if validEdgeTicketVersion(1, guard) || validEdgeTicketVersion(2, nil) {
		t.Fatal("ticket version and guard requirement must agree")
	}
}

func TestV181BoundManifestRedirectsStayOnEdge(t *testing.T) {
	const nodeID, token, cdnHost, pinnedIP = 7, "edge-test-key", "cdn.example.com", "93.184.216.34"
	var originHits, guardedHits, egressHits, masterDirectHits atomic.Int32
	manifest := "#EXTM3U\n#EXTINF:10,\nrelative.ts\n"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guardedHits.Add(1)
		if r.URL.Path != "/branch/live.m3u8" || r.Header.Get("Range") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("guarded manifest received unexpected request path %q or headers", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprint(w, manifest)
	}))
	defer cdn.Close()
	_, cdnPort, _ := net.SplitHostPort(cdn.Listener.Addr().String())
	cdnURL := "http://" + net.JoinHostPort(cdnHost, cdnPort)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		if r.URL.Path == "/entry.m3u8" {
			http.Redirect(w, r, cdnURL+"/branch/live.m3u8", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = fmt.Fprint(w, manifest)
	}))
	defer origin.Close()

	agent := newEdgeAgent(edgeAgentConfig{MasterURL: "http://panel.example.com", NodeID: nodeID, Token: token})
	agent.dynamicResolver = dynamicIPResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != cdnHost {
			return nil, fmt.Errorf("unexpected host %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP(pinnedIP)}}, nil
	})
	agent.dynamicDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != net.JoinHostPort(pinnedIP, cdnPort) {
			return nil, fmt.Errorf("unexpected dial address %q", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, cdn.Listener.Addr().String())
	}
	edge := httptest.NewServer(agent.handler())
	defer edge.Close()
	edgeURL, _ := url.Parse(edge.URL)
	bound := edgeEgressTarget{NodeID: nodeID, PublicURL: edge.URL, Token: token}

	issuer := newStructuredDiscoveryTestIssuer(t)
	trusted, err := normalizeTrustedCapabilityURL(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	issuer.primaryAuthority = redirectHostKey(trusted)
	issuer.configuredAuthorities = map[string]bool{issuer.primaryAuthority: true}
	issuer.configuredTransport = &edgeEgressTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == edgeURL.Host && req.URL.Path == edgeEgressPath {
			egressHits.Add(1)
		} else {
			masterDirectHits.Add(1)
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	issuer.transportFactory = func(*url.URL, []net.IP, *dynamicSelfTargetPolicy) (*http.Transport, error) {
		masterDirectHits.Add(1)
		return nil, fmt.Errorf("bound manifest attempted master-side transport")
	}
	issuer.site.EdgeRoutingMode = edgeRoutingModeEdgeFull
	node := &edgeNodeLiveState{lastSeen: time.Now(), supportsDynamicGuard: true}
	issuer.pm = &ProxyManager{edges: &edgeNodeRuntime{nodes: map[int64]*edgeNodeLiveState{nodeID: node}}}

	mint := func(target string, trusted bool) string {
		t.Helper()
		parsed, parseErr := normalizeTrustedCapabilityURL(target)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		var route string
		var ok bool
		var err *dynamicProxyError
		if trusted {
			route, ok, err = issuer.mintTrustedValidatedTracked(parsed, parsed.String(), nil, nil, dynamicDiscoverySourceHLS, dynamicCapabilityKindManifest, 1)
		} else {
			route, ok, err = issuer.mintValidatedResourceWithRequiredHeadersTracked(context.Background(), nil, parsed, dynamicDiscoverySourceHLS, parsed.String(), nil, nil, nil, dynamicCapabilityKindManifest, 1)
		}
		if err != nil || !ok {
			t.Fatalf("mint manifest capability = %q, %v, %v", route, ok, err)
		}
		if !issuer.state.settleCapabilities([]string{capabilityTokenFromRoute(t, route)}, true, time.Now()) {
			t.Fatal("could not publish manifest capability")
		}
		return route
	}
	serve := func(route string, useEdge bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("Range", "bytes=0-3")
		req.Header.Set("Authorization", "Bearer fixture")
		ctx := withEdgeRequestContext(req.Context(), "", nil, "", true, edgeRouteDecision{})
		if useEdge {
			ctx = withEdgeEgressTarget(ctx, bound)
		}
		rec := httptest.NewRecorder()
		issuer.serve(rec, req.WithContext(ctx))
		return rec
	}
	entryRoute := mint(origin.URL+"/entry.m3u8", true)
	guardedRoute := mint(cdnURL+"/branch/live.m3u8", false)
	directRoute := mint(origin.URL+"/master.m3u8", true)

	rec := serve(entryRoute, true)
	if rec.Code != http.StatusOK || egressHits.Load() != 2 || originHits.Load() != 1 || guardedHits.Load() != 1 || masterDirectHits.Load() != 0 {
		t.Fatalf("bound redirect status=%d edge=%d origin=%d guarded=%d master=%d body=%q", rec.Code, egressHits.Load(), originHits.Load(), guardedHits.Load(), masterDirectHits.Load(), rec.Body.String())
	}
	children := structuredCapabilityRoutes(rec.Body.String())
	if len(children) != 1 {
		t.Fatalf("rewritten manifest has %d children, want one: %q", len(children), rec.Body.String())
	}
	child, err := openDynamicCapability(issuer.key, capabilityTokenFromRoute(t, children[0]))
	if err != nil || child.Target != cdnURL+"/branch/relative.ts" {
		t.Fatalf("manifest child target = %q, error = %v", child.Target, err)
	}

	node.supportsDynamicGuard = false
	for _, test := range []struct {
		route      string
		wantEgress int32
	}{
		{guardedRoute, 2},
		{entryRoute, 3},
	} {
		rec = serve(test.route, true)
		if rec.Code != http.StatusBadGateway || egressHits.Load() != test.wantEgress || guardedHits.Load() != 1 || masterDirectHits.Load() != 0 {
			t.Fatalf("legacy bound status=%d edge=%d guarded=%d master=%d body=%q", rec.Code, egressHits.Load(), guardedHits.Load(), masterDirectHits.Load(), rec.Body.String())
		}
	}
	if rec = serve(directRoute, true); rec.Code != http.StatusOK || egressHits.Load() != 4 || masterDirectHits.Load() != 0 {
		t.Fatalf("trusted manifest over legacy-compatible egress status=%d edge=%d master=%d body=%q", rec.Code, egressHits.Load(), masterDirectHits.Load(), rec.Body.String())
	}
	if rec = serve(directRoute, false); rec.Code != http.StatusOK || masterDirectHits.Load() != 1 || !strings.Contains(rec.Body.String(), dynamicRoutePrefix) {
		t.Fatalf("master-bound manifest status=%d master=%d body=%q", rec.Code, masterDirectHits.Load(), rec.Body.String())
	}
}
