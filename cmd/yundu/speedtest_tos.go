package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tosSpeedtestDuration      = 10 * time.Second
	tosSpeedtestDownloadRange = "bytes=0-524287999"
	tosSpeedtestBytes         = 500 << 20
	tosMinimumDownloadBytes   = 64 << 10
	maxTOSDirectoryCandidates = 16
	maxTOSCandidates          = maxTOSDirectoryCandidates + maxSpeedtestDNSCandidates
	tosProbeParallelism       = 4
	tosProbeTimeout           = 4 * time.Second
	tosCatalogTimeout         = 5 * time.Second
)

// IPv4 snapshot of the public TcpQuality TOS catalog on 2026-11-24
// (https://tcpquality.ibsgss.uk/getNodes?format=tsv&scope=tos).
// Use only when the live catalog has no addresses for the matching group.
var tosCatalogFallback = map[string][]string{
	"bj-ct": {"42.81.80.86"},
	"bj-cu": {"119.250.10.100", "119.250.10.0", "119.250.10.102", "221.194.175.109"},
	"bj-cm": {"120.255.0.180"},
	"sh-ct": {"180.97.50.1"},
	"sh-cu": {"122.97.126.101", "122.97.126.106", "116.147.21.1"},
	"sh-cm": {"36.150.251.100", "36.150.251.10", "36.156.86.79", "36.156.86.18", "223.109.53.225", "223.109.53.191", "36.151.164.131"},
	"gd-ct": {"14.119.66.1"},
	"gd-cu": {"36.249.75.208", "36.249.75.222", "36.250.245.237", "36.249.75.220", "157.255.228.102"},
	"gd-cm": {"112.48.236.196", "112.47.9.244", "112.48.236.199", "112.48.236.253", "112.48.236.197", "112.48.201.216", "183.232.151.102"},
}

func tosTransferBps(bytes int64, elapsed time.Duration) int64 {
	return int64(float64(bytes) / max(elapsed, 10*time.Millisecond).Seconds())
}

func canonicalTOSSpeedtestHost(host string) string {
	switch strings.ToLower(host) {
	case volcengineTOSBeijingHost:
		return volcengineTOSBeijingHost
	case volcengineTOSShanghaiHost:
		return volcengineTOSShanghaiHost
	case volcengineTOSGuangzhouHost:
		return volcengineTOSGuangzhouHost
	default:
		return ""
	}
}

func isTOSSpeedtestHost(host string) bool {
	return canonicalTOSSpeedtestHost(host) != ""
}

func validTOSOrigin(u *url.URL, host string) bool {
	return host != "" && u.Scheme == "https" && strings.EqualFold(u.Hostname(), host) &&
		(u.Port() == "" || u.Port() == "443") && u.User == nil &&
		u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == ""
}

func tosUploadObjectKey(path string) string {
	key, ok := strings.CutPrefix(path, "/upload/yundu-")
	if !ok || len(key) != 16 {
		return ""
	}
	if _, err := hex.DecodeString(key); err != nil {
		return ""
	}
	return key
}

func validateTOSRange(resp *http.Response, maxBytes int64) (int64, error) {
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("测试文件返回 HTTP %d", resp.StatusCode)
	}
	rangeValue, ok := strings.CutPrefix(resp.Header.Get("Content-Range"), "bytes 0-")
	endText, totalText, split := strings.Cut(rangeValue, "/")
	if !ok || !split {
		return 0, errors.New("测速响应缺少有效的 Content-Range")
	}
	end, err := strconv.ParseInt(endText, 10, 64)
	total, totalErr := strconv.ParseInt(totalText, 10, 64)
	if err != nil || totalErr != nil || end < 0 || end >= maxBytes || total <= end ||
		(resp.ContentLength >= 0 && resp.ContentLength != end+1) {
		return 0, errors.New("测速响应的字节范围无效")
	}
	return end + 1, nil
}

