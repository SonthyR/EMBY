package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/net/publicsuffix"
)

const (
	defaultEdgeSubdomainPrefix   = "node"
	edgeAutoHTTPSRenewInterval   = 6 * time.Hour
	edgeAutoHTTPSDNSRetryBackoff = 60 * time.Second
)

const edgeAutoHTTPSSchemaSQL = `
CREATE TABLE IF NOT EXISTS edge_tls_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	enabled INTEGER NOT NULL DEFAULT 0,
	base_domain TEXT NOT NULL DEFAULT '',
	subdomain_prefix TEXT NOT NULL DEFAULT 'node',
	acme_email TEXT NOT NULL DEFAULT '',
	cf_token_ciphertext TEXT NOT NULL DEFAULT '',
	zone_id TEXT NOT NULL DEFAULT '',
	last_cert_error TEXT NOT NULL DEFAULT '',
	last_renewed_at TEXT NOT NULL DEFAULT '',
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO edge_tls_settings (id) VALUES (1);
`

func ensureEdgeAutoHTTPSSchema(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, edgeAutoHTTPSSchemaSQL)
	return err
}

type edgeAutoHTTPSSettings struct {
	Enabled           bool
	BaseDomain        string
	SubdomainPrefix   string
	ACMEEmail         string
	CFTokenCiphertext string
	ZoneID            string
	LastCertError     string
	LastRenewedAt     string
}

type EdgeAutoHTTPSStatus struct {
	Enabled           bool     `json:"enabled"`
	BaseDomain        string   `json:"base_domain"`
	WildcardDomain    string   `json:"wildcard_domain"`
	SubdomainPrefix   string   `json:"subdomain_prefix"`
	ACMEEmail         string   `json:"acme_email"`
	TokenConfigured   bool     `json:"token_configured"`
	ZoneID            string   `json:"zone_id,omitempty"`
	CertConfigured    bool     `json:"cert_configured"`
	CertValid         bool     `json:"cert_valid"`
	CertMatchesDomain bool     `json:"cert_matches_domain"`
	CertExpiresAt     string   `json:"cert_expires_at,omitempty"`
	CertDaysRemaining int      `json:"cert_days_remaining,omitempty"`
	CertDNSNames      []string `json:"cert_dns_names,omitempty"`
	Issuing           bool     `json:"issuing"`
	LastCertError     string   `json:"last_cert_error,omitempty"`
	LastRenewedAt     string   `json:"last_renewed_at,omitempty"`
	AutoNodesCount    int      `json:"auto_nodes_count"`
	SyncedNodesCount  int      `json:"synced_nodes_count"`
}

type edgeAutoHTTPSManager struct {
	db         *DB
	edges      *edgeNodeRuntime
	certFile   string
	keyFile    string
	accountDir string
	httpClient *http.Client
	cfAPIBase  string

	mu            sync.Mutex
	issuing       bool
	syncInFlight  map[int64]bool
	lastSyncError map[int64]time.Time

	// Injectable certificate issuer for deterministic offline tests.
	issuer func(ctx context.Context, email, token, baseDomain string) (certPEM, keyPEM []byte, err error)
}

func edgeAutoHTTPSPaths(dbPath string) (certFile, keyFile, accountDir string) {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return "", "", ""
	}
	base := filepath.Join(filepath.Dir(dbPath), "edge-tls")
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "privkey.pem"), base
}

func newEdgeAutoHTTPSManager(db *DB, dbPath string, edges *edgeNodeRuntime) *edgeAutoHTTPSManager {
	certFile, keyFile, accountDir := edgeAutoHTTPSPaths(dbPath)
	return &edgeAutoHTTPSManager{
		db:            db,
		edges:         edges,
		certFile:      certFile,
		keyFile:       keyFile,
		accountDir:    accountDir,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		cfAPIBase:     "https://api.cloudflare.com/client/v4",
		syncInFlight:  make(map[int64]bool),
		lastSyncError: make(map[int64]time.Time),
	}
}

