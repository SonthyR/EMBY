package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	xdbStructure20      = 2
	xdbStructure30      = 3
	xdbHeaderLength     = 256
	xdbVectorIndexCols  = 256
	xdbVectorIndexSize  = 8
	xdbVectorIndexBytes = 256 * 256 * 8
	xdbMinFileBytes     = xdbHeaderLength + xdbVectorIndexBytes
	xdbMaxDownloadBytes = 128 << 20

	xdbIPv4VersionNo = 4
	xdbIPv6VersionNo = 6
	xdbIPv4SegSize   = 14 // 4 + 4 + 2 + 4
	xdbIPv6SegSize   = 38 // 16 + 16 + 2 + 4

	ispGroupCT    = "ct"
	ispGroupCU    = "cu"
	ispGroupCM    = "cm"
	ispGroupOther = "other"
)

var ip2regionMirrorBases = []string{
	"https://gcore.jsdelivr.net/gh/lionsoul2014/ip2region@master/data/",
	"https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/",
	"https://ghproxy.net/https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/",
}

type ip2regionResult struct {
	Found    bool   `json:"found"`
	Raw      string `json:"raw,omitempty"`
	Region   string `json:"region"`
	ISP      string `json:"isp"`
	ISPGroup string `json:"isp_group"`
	ISPLabel string `json:"isp_label"`
}

func ispGroupLabel(group string) string {
	switch group {
	case ispGroupCT:
		return "电信"
	case ispGroupCU:
		return "联通"
	case ispGroupCM:
		return "移动"
	default:
		return "其他"
	}
}

func validISPGroup(group string) bool {
	switch group {
	case ispGroupCT, ispGroupCU, ispGroupCM, ispGroupOther:
		return true
	default:
		return false
	}
}

var allISPGroups = []string{ispGroupCT, ispGroupCU, ispGroupCM, ispGroupOther}

// classifyISPGroup maps an ip2region ISP string into one of the four routing groups.
func classifyISPGroup(isp string) string {
	lower := strings.ToLower(strings.TrimSpace(isp))
	if lower == "" || lower == "0" {
		return ispGroupOther
	}
	switch {
	case strings.Contains(lower, "电信"),
		strings.Contains(lower, "chinanet"),
		strings.Contains(lower, "chinatelecom"),
		strings.Contains(lower, "china telecom"),
		strings.Contains(lower, "ctcc"):
		return ispGroupCT
	case strings.Contains(lower, "联通"),
		strings.Contains(lower, "unicom"),
		strings.Contains(lower, "china169"),
		strings.Contains(lower, "cncgroup"):
		return ispGroupCU
	case strings.Contains(lower, "移动"),
		strings.Contains(lower, "cmcc"),
		strings.Contains(lower, "cmnet"),
		strings.Contains(lower, "chinamobile"),
		strings.Contains(lower, "china mobile"),
		strings.Contains(lower, "铁通"),
		strings.Contains(lower, "tietong"):
		return ispGroupCM
	default:
		return ispGroupOther
	}
}

// parseIP2RegionRaw parses ip2region's pipe-delimited record into a clean region
// label and ISP name. Supports both 5-field (Country|Area|Province|City|ISP)
// and 4-field (Country|Province|City|ISP) schemas.
func parseIP2RegionRaw(raw string) ip2regionResult {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ip2regionResult{ISPGroup: ispGroupOther, ISPLabel: ispGroupLabel(ispGroupOther)}
	}
	fields := strings.Split(raw, "|")
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" || s == "0" {
			return ""
		}
		return s
	}
	var regionParts []string
	var isp string
	switch {
	case len(fields) >= 5:
		for _, idx := range []int{0, 2, 3} {
			if v := clean(fields[idx]); v != "" {
				if len(regionParts) == 0 || !strings.EqualFold(regionParts[len(regionParts)-1], v) {
					regionParts = append(regionParts, v)
				}
			}
		}
		isp = clean(fields[4])
	case len(fields) == 4:
		for _, idx := range []int{0, 1, 2} {
			if v := clean(fields[idx]); v != "" {
				if len(regionParts) == 0 || !strings.EqualFold(regionParts[len(regionParts)-1], v) {
					regionParts = append(regionParts, v)
				}
			}
		}
		isp = clean(fields[3])
	default:
		for _, field := range fields {
			if v := clean(field); v != "" {
				regionParts = append(regionParts, v)
			}
		}
	}
	group := classifyISPGroup(isp)
	return ip2regionResult{
		Found:    len(regionParts) > 0 || isp != "",
		Raw:      raw,
		Region:   strings.Join(regionParts, " · "),
		ISP:      isp,
		ISPGroup: group,
		ISPLabel: ispGroupLabel(group),
	}
}