// newTOSTarget combines catalogue and ECS IPs. The local province definition
// supplies the hostname and paths so HTTP Host and TLS SNI stay correct.
func newTOSTarget(def speedtestProvinceDef, group string, directoryIPs, ecsIPs []string, duration time.Duration) agentSpeedtestTarget {
	label := ispGroupLabel(group)
	target := agentSpeedtestTarget{
		TargetKey:  def.Code + "-" + group,
		Label:      def.Label + label,
		Province:   def.Label,
		ISPGroup:   group,
		ISPLabel:   label,
		TargetKind: "builtin",
		DurationMs: int(duration / time.Millisecond),
	}
	base := "https://" + def.TOSHost
	for _, ip := range filterPublicIPv4Candidates(slices.Concat(directoryIPs, ecsIPs), maxTOSCandidates) {
		target.Candidates = append(target.Candidates, speedtestCandidate{
			Label:       ip,
			ResolvedIP:  ip,
			UploadURL:   base + "/upload/yundu-" + newSpeedtestRandomHex(8),
			DownloadURL: base + "/download/test",
			Method:      http.MethodPut,
		})
	}
	// Older Agents also read the first candidate from the top-level fields.
	if len(target.Candidates) > 0 {
		first := target.Candidates[0]
		target.ResolvedIP, target.UploadURL, target.DownloadURL, target.Method =
			first.ResolvedIP, first.UploadURL, first.DownloadURL, first.Method
	}
	return target
}

func (m *speedtestManager) buildTOSTargets(provinces []speedtestProvinceDef, groups []string, duration time.Duration) []agentSpeedtestTarget {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var catalog map[string][]string
	wg.Go(func() { catalog = m.loadTOSCatalog(ctx) })

	resolved := make([][]string, len(provinces)*len(groups))
	for pi, def := range provinces {
		for gi, group := range groups {
			wg.Go(func() {
				resolved[pi*len(groups)+gi] = m.resolveVolcengineTOSIPs(ctx, def.TOSHost, def.ecsForISP(group))
			})
		}
	}
	wg.Wait()

	targets := make([]agentSpeedtestTarget, 0, len(resolved))
	for pi, def := range provinces {
		for gi, group := range groups {
			key := def.Code + "-" + group
			directoryIPs := catalog[key]
			if len(directoryIPs) == 0 {
				directoryIPs = tosCatalogFallback[key]
			}
			targets = append(targets, newTOSTarget(def, group, directoryIPs, resolved[pi*len(groups)+gi], duration))
		}
	}
	return targets
}

// probeTOSCandidate reads one byte over the same pinned HTTPS path used for
// the speed measurement. Probe bytes are never included in the result.
func probeTOSCandidate(ctx context.Context, target agentSpeedtestTarget, transport *http.Transport) error {
	if !isPublicUnicastIPv4(target.ResolvedIP) {
		return errors.New("无有效的公网 IPv4")
	}
	provinceCode, _, _ := strings.Cut(target.TargetKey, "-")
	province, _ := lookupSpeedtestProvince(provinceCode)
	uploadURL, err := url.Parse(target.UploadURL)
	if err != nil || !validTOSOrigin(uploadURL, province.TOSHost) ||
		target.Method != http.MethodPut || tosUploadObjectKey(uploadURL.Path) == "" {
		return errors.New("测速上传地址无效")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.DownloadURL, nil)
	if err != nil || !validTOSOrigin(req.URL, province.TOSHost) || req.URL.Path != "/download/test" {
		return errors.New("测速文件地址无效")
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", speedtestBrowserUserAgent)
	client, getConn := newSpeedtestSingleConnHTTPClient(target.ResolvedIP, province.TOSHost, tosProbeTimeout, transport)
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		timedOut := errors.As(err, &netErr) && netErr.Timeout()
		if getConn() == nil {
			if timedOut {
				return errors.New("TCP 443 连接超时")
			}
			return fmt.Errorf("TCP 443 连接失败（%s）", conciseSpeedtestError(err.Error()))
		}
		if timedOut {
			return errors.New("HTTPS 探测超时")
		}
		return fmt.Errorf("HTTPS 探测失败（%s）", conciseSpeedtestError(err.Error()))
	}
	defer resp.Body.Close()
	if _, err := validateTOSRange(resp, 1); err != nil {
		return err
	}
	var firstByte [1]byte
	if _, err := io.ReadFull(resp.Body, firstByte[:]); err != nil {
		return fmt.Errorf("读取测试文件失败（%s）", conciseSpeedtestError(err.Error()))
	}
	return nil
}

