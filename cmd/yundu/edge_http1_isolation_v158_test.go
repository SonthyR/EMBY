package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestEdgeAgentUpstreamDisablesHTTP2AndIsolatesConcurrentStreams(t *testing.T) {
	var (
		mu          sync.Mutex
		protos      []int
		remoteAddrs = make(map[string]bool)
	)
	stream1Started := make(chan struct{})
	releaseStream1 := make(chan struct{})

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.ProtoMajor)
		remoteAddrs[r.RemoteAddr] = true
		mu.Unlock()

		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)

		if r.URL.Path == "/slow" {
			_, _ = w.Write(bytes.Repeat([]byte("A"), 64<<10))
			if flusher != nil {
				flusher.Flush()
			}
			close(stream1Started)
			select {
			case <-releaseStream1:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(bytes.Repeat([]byte("B"), 64<<10))
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("C"), 256<<10))
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	agent := newEdgeAgent(edgeAgentConfig{
		MasterURL: "https://master.example.com",
		NodeID:    9,
		Token:     "test-token-v158",
		Port:      18080,
	})
	tr, ok := agent.upstreamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport on upstreamClient")
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatalf("expected ForceAttemptHTTP2=false on edge upstreamTransport")
	}
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Fatalf("expected non-nil empty TLSNextProto to force HTTP/1.1, got %v", tr.TLSNextProto)
	}
	if !tr.DisableCompression {
		t.Fatalf("expected DisableCompression=true on edge upstreamTransport")
	}
	if tr.ReadBufferSize < 128<<10 || tr.WriteBufferSize < 128<<10 {
		t.Fatalf("expected >=128KB read/write buffers, got read=%d write=%d", tr.ReadBufferSize, tr.WriteBufferSize)
	}

	// Trust only the test CA while preserving all HTTP/1.1 isolation & TLS settings.
	tr.TLSClientConfig.RootCAs = upstream.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	agentSrv := httptest.NewServer(agent.handler())
	defer agentSrv.Close()

	makeTicketURL := func(path string) string {
		sealed, err := sealEdgePlaybackTicket(agent.cfg.Token, edgePlaybackTicket{
			NodeID:    agent.cfg.NodeID,
			SiteID:    1,
			TargetURL: upstream.URL + path,
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatalf("seal ticket: %v", err)
		}
		return agentSrv.URL + edgeTicketPathPrefix + sealed
	}

	// Start stream 1 and pause reading from it (simulates one slow/back-pressured client).
	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, makeTicketURL("/slow"), nil)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("stream 1 request failed: %v", err)
	}
	defer resp1.Body.Close()
	select {
	case <-stream1Started:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for stream 1 to reach upstream /slow")
	}

	// While stream 1 is held open mid-transfer, stream 2 MUST use a separate HTTP/1.1 TCP
	// connection and complete cleanly without any HTTP/2 cross-stream blockage.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, makeTicketURL("/fast"), nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("stream 2 request failed: %v", err)
	}
	body2, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		t.Fatalf("read stream 2: %v", err)
	}
	if len(body2) != 256<<10 {
		t.Fatalf("expected 256KiB from stream 2, got %d", len(body2))
	}

	close(releaseStream1)
	body1, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatalf("read stream 1: %v", err)
	}
	if len(body1) != 128<<10 {
		t.Fatalf("expected 128KiB from stream 1, got %d", len(body1))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(protos) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(protos))
	}
	for i, p := range protos {
		if p != 1 {
			t.Fatalf("upstream request #%d used HTTP/%d, want HTTP/1.1", i+1, p)
		}
	}
	if len(remoteAddrs) != 2 {
		t.Fatalf("expected 2 distinct upstream TCP connections for concurrent streams, got %d (%v)", len(remoteAddrs), remoteAddrs)
	}
}
