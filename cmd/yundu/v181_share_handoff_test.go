package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestV181PersonalHandoffCommitAndReload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "account deleted", status: http.StatusUnauthorized},
		{name: "client logs out", status: http.StatusUnauthorized},
		{name: "target permission removed before reload", status: http.StatusServiceUnavailable},
		{name: "target instance replaced", status: http.StatusServiceUnavailable},
		{name: "database write fails", status: http.StatusServiceUnavailable},
		{name: "reload waits for commit"},
		{name: "commit waits for reload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			db, rt := app.db, app.pm.shares
			siteA, err := db.CreateSiteRecord(Site{Name: "A", ListenPort: 20401, IngressMode: ingressModePath, PathPrefix: "/a", TargetURL: "http://127.0.0.1:8096", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			siteB, err := db.CreateSiteRecord(Site{Name: "B", ListenPort: 20402, IngressMode: ingressModePath, PathPrefix: "/b", TargetURL: "http://127.0.0.1:8097", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.saveSiteAccount(siteB.ID, 0, SiteAccountInput{Username: "upstream-b", Password: "password"}); err != nil {
				t.Fatal(err)
			}
			user := createDanmakuTestShareUser(t, app, siteA.ID, "handoff-v181", "", true, 0, nil)
			if _, err := db.db.Exec(`INSERT INTO share_user_sites(share_user_id, site_id, sort_order) VALUES(?, ?, 2)`, user.ID, siteB.ID); err != nil {
				t.Fatal(err)
			}
			if err := rt.reload(); err != nil {
				t.Fatal(err)
			}
			pending := &shareLoginPending{user: rt.users[user.ID], siteID: siteA.ID}
			const oldToken, newToken = "v181-old", "v181-new"
			if err := rt.registerSession(pending, oldToken, "uid-a"); err != nil {
				t.Fatal(err)
			}
			hash := shareTokenHash(oldToken)
			started, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			resumeLogin := func() { resumeOnce.Do(func() { close(resume) }) }
			t.Cleanup(resumeLogin)
			logouts := make(chan string, 2)
			logout := func(_ context.Context, token string) error {
				logouts <- token
				return nil
			}
			app.pm.mu.Lock()
			app.pm.proxies[siteA.ID] = &ProxyInstance{Site: *siteA, shareLogout: logout}
			app.pm.proxies[siteB.ID] = &ProxyInstance{
				Site: *siteB,
				shareLogin: func(ctx context.Context, _, _, _, _, _ string) (string, string, int, error) {
					close(started)
					select {
					case <-resume:
						return newToken, "uid-b", http.StatusOK, nil
					case <-ctx.Done():
						return "", "", 0, ctx.Err()
					}
				},
				shareLogout: logout,
			}
			app.pm.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan int, 1)
			go func() {
				status, _ := rt.handoffPersonalRouteSite(ctx, hash, siteB.ID, "")
				result <- status
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("handoff did not start target login")
			}
			var reloaded chan error
			switch tc.name {
			case "account deleted":
				if _, err := db.db.Exec(`DELETE FROM share_users WHERE id=?`, user.ID); err != nil {
					t.Fatal(err)
				}
				if err := rt.reload(); err != nil {
					t.Fatal(err)
				}
			case "client logs out":
				req := httptest.NewRequest(http.MethodPost, "/Sessions/Logout", nil)
				req.Header.Set("X-Emby-Token", oldToken)
				res := httptest.NewRecorder()
				if !rt.handleShareClientLogout(res, req, siteA.ID) || res.Code != http.StatusNoContent {
					t.Fatalf("explicit logout status = %d", res.Code)
				}
				if err := rt.reload(); err != nil {
					t.Fatal(err)
				}
			case "target permission removed before reload":
				if _, err := db.db.Exec(`DELETE FROM share_user_sites WHERE share_user_id=? AND site_id=?`, user.ID, siteB.ID); err != nil {
					t.Fatal(err)
				}
			case "target instance replaced":
				app.pm.mu.Lock()
				app.pm.proxies[siteB.ID] = &ProxyInstance{Site: *siteB}
				app.pm.mu.Unlock()
			case "database write fails":
				if _, err := db.db.Exec(`CREATE TRIGGER deny_handoff BEFORE UPDATE OF site_id ON share_user_sessions
					BEGIN SELECT RAISE(ABORT, 'test session write failure'); END`); err != nil {
					t.Fatal(err)
				}
			default:
				// Block SQLite access after the target has accepted the login.
				// Depending on who takes the store lock first, either the whole
				// reload or the whole commit must wait for the other.
				conn, err := db.db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				reloaded = make(chan error, 1)
				reload := func() { reloaded <- rt.reload() }
				if tc.name == "reload waits for commit" {
					resumeLogin()
					waitForV181SessionStoreLock(t, ctx, rt)
					go reload()
				} else {
					go reload()
					waitForV181SessionStoreLock(t, ctx, rt)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			}
			resumeLogin()
			select {
			case status := <-result:
				if status != tc.status {
					t.Fatalf("handoff status = %d, want %d", status, tc.status)
				}
			case <-ctx.Done():
				t.Fatal("handoff did not return")
			}
			if reloaded != nil {
				select {
				case err := <-reloaded:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("reload did not return")
				}
			}

			wantSite, wantToken, wantLogout := siteB.ID, newToken, oldToken
			if tc.status != 0 {
				wantSite, wantToken, wantLogout = siteA.ID, oldToken, newToken
			}
			expectedLogouts := map[string]bool{wantLogout: true}
			if tc.name == "client logs out" {
				expectedLogouts[oldToken] = true
			}
			for len(expectedLogouts) > 0 {
				select {
				case got := <-logouts:
					if !expectedLogouts[got] {
						t.Fatalf("logged out unexpected token %q", got)
					}
					delete(expectedLogouts, got)
				case <-ctx.Done():
					t.Fatal("upstream logout did not run")
				}
			}
			rt.mu.RLock()
			live, exists := rt.sessions[hash]
			rt.mu.RUnlock()
			var storedSite int64
			var storedUpstreamCipher string
			err = db.db.QueryRow(`SELECT site_id, COALESCE(NULLIF(upstream_token_ciphertext, ''), token_ciphertext) FROM share_user_sessions WHERE token_hash=?`, hash).
				Scan(&storedSite, &storedUpstreamCipher)
			if tc.status == http.StatusUnauthorized {
				if exists || !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("deleted account left a session: exists=%t, DB error=%v", exists, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			storedToken, err := decryptSiteAccountSecret(storedUpstreamCipher)
			if err != nil || storedSite != wantSite || storedToken != wantToken {
				t.Fatalf("stored site/token = %d/%q, want %d/%q (error %v)", storedSite, storedToken, wantSite, wantToken, err)
			}
			if !exists || live.siteID != wantSite || live.upstreamToken != wantToken {
				t.Fatalf("live site/token = %d/%q, want %d/%q (exists %t)", live.siteID, live.upstreamToken, wantSite, wantToken, exists)
			}
		})
	}
}

func waitForV181SessionStoreLock(t *testing.T, ctx context.Context, rt *shareUserRuntime) {
	t.Helper()
	for rt.sessionStoreMu.TryLock() {
		rt.sessionStoreMu.Unlock()
		if ctx.Err() != nil {
			t.Fatal("operation did not acquire the session store lock")
		}
		runtime.Gosched()
	}
}
