package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type playbackProxyFallbackPayload struct {
	MediaSources []struct {
		DirectStreamURL string `json:"DirectStreamUrl"`
		TranscodingURL  string `json:"TranscodingUrl"`
		Path            string `json:"Path"`
		Protocol        string `json:"Protocol"`
		MediaStreams    []struct {
			DeliveryURL string `json:"DeliveryUrl"`
		} `json:"MediaStreams"`
	} `json:"MediaSources"`
}

func rewritePlaybackProxyFallbackResponse(t *testing.T, issuer *dynamicCapabilityIssuer, body string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://line.example.com/Items/1/PlaybackInfo", nil)
	if err != nil {
		t.Fatalf("build PlaybackInfo request: %v", err)
	}
	clientBase := mustStructuredURL(t, "http://public.example.com/Items/1/PlaybackInfo")
	request = request.WithContext(context.WithValue(request.Context(), dynamicPlaybackInfoBaseContextKey{}, clientBase))
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
	return response, rewriteDynamicStructuredResponseExpected(response, issuer, false, dynamicDiscoverySourcePlaybackInfo, 0, false)
}

func readPlaybackProxyFallbackPayload(t *testing.T, response *http.Response) playbackProxyFallbackPayload {
	t.Helper()
	if response.Body == nil {
		t.Fatal("PlaybackInfo response body is unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read PlaybackInfo response: %v", err)
	}
	var decoded playbackProxyFallbackPayload
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode PlaybackInfo response: %v", err)
	}
	return decoded
}

func assertNoPlaybackProxyFallbackCapabilities(t *testing.T, issuer *dynamicCapabilityIssuer) {
	t.Helper()
	issuer.state.runtime.mu.Lock()
	defer issuer.state.runtime.mu.Unlock()
	issuer.state.mu.Lock()
	defer issuer.state.mu.Unlock()
	if len(issuer.state.capabilities) != 0 || issuer.state.runtime.activeCapabilities != 0 {
		t.Fatalf("rejected PlaybackInfo leaked capabilities: entries=%d active=%d", len(issuer.state.capabilities), issuer.state.runtime.activeCapabilities)
	}
}

func TestPlaybackProxyFallbackProxiesExternalMainVideo(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	body := `{"MediaSources":[{"DirectStreamUrl":"https://cdn.example.com/video.mp4","MediaStreams":[{"DeliveryUrl":"http://backend.invalidtld/subtitle.vtt","IsExternalUrl":true}]}]}`
	response, err := rewritePlaybackProxyFallbackResponse(t, issuer, body)
	if err != nil {
		t.Fatalf("rewrite PlaybackInfo with external main video: %v", err)
	}
	payload := readPlaybackProxyFallbackPayload(t, response)
	if len(payload.MediaSources) != 1 {
		t.Fatalf("unexpected MediaSources: %#v", payload.MediaSources)
	}
	main := payload.MediaSources[0]
	if !strings.HasPrefix(main.DirectStreamURL, dynamicRoutePrefix) || strings.Contains(main.DirectStreamURL, "cdn.example.com") {
		t.Fatalf("external main video was not proxied: %q", main.DirectStreamURL)
	}
	claims, err := openDynamicCapability(issuer.key, capabilityTokenFromRoute(t, main.DirectStreamURL))
	if err != nil {
		t.Fatalf("open main video capability: %v", err)
	}
	if claims.Target != "https://cdn.example.com/video.mp4" {
		t.Fatalf("main video capability target = %q", claims.Target)
	}
	if len(main.MediaStreams) != 1 || main.MediaStreams[0].DeliveryURL != "http://backend.invalidtld/subtitle.vtt" {
		t.Fatalf("optional invalid subtitle was not preserved: %#v", main.MediaStreams)
	}
}