func normalizeEdgeAutoHTTPSDomain(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimPrefix(v, "http://")
	if slash := strings.IndexByte(v, '/'); slash >= 0 {
		v = v[:slash]
	}
	v = strings.TrimPrefix(v, "*.")
	v = strings.Trim(v, ".")
	if v == "" {
		return "", errors.New("请填写从机泛域名（例如 *.example.com 或 example.com）")
	}
	host, err := normalizePublicHost(v)
	if err != nil {
		return "", errors.New("泛域名格式无效，请填写有效域名（例如 *.example.com）")
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(host); err != nil {
		return "", errors.New("泛域名必须包含有效顶级域名（例如 *.example.com）")
	}
	return host, nil
}

func normalizeEdgeSubdomainPrefix(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return defaultEdgeSubdomainPrefix, nil
	}
	if len(v) > 20 {
		return "", errors.New("子域名前缀不能超过 20 个字符")
	}
	if v[0] == '-' || v[len(v)-1] == '-' {
		return "", errors.New("子域名前缀不能以连字符开头或结尾")
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return "", errors.New("子域名前缀仅支持小写字母、数字和连字符")
		}
	}
	return v, nil
}

func isSingleLevelSubdomainOf(host, baseDomain string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if host == "" || baseDomain == "" {
		return false
	}
	suffix := "." + baseDomain
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := strings.TrimSuffix(host, suffix)
	return label != "" && !strings.Contains(label, ".")
}

func autoEdgeHostname(prefix string, nodeID int64, baseDomain string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = defaultEdgeSubdomainPrefix
	}
	return fmt.Sprintf("%s-%d.%s", p, nodeID, baseDomain)
}

func (m *edgeAutoHTTPSManager) loadSettings() (edgeAutoHTTPSSettings, error) {
	var s edgeAutoHTTPSSettings
	s.SubdomainPrefix = defaultEdgeSubdomainPrefix
	if m == nil || m.db == nil {
		return s, nil
	}
	var enabledInt int
	err := m.db.db.QueryRow(`SELECT enabled, base_domain, subdomain_prefix, acme_email, cf_token_ciphertext, zone_id, last_cert_error, last_renewed_at FROM edge_tls_settings WHERE id=1`).
		Scan(&enabledInt, &s.BaseDomain, &s.SubdomainPrefix, &s.ACMEEmail, &s.CFTokenCiphertext, &s.ZoneID, &s.LastCertError, &s.LastRenewedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s, nil
		}
		return s, err
	}
	s.Enabled = enabledInt != 0
	if strings.TrimSpace(s.SubdomainPrefix) == "" {
		s.SubdomainPrefix = defaultEdgeSubdomainPrefix
	}
	return s, nil
}

func (m *edgeAutoHTTPSManager) saveSettings(s edgeAutoHTTPSSettings) error {
	if m == nil || m.db == nil {
		return errors.New("数据库未初始化")
	}
	_, err := m.db.db.Exec(
		`INSERT INTO edge_tls_settings(id, enabled, base_domain, subdomain_prefix, acme_email, cf_token_ciphertext, zone_id, last_cert_error, last_renewed_at, updated_at)
		 VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   enabled=excluded.enabled,
		   base_domain=excluded.base_domain,
		   subdomain_prefix=excluded.subdomain_prefix,
		   acme_email=excluded.acme_email,
		   cf_token_ciphertext=excluded.cf_token_ciphertext,
		   zone_id=excluded.zone_id,
		   last_cert_error=excluded.last_cert_error,
		   last_renewed_at=excluded.last_renewed_at,
		   updated_at=CURRENT_TIMESTAMP`,
		sqliteBool(s.Enabled), s.BaseDomain, s.SubdomainPrefix, s.ACMEEmail, s.CFTokenCiphertext, s.ZoneID, s.LastCertError, s.LastRenewedAt,
	)
	return err
}

