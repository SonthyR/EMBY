package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlaybackTitleReportPreservesBody(t *testing.T) {
	const body = `{"ItemId":"episode-3","PlaySessionId":"p3","OtherField":true}`
	req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress", strings.NewReader(body))
	report, ok := readPlaybackTitleReport(req)
	if !ok || report.identity.itemID != "episode-3" || report.identity.playID != "p3" || report.start || report.stop {
		t.Fatalf("report = %+v, parsed=%t", report, ok)
	}
	forwarded, err := io.ReadAll(req.Body)
	if err != nil || string(forwarded) != body {
		t.Fatalf("forwarded report changed: %q, %v", forwarded, err)
	}
}

func TestPlaybackTitleUsesCurrentSourceAccount(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/emby/Users/real-user/Items/ep-3" || r.Header.Get("X-Emby-Token") != "upstream-session-token" {
			t.Errorf("unexpected metadata request: method=%s path=%s authenticated=%t", r.Method, r.URL.Path, r.Header.Get("X-Emby-Token") != "")
		}
		_, _ = io.WriteString(w, `{"Type":"Episode","Name":"Episode title","SeriesName":"三体","ParentIndexNumber":1,"IndexNumber":3}`)
	}))
	defer origin.Close()
	target, err := url.Parse(origin.URL + "/emby")
	if err != nil {
		t.Fatal(err)
	}
	fetch := newPlaybackTitleFetchFunc(origin.Client().Transport, target, UAHeaderPolicy{}, upstreamHeaderPolicy{})
	if got := fetch(context.Background(), "real-user", "upstream-session-token", "ep-3", ""); got != "三体 S01E03" {
		t.Fatalf("title = %q", got)
	}
}

func TestPlaybackTitleStateRejectsStaleReports(t *testing.T) {
	now := time.Now()
	ep3 := playbackTitleIdentity{itemID: "ep-3", playID: "p3"}
	ep4 := playbackTitleIdentity{itemID: "ep-4", playID: "p4"}
	var state sharePlaybackTitleState
	if !state.apply(playbackTitleReport{identity: ep3, start: true}, now) {
		t.Fatal("start episode 3 rejected")
	}
	old := state.current
	if !state.apply(playbackTitleReport{identity: ep4, start: true}, now.Add(time.Second)) {
		t.Fatal("start episode 4 rejected")
	}
	if old == state.current || state.current.identity != ep4 {
		t.Fatal("new episode did not replace prior playback")
	}
	if state.apply(playbackTitleReport{identity: ep3}, now.Add(2*time.Second)) ||
		state.apply(playbackTitleReport{identity: ep3, stop: true}, now.Add(3*time.Second)) ||
		state.apply(playbackTitleReport{identity: ep3, start: true}, now) {
		t.Fatal("old progress, stop or reordered start changed the current episode")
	}
	if !state.apply(playbackTitleReport{identity: ep4, stop: true}, now.Add(4*time.Second)) || state.current != nil {
		t.Fatal("stopping the current episode did not clear the title")
	}
	if state.apply(playbackTitleReport{identity: ep4}, now.Add(5*time.Second)) {
		t.Fatal("late progress restored the stopped episode")
	}
}

