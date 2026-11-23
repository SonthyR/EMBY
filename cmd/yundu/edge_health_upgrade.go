package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	edgeAgentReleaseBaseURL = "https://github.com/SonthyR/EMBY/releases/download"
	edgeAgentMaxBinaryBytes = 128 << 20
	edgeProbeInterval       = 60 * time.Second
	edgeAlertCheckInterval  = 15 * time.Second
)

var edgeReleaseVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$`)

type edgeSiteLatency struct {
	SiteName  string `json:"site_name"`
	LatencyMs int    `json:"latency_ms"` // -1 = unreachable
}

type agentProbeTarget struct {
	SiteID int64  `json:"site_id"`
	URL    string `json:"url"`
}

func validEdgeReleaseVersion(version string) bool {
	return edgeReleaseVersionPattern.MatchString(strings.TrimSpace(version))
}

func newEdgeUpgradeRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// currentProcessMemoryBytes returns resident memory bytes on Linux via
// /proc/self/statm when available, falling back to Go runtime Sys bytes.
func currentProcessMemoryBytes() uint64 {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/self/statm"); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) >= 2 {
				if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil && pages > 0 {
					if pageSize := os.Getpagesize(); pageSize > 0 {
						return pages * uint64(pageSize)
					}
				}
			}
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys
}

// ---------------------------------------------------------------------------
// Agent upstream latency prober

func (a *edgeAgent) setProbeTargets(targets []agentProbeTarget) {
	a.probeMu.Lock()
	changed := len(targets) != len(a.probeTargets)
	if !changed {
		for i := range targets {
			if targets[i] != a.probeTargets[i] {
				changed = true
				break
			}
		}
	}
	if changed {
		a.probeTargets = append([]agentProbeTarget(nil), targets...)
		if len(targets) == 0 {
			a.latencies = nil
		}
	}
	a.probeMu.Unlock()
	if changed && len(targets) > 0 {
		select {
		case a.probeNowCh <- struct{}{}:
		default:
		}
	}
}

func (a *edgeAgent) snapshotLatencies() map[int64]int {
	a.probeMu.RLock()
	defer a.probeMu.RUnlock()
	if len(a.latencies) == 0 {
		return nil
	}
	out := make(map[int64]int, len(a.latencies))
	for id, ms := range a.latencies {
		out[id] = ms
	}
	return out
}

func (a *edgeAgent) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(edgeProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.probeNowCh:
			a.runProbeOnce(ctx)
		case <-ticker.C:
			a.runProbeOnce(ctx)
		}
	}
}

func (a *edgeAgent) runProbeOnce(ctx context.Context) {
	a.probeMu.RLock()
	targets := append([]agentProbeTarget(nil), a.probeTargets...)
	a.probeMu.RUnlock()
	if len(targets) == 0 {
		return
	}
	results := make(map[int64]int, len(targets))
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		ms := probeURLTCPMs(ctx, dialer, target.URL)
		results[target.SiteID] = ms
	}
	a.probeMu.Lock()
	a.latencies = results
	a.probeMu.Unlock()
}

func probeURLTCPMs(ctx context.Context, dialer *net.Dialer, rawURL string) int {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return -1
	}
	host := parsed.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		if strings.EqualFold(parsed.Scheme, "https") {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return -1
	}
	_ = conn.Close()
	ms := int(time.Since(start).Milliseconds())
	if ms < 1 {
		ms = 1
	}
	return ms
}

// ---------------------------------------------------------------------------
// Agent remote self-upgrade (SHA-256 verified atomic binary swap)

func edgeAgentAssetName() (string, error) {
	goos := runtime.GOOS
	arch := runtime.GOARCH
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return "", fmt.Errorf("unsupported OS: %s", goos)
	}
	switch arch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported arch: %s", arch)
	}
	name := fmt.Sprintf("meridian-%s-%s", goos, arch)
	if goos == "windows" {
		name += ".exe"
	}
	return name, nil
}

func parseSHA256SumsForAsset(r io.Reader, asset string) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(r, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			sum := strings.ToLower(strings.TrimSpace(fields[0]))
			if len(sum) == 64 {
				return sum, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("SHA256SUMS missing entry for %s", asset)
}

func (a *edgeAgent) setUpgradeState(requestID, status string) {
	a.upgradeMu.Lock()
	a.upgradeReqID = requestID
	a.upgradeStatus = status
	a.upgradeMu.Unlock()
}

func (a *edgeAgent) getUpgradeState() (string, string) {
	a.upgradeMu.RLock()
	defer a.upgradeMu.RUnlock()
	return a.upgradeReqID, a.upgradeStatus
}

func (a *edgeAgent) maybeTriggerUpgrade(ctx context.Context, version, requestID string) {
	version = strings.TrimSpace(version)
	requestID = strings.TrimSpace(requestID)
	if version == "" || requestID == "" || version == appVersion {
		return
	}
	a.upgradeMu.Lock()
	if a.lastHandledUpgradeID == requestID {
		a.upgradeMu.Unlock()
		return
	}
	a.upgradeMu.Unlock()
	if !a.upgrading.CompareAndSwap(false, true) {
		return
	}
	a.upgradeMu.Lock()
	a.lastHandledUpgradeID = requestID
	a.upgradeReqID = requestID
	a.upgradeStatus = "updating"
	a.upgradeMu.Unlock()

	go func() {
		defer a.upgrading.Store(false)
		upgradeCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := a.performSelfUpgrade(upgradeCtx, version); err != nil {
			log.Printf("edge agent self-upgrade to %s failed: %v", version, err)
			a.setUpgradeState(requestID, "failed: "+err.Error())
			a.triggerFlush()
			return
		}
		log.Printf("edge agent upgraded binary to %s; restarting...", version)
		a.setUpgradeState(requestID, "restarting")
		a.triggerFlush()
		time.Sleep(500 * time.Millisecond)
		a.requestRestart()
	}()
}

func (a *edgeAgent) performSelfUpgrade(ctx context.Context, targetVersion string) error {
	if !validEdgeReleaseVersion(targetVersion) {
		return fmt.Errorf("invalid target version %q", targetVersion)
	}
	asset, err := edgeAgentAssetName()
	if err != nil {
		return err
	}
	exePath := strings.TrimSpace(a.cfg.ExecutablePath)
	if exePath == "" {
		exePath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate executable: %w", err)
		}
		if resolved, evalErr := filepath.EvalSymlinks(exePath); evalErr == nil {
			exePath = resolved
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(a.cfg.ReleaseBaseURL), "/")
	if baseURL == "" {
		baseURL = edgeAgentReleaseBaseURL
	}
	client := a.cfg.ReleaseClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}

	checksumURL := fmt.Sprintf("%s/%s/SHA256SUMS", baseURL, targetVersion)
	// #nosec G704 -- URL is constructed from a fixed GitHub Release base plus strict semver tag.
	sumReq, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	sumReq.Header.Set("User-Agent", "Yundu-Agent/"+appVersion)
	// #nosec G704 -- fixed GitHub Release checksum URL.
	sumResp, err := client.Do(sumReq)
	if err != nil {
		return fmt.Errorf("fetch SHA256SUMS: %w", err)
	}
	defer sumResp.Body.Close()
	if sumResp.StatusCode != http.StatusOK {
		return fmt.Errorf("SHA256SUMS HTTP %d", sumResp.StatusCode)
	}
	expectedSum, err := parseSHA256SumsForAsset(sumResp.Body, asset)
	if err != nil {
		return err
	}

	binURL := fmt.Sprintf("%s/%s/%s", baseURL, targetVersion, asset)
	// #nosec G704 -- fixed GitHub Release binary URL.
	binReq, err := http.NewRequestWithContext(ctx, http.MethodGet, binURL, nil)
	if err != nil {
		return err
	}
	binReq.Header.Set("User-Agent", "Yundu-Agent/"+appVersion)
	// #nosec G704 -- fixed GitHub Release binary URL.
	binResp, err := client.Do(binReq)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer binResp.Body.Close()
	if binResp.StatusCode != http.StatusOK {
		return fmt.Errorf("binary download HTTP %d", binResp.StatusCode)
	}

	dir := filepath.Dir(exePath)
	tmpFile, err := os.CreateTemp(dir, ".meridian-upgrade-*")
	if err != nil {
		return fmt.Errorf("目录无写入权限（请重跑一次 install.sh agent 更新服务权限）: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		_ = tmpFile.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, hasher), io.LimitReader(binResp.Body, edgeAgentMaxBinaryBytes+1))
	if err != nil {
		return fmt.Errorf("write binary: %w", err)
	}
	if written == 0 || written > edgeAgentMaxBinaryBytes {
		return errors.New("downloaded binary size out of bounds")
	}
	actualSum := hex.EncodeToString(hasher.Sum(nil))
	if actualSum != expectedSum {
		return fmt.Errorf("SHA-256 校验不匹配")
	}
	if err := tmpFile.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	// #nosec G703 -- exePath is resolved from os.Executable() (not user request input).
	if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("替换可执行文件失败（请重跑一次 install.sh agent 更新服务权限）: %w", err)
	}
	cleanup = false
	return nil
}

// ---------------------------------------------------------------------------
// Master Telegram alerting for Edge Nodes (offline/recovery/quota 80% & 100%)

func runEdgeNodeAlertMonitor(ctx context.Context, db *DB, rt *edgeNodeRuntime) {
	if db == nil || rt == nil {
		return
	}
	ticker := time.NewTicker(edgeAlertCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.checkAndSendAlerts(ctx, db)
		}
	}
}

func (rt *edgeNodeRuntime) checkAndSendAlerts(ctx context.Context, db *DB) {
	if rt == nil || db == nil {
		return
	}
	messages := rt.collectAlerts(time.Now())
	if len(messages) == 0 {
		return
	}
	stored, err := db.telegramReportSettings()
	if err != nil || !stored.Configured {
		return
	}
	if !stored.Enabled && (stored.Bot == nil || !stored.Bot.Enabled) {
		return
	}
	botToken, err := decryptTelegramBotToken(stored.BotTokenCiphertext)
	if err != nil || strings.TrimSpace(botToken) == "" || strings.TrimSpace(stored.ChatID) == "" {
		return
	}
	for _, msg := range messages {
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if sendErr := sendTelegramReport(sendCtx, botToken, stored.ChatID, msg); sendErr != nil {
			log.Printf("edge node TG alert failed: %v", sendErr)
		}
		cancel()
	}
}

func (rt *edgeNodeRuntime) collectAlerts(now time.Time) []string {
	if rt == nil {
		return nil
	}
	var messages []string

	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, node := range rt.nodes {
		if !node.enabled {
			continue
		}
		online := node.isOnline(now)
		if !node.lastSeen.IsZero() {
			if online {
				if node.alertKnownOffline {
					node.alertKnownOffline = false
					messages = append(messages, fmt.Sprintf("✅ 云渡从机恢复上线\n节点：%s\n版本：%s\n地址：%s",
						node.name, nonEmptyOrDash(node.agentVersion), nonEmptyOrDash(node.publicURL())))
				}
				node.alertSeenOnline = true
			} else if node.alertSeenOnline && !node.alertKnownOffline {
				node.alertKnownOffline = true
				messages = append(messages, fmt.Sprintf("⚠️ 云渡从机已离线\n节点：%s\n地址：%s\n状态：已自动降级到下一顺位节点",
					node.name, nonEmptyOrDash(node.publicURL())))
			}
		}
		if node.quota > 0 {
			used := node.totalUsed()
			period := node.resetPeriod
			if period == "" {
				period = "total"
			}
			pct := float64(used) * 100 / float64(node.quota)
			if used >= node.quota && node.alertQuota100Period != period {
				node.alertQuota100Period = period
				node.alertQuota80Period = period
				messages = append(messages, fmt.Sprintf("🚨 云渡从机流量已耗尽（100%%）\n节点：%s\n已用：%s / %s\n状态：已自动暂停分流并降级",
					node.name, formatBytesForAlert(used), formatBytesForAlert(node.quota)))
			} else if pct >= 80 && node.alertQuota80Period != period {
				node.alertQuota80Period = period
				messages = append(messages, fmt.Sprintf("📊 云渡从机流量预警（%.0f%%）\n节点：%s\n已用：%s / %s",
					pct, node.name, formatBytesForAlert(used), formatBytesForAlert(node.quota)))
			}
		}
	}
	return messages
}

func nonEmptyOrDash(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "—"
	}
	return v
}

func formatBytesForAlert(b int64) string {
	const (
		gb = 1024 * 1024 * 1024
		mb = 1024 * 1024
	)
	if b >= gb {
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	}
	if b >= mb {
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	}
	return fmt.Sprintf("%d B", b)
}
