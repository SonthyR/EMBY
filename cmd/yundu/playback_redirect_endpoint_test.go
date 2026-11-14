package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPlaybackRedirectEndpointRecognition(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/emya/video", want: true},
		{path: "/emby/emya/video", want: true},
		{path: "/EMBY/EMYA/VIDEO", want: true},
		{path: "/play/video/example-id", want: true},
		{path: "/emby/play/video/example-id", want: true},
		{path: "/EMBY/PLAY/VIDEO/01M5ABC", want: true},
		{path: "/play/video/01M5.abc-def_1", want: true},
		{path: "/play/video", want: false},
		{path: "/play/video/", want: false},
		{path: "/play/video/example-id/", want: false},
		{path: "/play/video/example-id/extra", want: false},
		{path: "/play/video//example-id", want: false},
		{path: "/emby/play/video", want: false},
		{path: "/emby/play/video/", want: false},
		{path: "/emby/play/video/example-id/", want: false},
		{path: "/emby/play/video/example-id/extra", want: false},
		{path: "/emby/play/video//example-id", want: false},
		{path: "/x/play/video/example-id", want: false},
		{path: "/x/emby/play/video/example-id", want: false},
		{path: "/play/videos/example-id", want: false},
		{path: "/emby/play/example-id", want: false},
		{path: "/Users/AuthenticateByName", want: false},
		{path: "/emby/Users/AuthenticateByName", want: false},
	} {
		if got := isPlaybackRedirectEndpoint(test.path); got != test.want {
			t.Errorf("isPlaybackRedirectEndpoint(%q) = %t, want %t", test.path, got, test.want)
		}
	}
}