func (m *edgeAutoHTTPSManager) newCloudflareClient(token string) *cloudflareClient {
	apiBase := "https://api.cloudflare.com/client/v4"
	if m != nil && strings.TrimSpace(m.cfAPIBase) != "" {
		apiBase = m.cfAPIBase
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if m != nil && m.httpClient != nil {
		httpClient = m.httpClient
	}
	return &cloudflareClient{
		token:      strings.TrimSpace(token),
		httpClient: httpClient,
		apiBase:    apiBase,
	}
}

func (m *edgeAutoHTTPSManager) certPEMForHost(hostname string) (certPEM, keyPEM, fingerprint string, ok bool) {
	hostname = strings.TrimSpace(hostname)
	if m == nil || hostname == "" || m.certFile == "" || m.keyFile == "" {
		return "", "", "", false
	}
	// #nosec G304 -- certFile and keyFile are generated under the private database edge-tls directory.
	certBytes, err := os.ReadFile(m.certFile)
	if err != nil {
		return "", "", "", false
	}
	// #nosec G304 -- certFile and keyFile are generated under the private database edge-tls directory.
	keyBytes, err := os.ReadFile(m.keyFile)
	if err != nil {
		return "", "", "", false
	}
	pair, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil || len(pair.Certificate) == 0 {
		return "", "", "", false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", "", "", false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return "", "", "", false
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return "", "", "", false
	}
	sum := sha256.Sum256(pair.Certificate[0])
	return string(certBytes), string(keyBytes), hex.EncodeToString(sum[:]), true
}

func (m *edgeAutoHTTPSManager) inspectCert(baseDomain string) (configured, valid, matches bool, expiresAt string, daysRemaining int, dnsNames []string) {
	if m == nil || m.certFile == "" || m.keyFile == "" {
		return false, false, false, "", 0, nil
	}
	pair, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil || len(pair.Certificate) == 0 {
		return false, false, false, "", 0, nil
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false, false, false, "", 0, nil
	}
	configured = true
	now := time.Now()
	valid = !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter)
	expiresAt = leaf.NotAfter.UTC().Format(time.RFC3339)
	days := int(time.Until(leaf.NotAfter).Hours() / 24)
	if days < 0 {
		days = 0
	}
	daysRemaining = days
	dnsNames = append([]string(nil), leaf.DNSNames...)
	if baseDomain != "" {
		matches = leaf.VerifyHostname("probe."+baseDomain) == nil
	}
	return configured, valid, matches, expiresAt, daysRemaining, dnsNames
}

func (m *edgeAutoHTTPSManager) status() EdgeAutoHTTPSStatus {
	s, _ := m.loadSettings()
	st := EdgeAutoHTTPSStatus{
		Enabled:         s.Enabled,
		BaseDomain:      s.BaseDomain,
		SubdomainPrefix: s.SubdomainPrefix,
		ACMEEmail:       s.ACMEEmail,
		TokenConfigured: s.CFTokenCiphertext != "",
		ZoneID:          s.ZoneID,
		LastCertError:   s.LastCertError,
		LastRenewedAt:   s.LastRenewedAt,
	}
	if s.BaseDomain != "" {
		st.WildcardDomain = "*." + s.BaseDomain
	}
	if m != nil {
		m.mu.Lock()
		st.Issuing = m.issuing
		m.mu.Unlock()
		st.CertConfigured, st.CertValid, st.CertMatchesDomain, st.CertExpiresAt, st.CertDaysRemaining, st.CertDNSNames = m.inspectCert(s.BaseDomain)
	}
	if m != nil && m.edges != nil {
		m.edges.mu.RLock()
		for _, n := range m.edges.nodes {
			if n != nil && n.autoHTTPS {
				st.AutoNodesCount++
				if n.cfDNSStatus == "synced" {
					st.SyncedNodesCount++
				}
			}
		}
		m.edges.mu.RUnlock()
	}
	return st
}