type xdbHeader struct {
	Version       uint16
	IndexPolicy   uint16
	CreatedAt     uint32
	StartIndexPtr uint32
	EndIndexPtr   uint32
	IPVersion     int
}

func parseXDBHeader(buf []byte) (xdbHeader, error) {
	if len(buf) < 20 {
		return xdbHeader{}, errors.New("xdb header truncated")
	}
	h := xdbHeader{
		Version:       binary.LittleEndian.Uint16(buf[0:2]),
		IndexPolicy:   binary.LittleEndian.Uint16(buf[2:4]),
		CreatedAt:     binary.LittleEndian.Uint32(buf[4:8]),
		StartIndexPtr: binary.LittleEndian.Uint32(buf[8:12]),
		EndIndexPtr:   binary.LittleEndian.Uint32(buf[12:16]),
		IPVersion:     int(binary.LittleEndian.Uint16(buf[16:18])),
	}
	switch h.Version {
	case xdbStructure20:
		h.IPVersion = xdbIPv4VersionNo
	case xdbStructure30:
		if h.IPVersion != xdbIPv4VersionNo && h.IPVersion != xdbIPv6VersionNo {
			return xdbHeader{}, fmt.Errorf("unsupported xdb ip version %d", h.IPVersion)
		}
	default:
		return xdbHeader{}, fmt.Errorf("unsupported xdb format version %d", h.Version)
	}
	return h, nil
}

// xdbReader is a concurrency-safe pure-Go reader for ip2region xdb v2/v3 files.
// IPv4 databases are loaded entirely in memory; IPv6 databases cache the 512 KiB
// vector index and read segment/region bytes via positional ReadAt.
type xdbReader struct {
	ipVersion   int
	ipBytes     int
	segSize     uint32
	contentBuff []byte
	vectorIndex []byte
	file        *os.File
	fileSize    int64
}

func openXDBReader(path string, expectedIPVersion int, loadFullBuffer bool) (*xdbReader, error) {
	// #nosec G304 -- path is built solely from operator-controlled dbDir and fixed xdb filenames.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.Size() < xdbMinFileBytes {
		_ = f.Close()
		return nil, fmt.Errorf("xdb file too small (%d bytes)", info.Size())
	}
	headerBuf := make([]byte, xdbHeaderLength)
	if _, err := io.ReadFull(f, headerBuf); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read xdb header: %w", err)
	}
	header, err := parseXDBHeader(headerBuf)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if expectedIPVersion != 0 && header.IPVersion != expectedIPVersion {
		_ = f.Close()
		return nil, fmt.Errorf("xdb ip version mismatch: got IPv%d, want IPv%d", header.IPVersion, expectedIPVersion)
	}
	ipBytes := 4
	segSize := uint32(xdbIPv4SegSize)
	if header.IPVersion == xdbIPv6VersionNo {
		ipBytes = 16
		segSize = uint32(xdbIPv6SegSize)
	}
	if loadFullBuffer {
		if info.Size() > xdbMaxDownloadBytes {
			_ = f.Close()
			return nil, fmt.Errorf("xdb file exceeds limit")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return &xdbReader{
			ipVersion:   header.IPVersion,
			ipBytes:     ipBytes,
			segSize:     segSize,
			contentBuff: data,
			fileSize:    int64(len(data)),
		}, nil
	}
	vIndex := make([]byte, xdbVectorIndexBytes)
	if _, err := io.ReadFull(f, vIndex); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read xdb vector index: %w", err)
	}
	return &xdbReader{
		ipVersion:   header.IPVersion,
		ipBytes:     ipBytes,
		segSize:     segSize,
		vectorIndex: vIndex,
		file:        f,
		fileSize:    info.Size(),
	}, nil
}