func TestPlaybackRedirectEndpointDynamicEligibilityAndLogging(t *testing.T) {
	for _, path := range []string{"/play/video/example-id", "/emby/play/video/01M5ABC"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			request := httptest.NewRequest(method, "http://line.example.com"+path, nil)
			if !isDynamicRedirectEligibleRequest(request) {
				t.Fatalf("%s %s was not dynamically eligible", method, path)
			}
			if !shouldInternallyFollowDynamicRedirect(request) {
				t.Fatalf("%s %s was not internally followed", method, path)
			}
			if got := classifyRequestLogResource(request); got != requestLogCategoryStream {
				t.Fatalf("%s %s log category = %q, want %q", method, path, got, requestLogCategoryStream)
			}
		}
		request := httptest.NewRequest(http.MethodPost, "http://line.example.com"+path, nil)
		if isDynamicRedirectEligibleRequest(request) || shouldInternallyFollowDynamicRedirect(request) {
			t.Fatalf("POST %s was treated as a playback redirect", path)
		}
	}

	for _, path := range []string{
		"/play/video",
		"/play/video/",
		"/play/video/example-id/extra",
		"/emby/play/video/example-id/extra",
		"/Users/AuthenticateByName",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://line.example.com"+path, nil)
		if isDynamicRedirectEligibleRequest(request) || shouldInternallyFollowDynamicRedirect(request) {
			t.Fatalf("ordinary request %q was treated as a playback redirect", path)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://line.example.com/Users/AuthenticateByName", nil)
	if got := classifyRequestLogResource(request); got == requestLogCategoryStream {
		t.Fatalf("ordinary API request was classified as %q", got)
	}
}

func startPlaybackRedirectEndpointTestSite(t *testing.T, app *App, targetURL, dnsHost string, factory dynamicTransportFactory) (*Site, int) {
	t.Helper()
	app.pm.dynamicRuntime.resolver = dynamicIPResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != dnsHost {
			return nil, fmt.Errorf("unexpected DNS host %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	app.pm.dynamicTransportFactory = factory
	port := freePort(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:                    "playback-redirect-e2e",
		ListenPort:              port,
		IngressMode:             ingressModePort,
		TargetURL:               targetURL,
		PlaybackMode:            "direct",
		StreamHosts:             "[]",
		UAMode:                  "infuse",
		MainVideoStreamMode:     mainVideoStreamModeProxy,
		DynamicDiscoveryEnabled: true,
		DynamicProfile:          dynamicProfileCompatible,
		DynamicDiscoverySources: allDynamicDiscoverySources(),
	})
	if err != nil {
		t.Fatalf("create playback redirect site: %v", err)
	}
	key := bytes.Repeat([]byte{0x6d}, 32)
	if err := app.pm.ConfigureDynamicDiscovery(key, "panel.example.com", 9090, func() ([]net.Addr, error) { return nil, nil }); err != nil {
		t.Fatalf("configure dynamic discovery: %v", err)
	}
	app.dynamicRouteKey = append([]byte(nil), key...)
	releasePort(port)
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("start playback redirect site: %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	return site, port
}

func TestPlaybackRedirectEndpointFollowsCDNEndToEnd(t *testing.T) {
	app := newTestApp(t)
	videoBody := "video-body"
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/play/video/example-id" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", "https://cdn.example.com/video.mp4?sig=origin-secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer apiUpstream.Close()

	captures := make(chan redirectRuntimeDialCapture, 1)
	site, port := startPlaybackRedirectEndpointTestSite(t, app, apiUpstream.URL, "cdn.example.com", redirectRuntimeFactory(captures, func(*http.Request) string {
		return fmt.Sprintf("HTTP/1.1 206 Partial Content\r\nContent-Type: video/mp4\r\nContent-Length: %d\r\nContent-Range: bytes 0-9/20\r\nAccept-Ranges: bytes\r\nSet-Cookie: origin=secret\r\n\r\n%s", len(videoBody), videoBody)
	}))

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/emby/play/video/example-id", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-9")
	request.Header.Set("Cookie", "meridian_session=must-not-cross")
	request.Header.Set("Authorization", "Bearer must-not-cross")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request playback redirect endpoint: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusPartialContent || string(body) != videoBody {
		t.Fatalf("playback redirect response status=%d read=%v body=%q location=%q", response.StatusCode, readErr, body, response.Header.Get("Location"))
	}
	if response.Header.Get("Location") != "" || response.Header.Get("Set-Cookie") != "" {
		t.Fatalf("playback redirect leaked external headers: %#v", response.Header)
	}

	select {
	case capture := <-captures:
		if capture.err != nil || capture.address != "1.1.1.1:443" || capture.request.URL.RequestURI() != "/video.mp4?sig=origin-secret" {
			t.Fatalf("CDN capture = %#v", capture)
		}
		if capture.request.Header.Get("Range") != "bytes=0-9" {
			t.Fatalf("CDN Range header = %q", capture.request.Header.Get("Range"))
		}
		if capture.request.Header.Get("Cookie") != "" || capture.request.Header.Get("Authorization") != "" {
			t.Fatalf("credentials crossed to the CDN: %#v", capture.request.Header)
		}
	case <-time.After(time.Second):
		t.Fatal("playback redirect endpoint did not dial the CDN transport")
	}

	instance := app.pm.proxies[site.ID]
	if instance == nil {
		t.Fatal("playback redirect site has no running instance")
	}
	if got := instance.bytesOut.Load(); got < int64(len(videoBody)) {
		t.Fatalf("traffic meter bytesOut = %d, want at least %d", got, len(videoBody))
	}
}

func TestPlaybackRedirectEndpointLeavesOrdinaryAPIRedirects(t *testing.T) {
	app := newTestApp(t)
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://cdn.example.com/campaign")
		w.WriteHeader(http.StatusFound)
	}))
	defer apiUpstream.Close()

	captures := make(chan redirectRuntimeDialCapture, 1)
	_, port := startPlaybackRedirectEndpointTestSite(t, app, apiUpstream.URL, "cdn.example.com", redirectRuntimeFactory(captures, func(*http.Request) string {
		return "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	}))

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/Users/AuthenticateByName", port))
	if err != nil {
		t.Fatalf("request ordinary API redirect: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "https://cdn.example.com/campaign" {
		t.Fatalf("ordinary API redirect was intercepted: status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
	select {
	case capture := <-captures:
		t.Fatalf("ordinary API redirect reached the CDN transport: %#v", capture)
	default:
	}
}