func (m *edgeAutoHTTPSManager) issueWildcardCert(ctx context.Context, force bool) (EdgeAutoHTTPSStatus, error) {
	if m == nil || m.db == nil {
		return EdgeAutoHTTPSStatus{}, errors.New("从机自动 HTTPS 服务未初始化")
	}
	if m.certFile == "" || m.keyFile == "" || m.accountDir == "" {
		return m.status(), errors.New("可写的 TLS 数据目录不可用")
	}
	s, err := m.loadSettings()
	if err != nil {
		return m.status(), err
	}
	if s.BaseDomain == "" {
		return m.status(), errors.New("请先填写并保存从机泛域名")
	}
	if s.CFTokenCiphertext == "" {
		return m.status(), errors.New("请先配置 Cloudflare DNS API Token")
	}
	token, err := decryptPanelACMEToken(s.CFTokenCiphertext)
	if err != nil {
		return m.status(), errors.New("已保存的 Cloudflare API Token 无法解密，请重新填写后保存")
	}
	if err := validatePanelCertificateRequest(s.ACMEEmail, token); err != nil {
		return m.status(), err
	}

	if !force {
		configured, valid, matches, _, days, _ := m.inspectCert(s.BaseDomain)
		if configured && valid && matches && days > int(panelCertificateRenewalWindow.Hours()/24) {
			return m.status(), nil
		}
	}

	m.mu.Lock()
	if m.issuing {
		m.mu.Unlock()
		return m.status(), errCertificateIssuanceBusy
	}
	m.issuing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.issuing = false
		m.mu.Unlock()
	}()

	var certPEM, keyPEM []byte
	if m.issuer != nil {
		certPEM, keyPEM, err = m.issuer(ctx, s.ACMEEmail, token, s.BaseDomain)
	} else {
		certPEM, keyPEM, err = m.issueWildcardCertViaACME(ctx, s.ACMEEmail, token, s.BaseDomain)
	}
	if err != nil {
		safeErr := errors.New(strings.ReplaceAll(err.Error(), token, "[已隐藏]"))
		s.LastCertError = safeErr.Error()
		_ = m.saveSettings(s)
		return m.status(), safeErr
	}
	if _, pairErr := tls.X509KeyPair(certPEM, keyPEM); pairErr != nil {
		s.LastCertError = "签发的证书格式无效: " + pairErr.Error()
		_ = m.saveSettings(s)
		return m.status(), errors.New(s.LastCertError)
	}
	if err := writePrivateFileAtomic(m.keyFile, keyPEM); err != nil {
		return m.status(), fmt.Errorf("写入证书私钥失败: %w", err)
	}
	if err := writePrivateFileAtomic(m.certFile, certPEM); err != nil {
		return m.status(), fmt.Errorf("写入证书链失败: %w", err)
	}

	s.Enabled = true
	s.LastCertError = ""
	s.LastRenewedAt = time.Now().UTC().Format(time.RFC3339)
	if err := m.saveSettings(s); err != nil {
		return m.status(), err
	}
	log.Printf("[edge-auto-https] issued wildcard certificate for *.%s", s.BaseDomain)
	return m.status(), nil
}

