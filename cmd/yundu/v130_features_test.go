package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShareMultiAccountLoadBalanceAndFailover(t *testing.T) {
	var mu sync.Mutex
	var loginUsers []string
	failAcc1Status := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Users/AuthenticateByName") {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Username string `json:"Username"`
				Pw       string `json:"Pw"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			loginUsers = append(loginUsers, body.Username)
			status := failAcc1Status
			mu.Unlock()
			if body.Username == "upstream-1" && status != 0 {
				w.Header().Set("X-Failed-Account", "upstream-1")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"rate limited"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"AccessToken":"tok-%s-%d","User":{"Name":%q}}`, body.Username, time.Now().UnixNano(), body.Username)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "lb.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x41}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	pm.SetTrustedProxies([]*net.IPNet{loopbackCIDR})
	app := &App{db: db, dbPath: dbPath, pm: pm, trustedProxies: []*net.IPNet{loopbackCIDR}}

	site, err := db.CreateSiteRecord(Site{
		Name:        "LBSite",
		TargetURL:   upstream.URL,
		ListenPort:  18201,
		IngressMode: ingressModePath,
		PathPrefix:  "/lbsite",
		Enabled:     true,
		UAMode:      "passthrough",
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if _, err := db.saveSiteAccount(site.ID, 0, SiteAccountInput{AccountName: "Acc1", Username: "upstream-1", Password: "pw1"}); err != nil {
		t.Fatalf("create acc1: %v", err)
	}
	if _, err := db.saveSiteAccount(site.ID, 0, SiteAccountInput{AccountName: "Acc2", Username: "upstream-2", Password: "pw2"}); err != nil {
		t.Fatalf("create acc2: %v", err)
	}
	if err := pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	defer pm.GracefulShutdown(context.Background())

	// Create share user with site_account_id = 0 (auto-balance)
	createRec := httptest.NewRecorder()
	createBody := fmt.Sprintf(`{"username":"friend1","password":"pass1234","enabled":true,"sites":[{"site_id":%d,"site_account_id":0}]}`, site.ID)
	app.handleShareUsers(createRec, httptest.NewRequest(http.MethodPost, "/api/share-users", strings.NewReader(createBody)))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create share user status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	h, _ := pm.SiteHandler(site.ID)
	doLogin := func(devID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"friend1","Pw":"pass1234"}`))
		req.RemoteAddr = "127.0.0.1:43000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Authorization", fmt.Sprintf(`MediaBrowser Client="Infuse", Device="TV-%s", DeviceId="%s", Version="8.0"`, devID, devID))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. Two logins should spread across both upstream accounts (least-active load balancing)
	rec1 := doLogin("d1")
	if rec1.Code != http.StatusOK {
		t.Fatalf("login 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}
	rec2 := doLogin("d2")
	if rec2.Code != http.StatusOK {
		t.Fatalf("login 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	firstTwo := append([]string(nil), loginUsers...)
	mu.Unlock()
	if len(firstTwo) != 2 || firstTwo[0] == firstTwo[1] {
		t.Fatalf("expected logins to spread across upstream-1 and upstream-2, got %v", firstTwo)
	}

	// 2. Simulate upstream-1 returning 429 rate-limit; login MUST failover seamlessly to upstream-2 with HTTP 200
	mu.Lock()
	failAcc1Status = http.StatusTooManyRequests
	loginUsers = nil
	mu.Unlock()

	// Ensure upstream-1 is attempted first by pinning preferredAccount or checking failover on any 429
	rec3 := doLogin("d3")
	if rec3.Code != http.StatusOK {
		t.Fatalf("failover login should return 200, got %d body=%s", rec3.Code, rec3.Body.String())
	}
	if rec3.Header().Get("X-Failed-Account") != "" {
		t.Fatalf("failed attempt headers must not leak into successful failover response")
	}
	if !strings.Contains(rec3.Body.String(), "upstream-2") {
		t.Fatalf("expected failover login to succeed with upstream-2, got %s", rec3.Body.String())
	}
}

func TestClientFilterBlacklistAndWhitelist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "cf.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x42}, 32))
	app := &App{db: db, dbPath: dbPath, pm: pm}

	site, err := db.CreateSiteRecord(Site{
		Name:        "FilterSite",
		TargetURL:   upstream.URL,
		ListenPort:  18202,
		IngressMode: ingressModePath,
		PathPrefix:  "/filtersite",
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

	h, _ := pm.SiteHandler(site.ID)
	probe := func(ua, embyClient string) int {
		req := httptest.NewRequest(http.MethodGet, "/emby/System/Info/Public", nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if embyClient != "" {
			req.Header.Set("X-Emby-Client", embyClient)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// 1. Blacklist mode
	putRec := httptest.NewRecorder()
	app.handleClientFilter(putRec, httptest.NewRequest(http.MethodPut, "/api/client-filter", strings.NewReader(`{"mode":"blacklist","rules":["curl","yt-dlp"]}`)))
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT blacklist status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	if code := probe("curl/8.5.0", ""); code != http.StatusForbidden {
		t.Fatalf("expected 403 for curl, got %d", code)
	}
	if code := probe("Infuse/8.0.1", "Infuse"); code != http.StatusOK {
		t.Fatalf("expected 200 for Infuse, got %d", code)
	}

	// 2. Whitelist mode
	putRec2 := httptest.NewRecorder()
	app.handleClientFilter(putRec2, httptest.NewRequest(http.MethodPut, "/api/client-filter", strings.NewReader(`{"mode":"whitelist","rules":["infuse","senplayer"]}`)))
	if putRec2.Code != http.StatusOK {
		t.Fatalf("PUT whitelist status=%d body=%s", putRec2.Code, putRec2.Body.String())
	}
	if code := probe("Infuse/8.0.1", ""); code != http.StatusOK {
		t.Fatalf("expected 200 for whitelisted Infuse, got %d", code)
	}
	if code := probe("CustomUA/1.0", "SenPlayer"); code != http.StatusOK {
		t.Fatalf("expected 200 for X-Emby-Client SenPlayer, got %d", code)
	}
	if code := probe("aria2/1.37", ""); code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-whitelisted aria2, got %d", code)
	}
	if code := probe("", ""); code != http.StatusForbidden {
		t.Fatalf("expected 403 for empty client in whitelist mode, got %d", code)
	}
}

func TestRedeemCodesRegisterAndRenewFlow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "redeem.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x43}, 32))
	_, loopbackCIDR, _ := net.ParseCIDR("127.0.0.1/32")
	app := &App{db: db, dbPath: dbPath, pm: pm, loginLimiter: newLoginRateLimiter(), trustedProxies: []*net.IPNet{loopbackCIDR}}

	site, err := db.CreateSiteRecord(Site{
		Name:        "RedeemSite",
		TargetURL:   "https://emby.example.com",
		ListenPort:  18203,
		IngressMode: ingressModePath,
		PathPrefix:  "/redeemsite",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}

	// 1. Admin creates 1 register code (30 days, 100GB) and 2 renew codes (60 days, 200GB)
	regBody := fmt.Sprintf(`{"kind":"register","count":1,"days":30,"traffic_quota":107374182400,"site_ids":[%d],"note":"VIP"}`, site.ID)
	regCreateRec := httptest.NewRecorder()
	app.handleRedeemCodes(regCreateRec, httptest.NewRequest(http.MethodPost, "/api/redeem-codes", strings.NewReader(regBody)))
	if regCreateRec.Code != http.StatusCreated {
		t.Fatalf("create register code status=%d body=%s", regCreateRec.Code, regCreateRec.Body.String())
	}
	var regCodes []ShareRedeemCode
	if err := json.Unmarshal(regCreateRec.Body.Bytes(), &regCodes); err != nil || len(regCodes) != 1 {
		t.Fatalf("decode register codes: %v %s", err, regCreateRec.Body.String())
	}

	renCreateRec := httptest.NewRecorder()
	app.handleRedeemCodes(renCreateRec, httptest.NewRequest(http.MethodPost, "/api/redeem-codes", strings.NewReader(`{"kind":"renew","count":2,"days":60,"traffic_quota":214748364800}`)))
	if renCreateRec.Code != http.StatusCreated {
		t.Fatalf("create renew codes status=%d body=%s", renCreateRec.Code, renCreateRec.Body.String())
	}
	var renCodes []ShareRedeemCode
	if err := json.Unmarshal(renCreateRec.Body.Bytes(), &renCodes); err != nil || len(renCodes) != 2 {
		t.Fatalf("decode renew codes: %v", err)
	}

	// 2. Public self-registration with register code
	regReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/redeem/register", strings.NewReader(fmt.Sprintf(`{"code":%q,"username":"newfriend","password":"mypassword"}`, regCodes[0].Code)))
	regReq.Header.Set("Origin", "http://example.com")
	regRec := httptest.NewRecorder()
	app.handleRedeemRegister(regRec, regReq)
	if regRec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", regRec.Code, regRec.Body.String())
	}
	var regResp struct {
		Username    string `json:"username"`
		RoutePrefix string `json:"route_prefix"`
	}
	_ = json.Unmarshal(regRec.Body.Bytes(), &regResp)
	if regResp.Username != "newfriend" || !strings.HasPrefix(regResp.RoutePrefix, "/") {
		t.Fatalf("unexpected register response: %+v", regResp)
	}
	var shareCookie *http.Cookie
	for _, c := range regRec.Result().Cookies() {
		if c.Name == shareSessionCookie {
			shareCookie = c
		}
	}
	if shareCookie == nil {
		t.Fatalf("expected share session cookie on self-registration")
	}

	// Reusing the same register code must fail
	dupReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/redeem/register", strings.NewReader(fmt.Sprintf(`{"code":%q,"username":"another","password":"mypassword"}`, regCodes[0].Code)))
	dupReq.Header.Set("Origin", "http://example.com")
	dupRec := httptest.NewRecorder()
	app.handleRedeemRegister(dupRec, dupReq)
	if dupRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when reusing register code, got %d", dupRec.Code)
	}

	// 3. Friend redeems renew code #1 via POST /api/me/redeem
	renewReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/me/redeem", strings.NewReader(fmt.Sprintf(`{"code":%q}`, renCodes[0].Code)))
	renewReq.Header.Set("Origin", "http://example.com")
	renewReq.AddCookie(shareCookie)
	renewRec := httptest.NewRecorder()
	app.handleShareSelf(renewRec, renewReq)
	if renewRec.Code != http.StatusOK {
		t.Fatalf("self redeem status=%d body=%s", renewRec.Code, renewRec.Body.String())
	}
	var renewResp struct {
		TrafficQuota  int64  `json:"traffic_quota"`
		RedeemSummary string `json:"redeem_summary"`
	}
	_ = json.Unmarshal(renewRec.Body.Bytes(), &renewResp)
	if renewResp.TrafficQuota != 214748364800 || !strings.Contains(renewResp.RedeemSummary, "续期 60 天") {
		t.Fatalf("unexpected renew response: %+v", renewResp)
	}

	// 4. Admin revokes renew code #2 -> redeeming it must fail
	revRec := httptest.NewRecorder()
	app.handleRedeemCodeByID(revRec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/redeem-codes/%d/revoke", renCodes[1].ID), nil))
	if revRec.Code != http.StatusOK {
		t.Fatalf("revoke code status=%d body=%s", revRec.Code, revRec.Body.String())
	}
	tryRevokedReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/me/redeem", strings.NewReader(fmt.Sprintf(`{"code":%q}`, renCodes[1].Code)))
	tryRevokedReq.Header.Set("Origin", "http://example.com")
	tryRevokedReq.AddCookie(shareCookie)
	tryRevokedRec := httptest.NewRecorder()
	app.handleShareSelf(tryRevokedRec, tryRevokedReq)
	if tryRevokedRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for revoked code, got %d", tryRevokedRec.Code)
	}
}

func TestShareAlertMonitorExpiryAndQuota(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "alerts.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	pm := NewProxyManager(db, bytes.Repeat([]byte{0x44}, 32))
	app := &App{db: db, dbPath: dbPath, pm: pm}

	site, err := db.CreateSiteRecord(Site{
		Name:        "AlertSite",
		TargetURL:   "https://emby.example.com",
		ListenPort:  18204,
		IngressMode: ingressModePath,
		PathPrefix:  "/alertsite",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}

	now := time.Now()
	soonDate := now.AddDate(0, 0, 2).Format("2006-01-02")
	u1Body := fmt.Sprintf(`{"username":"soonuser","password":"pass1234","enabled":true,"expires_at":%q,"traffic_quota":1000,"sites":[{"site_id":%d}]}`, soonDate, site.ID)
	rec1 := httptest.NewRecorder()
	app.handleShareUsers(rec1, httptest.NewRequest(http.MethodPost, "/api/share-users", strings.NewReader(u1Body)))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("create soonuser: %d %s", rec1.Code, rec1.Body.String())
	}
	var u1 struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec1.Body.Bytes(), &u1)

	// Set traffic_used to 85% (850 / 1000)
	if _, err := db.db.Exec(`UPDATE share_users SET traffic_used=850 WHERE id=?`, u1.ID); err != nil {
		t.Fatalf("update traffic_used: %v", err)
	}
	_ = pm.shares.reload()

	alerts := collectPendingShareAlerts(db, pm.shares, now)
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts (expiring + 80%% quota), got %d: %+v", len(alerts), alerts)
	}

	// Mark sent -> deduplicated (0 alerts)
	for _, a := range alerts {
		_, _ = db.db.Exec(`INSERT OR IGNORE INTO share_alert_sent(alert_key) VALUES(?)`, a.key)
	}
	if dup := collectPendingShareAlerts(db, pm.shares, now); len(dup) != 0 {
		t.Fatalf("expected 0 deduplicated alerts, got %+v", dup)
	}

	// Cross 100% quota -> 1 quota_full alert
	if _, err := db.db.Exec(`UPDATE share_users SET traffic_used=1000 WHERE id=?`, u1.ID); err != nil {
		t.Fatalf("update traffic_used 100%%: %v", err)
	}
	_ = pm.shares.reload()
	fullAlerts := collectPendingShareAlerts(db, pm.shares, now)
	if len(fullAlerts) != 1 || !strings.Contains(fullAlerts[0].message, "流量已耗尽") {
		t.Fatalf("expected 1 quota_full alert, got %+v", fullAlerts)
	}
}