func TestPlaybackProxyFallbackRejectsUnproxiableMainVideo(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "DirectStreamUrl", body: `{"MediaSources":[{"DirectStreamUrl":"https://backend.invalidtld/video.mp4"}]}`},
		{name: "DirectStreamUrlWithUserinfo", body: `{"MediaSources":[{"DirectStreamUrl":"https://user:secret@backend.invalidtld/video.mp4"}]}`},
		{name: "TranscodingUrl", body: `{"MediaSources":[{"TranscodingUrl":"https://backend.invalidtld/video.mp4"}]}`},
		{name: "HTTPPath", body: `{"MediaSources":[{"Protocol":"Http","Path":"https://backend.invalidtld/video.mp4"}]}`},
		{name: "HTTPPathWithFragment", body: `{"MediaSources":[{"Protocol":"Http","Path":"https://backend.invalidtld/video.mp4#fragment"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			issuer := newStructuredDiscoveryTestIssuer(t)
			response, err := rewritePlaybackProxyFallbackResponse(t, issuer, test.body)
			if err == nil {
				t.Fatal("unproxiable main video URL was accepted")
			}
			var denied *dynamicProxyError
			if !errors.As(err, &denied) || denied.reasonCode != dynamicObservationReasonPlaybackInfoDenied {
				t.Fatalf("main video rejection = %T %v, want playback_info_denied", err, err)
			}
			if response.Body != nil {
				_ = response.Body.Close()
			}
			assertNoPlaybackProxyFallbackCapabilities(t, issuer)
		})
	}
}

func TestPlaybackProxyFallbackPreservesRelativeAndLocalPaths(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		wantPath    string
		wantLearned string
	}{
		{
			name:        "relative main video",
			body:        `{"MediaSources":[{"DirectStreamUrl":"/Videos/1/original.mkv?token=relative-secret","MediaStreams":[{"DeliveryUrl":"http://backend.invalidtld/subtitle.vtt","IsExternalUrl":true}]}]}`,
			wantLearned: "/Videos/1/original.mkv",
		},
		{
			name:     "local File path",
			body:     `{"MediaSources":[{"Protocol":"File","DirectStreamUrl":"/Videos/1/stream.mp4","Path":"C:\\Videos\\movie.mkv","MediaStreams":[{"DeliveryUrl":"http://backend.invalidtld/subtitle.vtt","IsExternalUrl":true}]}]}`,
			wantPath: `C:\Videos\movie.mkv`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			issuer := newStructuredDiscoveryTestIssuer(t)
			response, err := rewritePlaybackProxyFallbackResponse(t, issuer, test.body)
			if err != nil {
				t.Fatalf("rewrite PlaybackInfo: %v", err)
			}
			payload := readPlaybackProxyFallbackPayload(t, response)
			if len(payload.MediaSources) != 1 {
				t.Fatalf("unexpected MediaSources: %#v", payload.MediaSources)
			}
			main := payload.MediaSources[0]
			if test.wantPath == "" {
				if main.DirectStreamURL != "/Videos/1/original.mkv?token=relative-secret" {
					t.Fatalf("relative main video changed to %q", main.DirectStreamURL)
				}
				if !issuer.state.hasLearnedPlaybackPath(test.wantLearned, time.Now()) {
					t.Fatalf("relative main video path %q was not learned", test.wantLearned)
				}
				return
			}
			if main.Protocol != "File" || main.Path != test.wantPath {
				t.Fatalf("local File path changed: protocol=%q path=%q", main.Protocol, main.Path)
			}
			if main.DirectStreamURL != "/Videos/1/stream.mp4" {
				t.Fatalf("relative main video changed to %q", main.DirectStreamURL)
			}
		})
	}
}

func TestPlaybackProxyFallbackRollsBackCapabilitiesWhenMainVideoFails(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	body := `{"MediaSources":[{"DirectStreamUrl":"https://cdn.example.com/video.mp4","MediaStreams":[{"DeliveryUrl":"http://backend.invalidtld/subtitle.vtt","IsExternalUrl":true}]},{"DirectStreamUrl":"https://backend.invalidtld/video.mp4"}]}`
	response, err := rewritePlaybackProxyFallbackResponse(t, issuer, body)
	if err == nil {
		t.Fatal("PlaybackInfo with an unproxiable main video was accepted")
	}
	var denied *dynamicProxyError
	if !errors.As(err, &denied) || denied.reasonCode != dynamicObservationReasonPlaybackInfoDenied {
		t.Fatalf("main video rejection = %T %v, want playback_info_denied", err, err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	assertNoPlaybackProxyFallbackCapabilities(t, issuer)
}