func (m *edgeAutoHTTPSManager) issueWildcardCertViaACME(ctx context.Context, email, token, baseDomain string) ([]byte, []byte, error) {
	if err := os.MkdirAll(m.accountDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create edge-tls directory: %w", err)
	}
	accountKey, err := loadOrCreateACMEAccountKey(m.accountDir, "acme-account.pem")
	if err != nil {
		return nil, nil, err
	}
	client := &acme.Client{
		Key:          accountKey,
		DirectoryURL: letsEncryptProductionDirectory,
		UserAgent:    "Yundu/" + appVersion,
	}
	_, err = client.Register(ctx, &acme.Account{Contact: []string{"mailto:" + strings.TrimSpace(email)}}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, fmt.Errorf("register ACME account: %w", err)
	}

	domains := []string{"*." + baseDomain}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domains...))
	if err != nil {
		return nil, nil, fmt.Errorf("create ACME order: %w", err)
	}
	cf := m.newCloudflareClient(token)
	for _, authorizationURL := range order.AuthzURLs {
		if err := fulfillCloudflareDNSAuthorization(ctx, client, cf, authorizationURL, baseDomain); err != nil {
			return nil, nil, err
		}
	}
	readyOrder, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, nil, fmt.Errorf("wait for ACME order: %w", err)
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate certificate key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: domains}, certKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate request: %w", err)
	}
	chain, _, err := client.CreateOrderCert(ctx, readyOrder.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, fmt.Errorf("issue certificate: %w", err)
	}
	certPEM := make([]byte, 0, len(chain)*1024)
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(certKey)
	if err != nil {
		return nil, nil, fmt.Errorf("encode certificate key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// ---------------------------------------------------------------------------
// Cloudflare A / AAAA DNS record management (always DNS-only, proxied=false)

func (c *cloudflareClient) findAddressRecord(ctx context.Context, zoneID, name, recordType string) (string, error) {
	q := "/zones/" + url.PathEscape(zoneID) + "/dns_records?name=" + url.QueryEscape(name) + "&type=" + url.QueryEscape(recordType) + "&per_page=1"
	result, err := c.request(ctx, http.MethodGet, q, nil)
	if err != nil {
		return "", err
	}
	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &records); err != nil || len(records) == 0 {
		return "", nil
	}
	return records[0].ID, nil
}

func (c *cloudflareClient) upsertAddressRecord(ctx context.Context, zoneID, existingRecordID, existingRecordType, name, ip string) (string, string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "", "", errors.New("节点 IP 地址格式无效")
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
		return "", "", errors.New("节点 IP 不能是本地回环或无效地址")
	}
	recordType := "A"
	if addr.Is6() {
		recordType = "AAAA"
	}
	ipStr := addr.String()

	payload, err := json.Marshal(map[string]any{
		"type":    recordType,
		"name":    name,
		"content": ipStr,
		"ttl":     60,
		"proxied": false,
	})
	if err != nil {
		return "", "", err
	}

	if existingRecordID != "" && existingRecordType != "" && existingRecordType != recordType {
		_ = c.deleteRecord(ctx, zoneID, existingRecordID)
		existingRecordID = ""
	}

	if existingRecordID != "" {
		updatePath := "/zones/" + url.PathEscape(zoneID) + "/dns_records/" + url.PathEscape(existingRecordID)
		if _, err := c.request(ctx, http.MethodPut, updatePath, strings.NewReader(string(payload))); err == nil {
			return existingRecordID, recordType, nil
		}
	}

	foundID, err := c.findAddressRecord(ctx, zoneID, name, recordType)
	if err == nil && foundID != "" {
		updatePath := "/zones/" + url.PathEscape(zoneID) + "/dns_records/" + url.PathEscape(foundID)
		if _, putErr := c.request(ctx, http.MethodPut, updatePath, strings.NewReader(string(payload))); putErr == nil {
			return foundID, recordType, nil
		}
	}

	createPath := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	result, err := c.request(ctx, http.MethodPost, createPath, strings.NewReader(string(payload)))
	if err != nil {
		return "", "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &created); err != nil || created.ID == "" {
		return "", "", errors.New("Cloudflare 未返回 DNS 记录 ID")
	}
	return created.ID, recordType, nil
}

func (m *edgeAutoHTTPSManager) resolveZoneID(ctx context.Context, cf *cloudflareClient, s *edgeAutoHTTPSSettings) (string, error) {
	if s.ZoneID != "" {
		return s.ZoneID, nil
	}
	zoneName, err := publicsuffix.EffectiveTLDPlusOne(s.BaseDomain)
	if err != nil {
		return "", fmt.Errorf("解析根域名失败: %w", err)
	}
	zoneID, err := cf.findZone(ctx, zoneName)
	if err != nil {
		return "", err
	}
	s.ZoneID = zoneID
	_ = m.saveSettings(*s)
	return zoneID, nil
}