func (r *xdbReader) close() {
	if r != nil && r.file != nil {
		_ = r.file.Close()
	}
}

func (r *xdbReader) readAt(offset int64, dst []byte) error {
	if offset < xdbHeaderLength || offset+int64(len(dst)) > r.fileSize {
		return fmt.Errorf("xdb read out of bounds: offset=%d len=%d", offset, len(dst))
	}
	if r.contentBuff != nil {
		copy(dst, r.contentBuff[offset:offset+int64(len(dst))])
		return nil
	}
	n, err := r.file.ReadAt(dst, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(dst) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func xdbCompareIPv4(ipBigEndian, segLittleEndian []byte) int {
	for i, j := 0, 3; i < 4; i, j = i+1, j-1 {
		if ipBigEndian[i] < segLittleEndian[j] {
			return -1
		}
		if ipBigEndian[i] > segLittleEndian[j] {
			return 1
		}
	}
	return 0
}

func (r *xdbReader) search(ipBytes []byte) (string, error) {
	if r == nil || len(ipBytes) != r.ipBytes {
		return "", nil
	}
	il0, il1 := int(ipBytes[0]), int(ipBytes[1])
	idx := il0*xdbVectorIndexCols*xdbVectorIndexSize + il1*xdbVectorIndexSize
	var sPtr, ePtr uint32
	if r.contentBuff != nil {
		sPtr = binary.LittleEndian.Uint32(r.contentBuff[xdbHeaderLength+idx:])
		ePtr = binary.LittleEndian.Uint32(r.contentBuff[xdbHeaderLength+idx+4:])
	} else {
		sPtr = binary.LittleEndian.Uint32(r.vectorIndex[idx:])
		ePtr = binary.LittleEndian.Uint32(r.vectorIndex[idx+4:])
	}
	if sPtr == 0 || ePtr == 0 || ePtr < sPtr {
		return "", nil
	}
	segBuf := make([]byte, r.segSize)
	dBytes := r.ipBytes * 2
	l, h := 0, int((ePtr-sPtr)/r.segSize)
	dataLen := 0
	var dataPtr uint32
	for l <= h {
		m := (l + h) >> 1
		p := sPtr + uint32(m)*r.segSize
		if err := r.readAt(int64(p), segBuf); err != nil {
			return "", err
		}
		var cmpStart, cmpEnd int
		if r.ipVersion == xdbIPv4VersionNo {
			cmpStart = xdbCompareIPv4(ipBytes, segBuf[0:4])
			if cmpStart >= 0 {
				cmpEnd = xdbCompareIPv4(ipBytes, segBuf[4:8])
			}
		} else {
			cmpStart = bytes.Compare(ipBytes, segBuf[0:16])
			if cmpStart >= 0 {
				cmpEnd = bytes.Compare(ipBytes, segBuf[16:32])
			}
		}
		if cmpStart < 0 {
			h = m - 1
		} else if cmpEnd > 0 {
			l = m + 1
		} else {
			dataLen = int(binary.LittleEndian.Uint16(segBuf[dBytes : dBytes+2]))
			dataPtr = binary.LittleEndian.Uint32(segBuf[dBytes+2 : dBytes+6])
			break
		}
	}
	if dataLen == 0 {
		return "", nil
	}
	regionBuf := make([]byte, dataLen)
	if err := r.readAt(int64(dataPtr), regionBuf); err != nil {
		return "", err
	}
	return string(regionBuf), nil
}

type ip2regionDBStatus struct {
	Version    string `json:"version"`
	Downloaded bool   `json:"downloaded"`
	SizeBytes  int64  `json:"size_bytes"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

type ip2regionStatusResponse struct {
	V4      ip2regionDBStatus `json:"v4"`
	V6      ip2regionDBStatus `json:"v6"`
	Mirrors []string          `json:"mirrors"`
}

type ip2regionManager struct {
	dir     string
	client  *http.Client
	mirrors []string

	mu       sync.RWMutex
	v4       *xdbReader
	v6       *xdbReader
	updateMu sync.Mutex
}

func newIP2RegionManager(dir string, client *http.Client) *ip2regionManager {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	mgr := &ip2regionManager{
		dir:     strings.TrimSpace(dir),
		client:  client,
		mirrors: append([]string(nil), ip2regionMirrorBases...),
	}
	if mgr.dir != "" {
		mgr.loadExisting()
	}
	return mgr
}

func ip2regionFilename(version string) (string, int, error) {
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "v4", "ipv4":
		return "ip2region_v4.xdb", xdbIPv4VersionNo, nil
	case "v6", "ipv6":
		return "ip2region_v6.xdb", xdbIPv6VersionNo, nil
	default:
		return "", 0, errors.New("version must be v4 or v6")
	}
}

func (m *ip2regionManager) filePath(version string) (string, int, error) {
	name, ipVer, err := ip2regionFilename(version)
	if err != nil {
		return "", 0, err
	}
	if m == nil || m.dir == "" {
		return "", ipVer, errors.New("IP 地区库目录不可用")
	}
	return filepath.Join(m.dir, name), ipVer, nil
}

func (m *ip2regionManager) loadExisting() {
	if path, ipVer, err := m.filePath("v4"); err == nil {
		if reader, err := openXDBReader(path, ipVer, true); err == nil {
			m.mu.Lock()
			m.v4 = reader
			m.mu.Unlock()
		}
	}
	if path, ipVer, err := m.filePath("v6"); err == nil {
		if reader, err := openXDBReader(path, ipVer, false); err == nil {
			m.mu.Lock()
			m.v6 = reader
			m.mu.Unlock()
		}
	}
}

func (m *ip2regionManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	v4, v6 := m.v4, m.v6
	m.v4, m.v6 = nil, nil
	m.mu.Unlock()
	v4.close()
	v6.close()
}

func (m *ip2regionManager) Lookup(ip string) ip2regionResult {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip2regionResult{ISPGroup: ispGroupOther, ISPLabel: ispGroupLabel(ispGroupOther)}
	}
	addr = addr.Unmap()
	if !dynamicIPIsPublic(addr) {
		return ip2regionResult{
			Found:    true,
			Region:   clientIPRegionLocal,
			ISPGroup: ispGroupOther,
			ISPLabel: ispGroupLabel(ispGroupOther),
		}
	}
	if m == nil {
		return ip2regionResult{ISPGroup: ispGroupOther, ISPLabel: ispGroupLabel(ispGroupOther)}
	}
	m.mu.RLock()
	var raw string
	if addr.Is4() && m.v4 != nil {
		b := addr.As4()
		raw, _ = m.v4.search(b[:])
	} else if addr.Is6() && m.v6 != nil {
		b := addr.As16()
		raw, _ = m.v6.search(b[:])
	}
	m.mu.RUnlock()
	if raw == "" {
		return ip2regionResult{ISPGroup: ispGroupOther, ISPLabel: ispGroupLabel(ispGroupOther)}
	}
	return parseIP2RegionRaw(raw)
}

func (m *ip2regionManager) LookupDisplay(ip string) (string, bool) {
	res := m.Lookup(ip)
	if !res.Found {
		return "", false
	}
	if res.Region != "" && res.ISP != "" {
		return res.Region + " · " + res.ISP, true
	}
	if res.Region != "" {
		return res.Region, true
	}
	if res.ISP != "" {
		return res.ISP, true
	}
	return "", false
}

func (m *ip2regionManager) Status() ip2regionStatusResponse {
	resp := ip2regionStatusResponse{
		V4:      ip2regionDBStatus{Version: "v4"},
		V6:      ip2regionDBStatus{Version: "v6"},
		Mirrors: append([]string(nil), ip2regionMirrorBases...),
	}
	if m == nil || m.dir == "" {
		return resp
	}
	inspect := func(version string, dst *ip2regionDBStatus) {
		path, _, err := m.filePath(version)
		if err != nil {
			return
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			dst.Downloaded = true
			dst.SizeBytes = info.Size()
			dst.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
	}
	inspect("v4", &resp.V4)
	inspect("v6", &resp.V6)
	return resp
}

func (m *ip2regionManager) Update(ctx context.Context, version string) (ip2regionDBStatus, error) {
	if m == nil || m.dir == "" {
		return ip2regionDBStatus{}, errors.New("IP 地区库目录不可用")
	}
	filename, ipVer, err := ip2regionFilename(version)
	if err != nil {
		return ip2regionDBStatus{}, err
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return ip2regionDBStatus{}, fmt.Errorf("创建目录失败: %w", err)
	}
	finalPath := filepath.Join(m.dir, filename)
	tmpPath := finalPath + ".tmp"
	defer func() { _ = os.Remove(tmpPath) }()

	var lastErr error
	downloaded := false
	for _, base := range m.mirrors {
		targetURL := strings.TrimRight(base, "/") + "/" + filename
		if err := m.downloadToFile(ctx, targetURL, tmpPath); err != nil {
			lastErr = err
			continue
		}
		// Validate the downloaded xdb file before replacing the live copy.
		probe, err := openXDBReader(tmpPath, ipVer, false)
		if err != nil {
			lastErr = fmt.Errorf("校验下载文件失败: %w", err)
			continue
		}
		probe.close()
		downloaded = true
		break
	}
	if !downloaded {
		if lastErr == nil {
			lastErr = errors.New("所有镜像源均下载失败")
		}
		return ip2regionDBStatus{}, fmt.Errorf("下载失败: %w", lastErr)
	}

	// On Windows an open file cannot be overwritten in-place, so close any
	// file-backed reader before renaming, then reopen immediately.
	m.mu.Lock()
	if ipVer == xdbIPv6VersionNo && m.v6 != nil {
		m.v6.close()
		m.v6 = nil
	}
	renameErr := os.Rename(tmpPath, finalPath)
	if renameErr != nil {
		_ = os.Remove(finalPath)
		renameErr = os.Rename(tmpPath, finalPath)
	}
	if renameErr != nil {
		if ipVer == xdbIPv6VersionNo {
			if restored, err := openXDBReader(finalPath, ipVer, false); err == nil {
				m.v6 = restored
			}
		}
		m.mu.Unlock()
		return ip2regionDBStatus{}, fmt.Errorf("保存地区库失败: %w", renameErr)
	}
	reader, openErr := openXDBReader(finalPath, ipVer, ipVer == xdbIPv4VersionNo)
	if openErr != nil {
		m.mu.Unlock()
		return ip2regionDBStatus{}, fmt.Errorf("加载地区库失败: %w", openErr)
	}
	if ipVer == xdbIPv4VersionNo {
		old := m.v4
		m.v4 = reader
		old.close()
	} else {
		old := m.v6
		m.v6 = reader
		old.close()
	}
	m.mu.Unlock()

	status := m.Status()
	if ipVer == xdbIPv4VersionNo {
		return status.V4, nil
	}
	return status.V6, nil
}

func (m *ip2regionManager) downloadToFile(ctx context.Context, targetURL, dstPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Yundu/"+appVersion)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// #nosec G304 -- dstPath is inside operator-controlled ip2region directory.
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, xdbMaxDownloadBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > xdbMaxDownloadBytes {
		return errors.New("文件体积超过限制")
	}
	if written < xdbMinFileBytes {
		return errors.New("下载文件不完整")
	}
	return nil
}

func (m *ip2regionManager) Delete(version string) error {
	path, ipVer, err := m.filePath(version)
	if err != nil {
		return err
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	m.mu.Lock()
	if ipVer == xdbIPv4VersionNo {
		m.v4.close()
		m.v4 = nil
	} else {
		m.v6.close()
		m.v6 = nil
	}
	m.mu.Unlock()

	// #nosec G703 -- path is derived from m.dataDir plus a fixed enum filename (ip2region_v4.xdb or ip2region_v6.xdb).
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