// A completed PUT may leave an object in the probe bucket. Remove only the
// unique object just uploaded, over the same pinned TOS client.
func deleteTOSSpeedtestUpload(ctx context.Context, client *http.Client, uploaded *http.Request) {
	host := canonicalTOSSpeedtestHost(uploaded.URL.Hostname())
	key := tosUploadObjectKey(uploaded.URL.Path)
	if !validTOSOrigin(uploaded.URL, host) || key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cleanupURL := &url.URL{Scheme: "https", Host: host, Path: "/upload/yundu-" + key}
	// #nosec G704 -- host is one of the fixed TOS constants and key is a validated 16-digit hex path component.
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cleanupURL.String(), nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req) // #nosec G704 -- fixed TOS HTTPS origin; the client pins the IP and rejects redirects.
	if err == nil {
		_ = resp.Body.Close()
	}
}

// executeTOSTarget screens candidates in small waves on the executing node.
// The upload and download measurement are then run sequentially on one IP.
func executeTOSTarget(ctx context.Context, target agentSpeedtestTarget, dur time.Duration, transport *http.Transport) speedtestTargetResult {
	rowCtx, cancel := context.WithTimeout(ctx, 2*dur+22*time.Second)
	defer cancel()
	candidates := target.Candidates[:min(len(target.Candidates), maxTOSCandidates)]
	if len(candidates) == 0 && isPublicUnicastIPv4(target.ResolvedIP) {
		candidates = []speedtestCandidate{{ResolvedIP: target.ResolvedIP}}
	}
	result := newSpeedtestTargetResult(target)
	result.Status = "error"
	if len(candidates) == 0 {
		result.ResolvedIP = ""
		result.Error = "该地区和运营商没有可用的 IPv4 测速候选"
		return result
	}

	checked, reachable := 0, 0
	var partial *speedtestTargetResult
	for batch := range slices.Chunk(candidates, tosProbeParallelism) {
		if rowCtx.Err() != nil {
			break
		}
		targets := make([]agentSpeedtestTarget, len(batch))
		probeErrors := make([]error, len(batch))
		var wg sync.WaitGroup
		for i, candidate := range batch {
			targets[i] = speedtestTargetWithCandidate(target, candidate)
			wg.Go(func() { probeErrors[i] = probeTOSCandidate(rowCtx, targets[i], transport) })
		}
		wg.Wait()
		checked += len(batch)
		for i, probeErr := range probeErrors {
			if probeErr != nil {
				if reachable == 0 {
					result.ResolvedIP = targets[i].ResolvedIP
					result.ResolvedNode = targets[i].ResolvedIP
					result.Error = probeErr.Error()
				}
				continue
			}
			reachable++
			if rowCtx.Err() != nil {
				break
			}
			current := executeSingleSpeedtestCandidate(rowCtx, targets[i], dur, transport)
			current.ResolvedNode = targets[i].ResolvedIP
			if current.Status == "done" {
				return current
			}
			if partial == nil && (current.ReturnBps > 0 || current.OutboundBps > 0) {
				partial = &current
			}
			result = current
		}
	}
	if partial != nil && ctx.Err() == nil {
		return *partial
	}
	switch {
	case ctx.Err() != nil:
		result.Error = "测速已取消"
	case rowCtx.Err() != nil:
		result.Error = fmt.Sprintf("测速总时限已到（已探测 %d/%d 个候选，%d 个可访问）", checked, len(candidates), reachable)
	case reachable == 0:
		result.Error = fmt.Sprintf("从测速节点探测的 %d 个候选均不可访问（%s）", checked, result.Error)
	default:
		result.Error = fmt.Sprintf("探测到 %d 个可访问候选，实际测速均失败（%s）", reachable, result.Error)
	}
	return result
}