func (m *edgeAutoHTTPSManager) syncNodeDNS(ctx context.Context, nodeID int64) error {
	if m == nil || m.db == nil || m.edges == nil || nodeID <= 0 {
		return nil
	}
	s, err := m.loadSettings()
	if err != nil || s.BaseDomain == "" || s.CFTokenCiphertext == "" {
		return errors.New("从机自动 HTTPS 未配置完成")
	}
	token, err := decryptPanelACMEToken(s.CFTokenCiphertext)
	if err != nil {
		return errors.New("Cloudflare API Token 无法解密")
	}

	var host, publicIP, detectedIP, recID, recType, recIP string
	var autoHTTPS bool
	m.edges.mu.RLock()
	if n := m.edges.nodes[nodeID]; n != nil {
		autoHTTPS = n.autoHTTPS
		host = n.publicHost
		publicIP = n.publicIP
		detectedIP = n.detectedIP
		recID = n.cfRecordID
		recType = n.cfRecordType
		recIP = n.cfRecordIP
	}
	m.edges.mu.RUnlock()

	if !autoHTTPS || host == "" {
		return nil
	}
	targetIP := strings.TrimSpace(publicIP)
	if targetIP == "" {
		targetIP = strings.TrimSpace(detectedIP)
	}
	if targetIP == "" {
		m.updateNodeDNSState(nodeID, recID, recType, recIP, "pending_ip")
		return nil
	}
	addr, parseErr := netip.ParseAddr(targetIP)
	if parseErr != nil || addr.Unmap().IsLoopback() || addr.Unmap().IsUnspecified() {
		m.updateNodeDNSState(nodeID, recID, recType, recIP, "pending_ip")
		return nil
	}
	targetIP = addr.Unmap().String()

	cf := m.newCloudflareClient(token)
	zoneID, err := m.resolveZoneID(ctx, cf, &s)
	if err != nil {
		safeErr := strings.ReplaceAll(err.Error(), token, "[已隐藏]")
		m.updateNodeDNSState(nodeID, recID, recType, recIP, "error:"+safeErr)
		return errors.New(safeErr)
	}

	newID, newType, err := cf.upsertAddressRecord(ctx, zoneID, recID, recType, host, targetIP)
	if err != nil {
		safeErr := strings.ReplaceAll(err.Error(), token, "[已隐藏]")
		m.updateNodeDNSState(nodeID, recID, recType, recIP, "error:"+safeErr)
		return errors.New(safeErr)
	}

	m.updateNodeDNSState(nodeID, newID, newType, targetIP, "synced")
	log.Printf("[edge-auto-https] synced Cloudflare DNS %s %s -> %s (node #%d)", newType, host, targetIP, nodeID)
	return nil
}