func TestPlaybackTitleDashboardThroughShareProxy(t *testing.T) {
	app := newTestApp(t)
	oldLookupStarted := make(chan struct{}, 1)
	releaseOldLookup := make(chan struct{}, 1)
	var episode4Lookups atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AccessToken":"upstream-token-1","User":{"Id":"real-user","Name":"real-user"}}`)
		case "/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped":
			w.WriteHeader(http.StatusNoContent)
		case "/Users/real-user/Items/ep-3":
			oldLookupStarted <- struct{}{}
			select {
			case <-releaseOldLookup:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `{"Type":"Episode","SeriesName":"三体","ParentIndexNumber":1,"IndexNumber":3}`)
		case "/Users/real-user/Items/ep-4":
			episode4Lookups.Add(1)
			_, _ = io.WriteString(w, `{"Type":"Episode","SeriesName":"三体","ParentIndexNumber":1,"IndexNumber":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	site, base := startShareTestSite(t, app, origin.URL)
	createShareUserForTest(t, app, site.ID, 0)
	if response, body := doShareLogin(t, base, "friend", "friend-pass"); response.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d body=%s", response.StatusCode, body)
	}

	report := func(path, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Token", "upstream-token-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("playback report status = %d", resp.StatusCode)
		}
	}
	visibleTitle := func() string {
		t.Helper()
		sessions, err := app.pm.shares.listAllLiveSessions()
		if err != nil || len(sessions) != 1 {
			t.Fatalf("live sessions = %d, err=%v", len(sessions), err)
		}
		return sessions[0].NowPlayingTitle
	}
	waitFor := func(condition func() bool, description string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", description)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	const ep3 = `{"ItemId":"ep-3","PlaySessionId":"p3"}`
	const ep4 = `{"ItemId":"ep-4","PlaySessionId":"p4"}`
	report("/Sessions/Playing", ep3)
	select {
	case <-oldLookupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("episode 3 lookup never started")
	}
	report("/Sessions/Playing", ep4)
	waitFor(func() bool { return visibleTitle() == "三体 S01E04" }, "episode 4 title")
	report("/Sessions/Playing/Progress", ep4)
	report("/Sessions/Playing/Progress", ep3)
	report("/Sessions/Playing/Stopped", ep3)
	releaseOldLookup <- struct{}{}
	waitFor(func() bool { return len(app.pm.shares.playbackTitleSlots) == 0 }, "all lookups to settle")
	if title := visibleTitle(); title != "三体 S01E04" || episode4Lookups.Load() != 1 {
		t.Fatalf("title=%q metadata calls=%d after late episode 3 events", title, episode4Lookups.Load())
	}
	report("/Sessions/Playing/Stopped", ep4)
	if title := visibleTitle(); title != "" {
		t.Fatalf("stopped playback shows %q", title)
	}
}

func TestPlaybackTitlePreservesEdgeEgressAndDiscardsResultAfterSwitch(t *testing.T) {
	rt := newShareUserRuntime(nil, nil)
	rt.activity["session"] = &shareSessionActivity{}
	rt.sessions["session"] = shareSessionEntry{siteID: 1, upstreamUID: "old-user", upstreamToken: "old-token"}
	started := make(chan edgeEgressTarget, 1)
	release := make(chan struct{}, 1)
	inst := &ProxyInstance{
		Site: Site{ID: 1}, ctx: context.Background(),
		fetchPlaybackTitle: func(ctx context.Context, _, _, _, _ string) string {
			target, _ := edgeEgressTargetFrom(ctx)
			started <- target
			select {
			case <-release:
				return "old source movie"
			case <-ctx.Done():
				return ""
			}
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing", strings.NewReader(`{"ItemId":"old-video"}`))
	report, ok := readPlaybackTitleReport(req)
	if !ok {
		t.Fatal("no report")
	}
	report.at = time.Now()
	ctx := withEdgeEgressTarget(req.Context(), edgeEgressTarget{NodeID: 7, PublicURL: "https://edge.example.com", Token: "edge-token"})
	ctx = context.WithValue(ctx, playbackTitleReportKey{}, report)
	observe := rt.preparePlaybackTitleReport(inst, "session", req.WithContext(ctx))
	if observe == nil {
		t.Fatal("no observation")
	}
	observe()
	select {
	case edge := <-started:
		if edge.NodeID != 7 {
			t.Fatalf("lookup used edge #%d instead of #7", edge.NodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("lookup never started")
	}

	rt.mu.Lock()
	rt.sessions["session"] = shareSessionEntry{siteID: 2, upstreamUID: "new-user", upstreamToken: "new-token"}
	retired := rt.activity["session"].mediaTitle.current
	rt.activity["session"].mediaTitle = sharePlaybackTitleState{}
	rt.mu.Unlock()
	release <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	for len(rt.playbackTitleSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if len(rt.playbackTitleSlots) > 0 || retired.title != "" || rt.activity["session"].mediaTitle.current != nil {
		t.Fatal("late metadata from the previous source was applied")
	}
}