func (m *edgeAutoHTTPSManager) updateNodeDNSState(nodeID int64, recID, recType, recIP, status string) {
	if len(status) > 240 {
		status = status[:240]
	}
	if m.db != nil {
		_, _ = m.db.db.Exec(
			`UPDATE edge_nodes SET cf_record_id=?, cf_record_type=?, cf_record_ip=?, cf_dns_status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			recID, recType, recIP, status, nodeID,
		)
	}
	if m.edges != nil {
		m.edges.mu.Lock()
		if n := m.edges.nodes[nodeID]; n != nil {
			n.cfRecordID = recID
			n.cfRecordType = recType
			n.cfRecordIP = recIP
			n.cfDNSStatus = status
		}
		m.edges.mu.Unlock()
	}
}

func (m *edgeAutoHTTPSManager) deleteRecordAsync(recordID string) {
	recordID = strings.TrimSpace(recordID)
	if m == nil || recordID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s, err := m.loadSettings()
		if err != nil || s.CFTokenCiphertext == "" || s.BaseDomain == "" {
			return
		}
		token, err := decryptPanelACMEToken(s.CFTokenCiphertext)
		if err != nil {
			return
		}
		cf := m.newCloudflareClient(token)
		zoneID, err := m.resolveZoneID(ctx, cf, &s)
		if err != nil {
			return
		}
		if err := cf.deleteRecord(ctx, zoneID, recordID); err != nil {
			log.Printf("[edge-auto-https] delete Cloudflare DNS record %s failed: %v", recordID, strings.ReplaceAll(err.Error(), token, "[已隐藏]"))
		} else {
			log.Printf("[edge-auto-https] deleted Cloudflare DNS record %s", recordID)
		}
	}()
}

func (m *edgeAutoHTTPSManager) maybeSyncNodeDNSAsync(nodeID int64, force bool) {
	if m == nil || nodeID <= 0 {
		return
	}
	m.mu.Lock()
	if m.syncInFlight[nodeID] {
		m.mu.Unlock()
		return
	}
	if !force {
		if lastErr, ok := m.lastSyncError[nodeID]; ok && time.Since(lastErr) < edgeAutoHTTPSDNSRetryBackoff {
			m.mu.Unlock()
			return
		}
	}
	m.syncInFlight[nodeID] = true
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.syncInFlight, nodeID)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := m.syncNodeDNS(ctx, nodeID); err != nil {
			m.mu.Lock()
			m.lastSyncError[nodeID] = time.Now()
			m.mu.Unlock()
			log.Printf("[edge-auto-https] node #%d DNS sync error: %v", nodeID, err)
		} else {
			m.mu.Lock()
			delete(m.lastSyncError, nodeID)
			m.mu.Unlock()
		}
	}()
}

func (m *edgeAutoHTTPSManager) syncAllAutoNodes(ctx context.Context) int {
	if m == nil || m.edges == nil {
		return 0
	}
	var ids []int64
	m.edges.mu.RLock()
	for id, n := range m.edges.nodes {
		if n != nil && n.autoHTTPS {
			ids = append(ids, id)
		}
	}
	m.edges.mu.RUnlock()
	synced := 0
	for _, id := range ids {
		if err := m.syncNodeDNS(ctx, id); err == nil {
			synced++
		}
	}
	return synced
}

// runEdgeAutoHTTPSMonitor periodically checks wildcard certificate expiration
// and renews it automatically when <= 30 days remain.
func runEdgeAutoHTTPSMonitor(ctx context.Context, m *edgeAutoHTTPSManager) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(edgeAutoHTTPSRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s, err := m.loadSettings()
			if err != nil || !s.Enabled || s.BaseDomain == "" || s.CFTokenCiphertext == "" || jwtSecretEphemeral {
				continue
			}
			configured, valid, matches, _, days, _ := m.inspectCert(s.BaseDomain)
			if !configured || !valid || !matches || days <= int(panelCertificateRenewalWindow.Hours()/24) {
				renewCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
				if _, err := m.issueWildcardCert(renewCtx, true); err != nil {
					log.Printf("[edge-auto-https] auto-renewal failed: %v", err)
				}
				cancel()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP Handlers: /api/edge-auto-https, /api/edge-auto-https/issue, /api/edge-auto-https/sync

func (a *App) handleEdgeAutoHTTPS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.edgeAutoHTTPS == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "从机自动 HTTPS 服务未初始化")
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.jsonOK(w, a.edgeAutoHTTPS.status())
	case http.MethodPut:
		var req struct {
			Enabled         *bool  `json:"enabled"`
			BaseDomain      string `json:"base_domain"`
			SubdomainPrefix string `json:"subdomain_prefix"`
			ACMEEmail       string `json:"acme_email"`
			CFAPIToken      string `json:"cf_api_token"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
			return
		}
		cur, err := a.edgeAutoHTTPS.loadSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取从机自动 HTTPS 配置失败")
			return
		}
		next := cur
		if req.Enabled != nil {
			next.Enabled = *req.Enabled
		}
		if strings.TrimSpace(req.BaseDomain) != "" {
			domain, err := normalizeEdgeAutoHTTPSDomain(req.BaseDomain)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if domain != cur.BaseDomain {
				next.ZoneID = ""
			}
			next.BaseDomain = domain
		} else if next.Enabled && next.BaseDomain == "" {
			a.jsonErr(w, http.StatusBadRequest, "请填写从机泛域名（例如 *.example.com）")
			return
		}
		prefix, err := normalizeEdgeSubdomainPrefix(req.SubdomainPrefix)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		next.SubdomainPrefix = prefix
		if strings.TrimSpace(req.ACMEEmail) != "" {
			next.ACMEEmail = strings.TrimSpace(req.ACMEEmail)
		}

		rawToken := strings.TrimSpace(req.CFAPIToken)
		if rawToken != "" {
			if jwtSecretEphemeral {
				a.jsonErr(w, http.StatusConflict, "当前 JWT_SECRET 不是持久密钥，无法安全保存 Cloudflare API Token")
				return
			}
			cipher, err := encryptPanelACMEToken(rawToken)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			next.CFTokenCiphertext = cipher
			next.ZoneID = ""
		}

		if next.Enabled {
			if next.BaseDomain == "" {
				a.jsonErr(w, http.StatusBadRequest, "启用从机自动 HTTPS 前请填写泛域名")
				return
			}
			if next.CFTokenCiphertext == "" {
				a.jsonErr(w, http.StatusBadRequest, "启用从机自动 HTTPS 前请填写 Cloudflare DNS API Token")
				return
			}
			tokenToCheck := rawToken
			if tokenToCheck == "" {
				tokenToCheck, err = decryptPanelACMEToken(next.CFTokenCiphertext)
				if err != nil {
					a.jsonErr(w, http.StatusConflict, "已保存的 Cloudflare API Token 无法解密，请重新填写")
					return
				}
			}
			if err := validatePanelCertificateRequest(next.ACMEEmail, tokenToCheck); err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			zoneName, err := publicsuffix.EffectiveTLDPlusOne(next.BaseDomain)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, "泛域名格式无效")
				return
			}
			verifyCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			cf := a.edgeAutoHTTPS.newCloudflareClient(tokenToCheck)
			zoneID, zErr := cf.findZone(verifyCtx, zoneName)
			cancel()
			if zErr != nil {
				a.jsonErr(w, http.StatusBadRequest, strings.ReplaceAll(zErr.Error(), tokenToCheck, "[已隐藏]"))
				return
			}
			next.ZoneID = zoneID
		}

		if err := a.edgeAutoHTTPS.saveSettings(next); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "保存配置失败")
			return
		}
		a.jsonOK(w, a.edgeAutoHTTPS.status())
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleEdgeAutoHTTPSAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.edgeAutoHTTPS == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "从机自动 HTTPS 服务未初始化")
		return
	}
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/edge-auto-https/"), "/")
	switch action {
	case "issue":
		var req struct {
			Force bool `json:"force"`
		}
		if r.Body != nil {
			_ = decodeJSONBody(w, r, &req)
		}
		issueCtx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		st, err := a.edgeAutoHTTPS.issueWildcardCert(issueCtx, req.Force)
		if err != nil {
			if errors.Is(err, errCertificateIssuanceBusy) {
				a.jsonErr(w, http.StatusConflict, "证书正在申请中，请稍候")
				return
			}
			a.jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		// Immediately sync DNS for all auto-HTTPS nodes so they become ready right away.
		syncCtx, syncCancel := context.WithTimeout(r.Context(), 30*time.Second)
		a.edgeAutoHTTPS.syncAllAutoNodes(syncCtx)
		syncCancel()
		st = a.edgeAutoHTTPS.status()
		a.jsonOK(w, st)
	case "sync":
		syncCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		synced := a.edgeAutoHTTPS.syncAllAutoNodes(syncCtx)
		a.jsonOK(w, map[string]any{
			"synced": synced,
			"status": a.edgeAutoHTTPS.status(),
		})
	default:
		a.jsonErr(w, http.StatusNotFound, "unknown action")
	}
}
