package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRepo      = "fran0220/amp-proxy-neo"
	LaunchAgentLabel = "com.fan.amp-proxy.neo"
)

type Options struct {
	Repo          string
	Current       string
	Channel       string
	APIBase       string
	CacheDir      string
	BinaryPath    string
	HTTPClient    *http.Client
	ProbeTimeout  time.Duration
	Restart       bool
	LaunchctlPath string
}

type Updater struct {
	repo          string
	current       string
	channel       string
	apiBase       string
	cacheDir      string
	binaryPath    string
	hc            *http.Client
	probeTimeout  time.Duration
	restart       bool
	launchctlPath string

	mu              sync.RWMutex
	lastCheck       time.Time
	updateAvailable bool
	lastVersion     string
}

type Info struct {
	CurrentVersion string    `json:"current_version"`
	LatestVersion  string    `json:"latest_version,omitempty"`
	Channel        string    `json:"channel"`
	Available      bool      `json:"available"`
	AssetName      string    `json:"asset_name,omitempty"`
	DownloadURL    string    `json:"download_url,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
	Message        string    `json:"message,omitempty"`
}

func New(opts Options) *Updater {
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.Current == "" {
		opts.Current = "dev"
	}
	if opts.Channel == "" {
		opts.Channel = "stable"
	}
	if opts.APIBase == "" {
		opts.APIBase = "https://api.github.com"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = 5 * time.Second
	}
	if opts.CacheDir == "" {
		home, _ := os.UserHomeDir()
		opts.CacheDir = filepath.Join(home, "Library", "Caches", "amp-proxy-neo")
	}
	if opts.BinaryPath == "" {
		opts.BinaryPath = "/Applications/AmpProxyNeo.app/Contents/MacOS/amp-proxy-neo"
	}
	if opts.LaunchctlPath == "" {
		opts.LaunchctlPath = "launchctl"
	}
	return &Updater{repo: opts.Repo, current: opts.Current, channel: normalizeChannel(opts.Channel), apiBase: strings.TrimRight(opts.APIBase, "/"), cacheDir: opts.CacheDir, binaryPath: opts.BinaryPath, hc: opts.HTTPClient, probeTimeout: opts.ProbeTimeout, restart: opts.Restart, launchctlPath: opts.LaunchctlPath}
}

func (u *Updater) LastCheck() (time.Time, bool, string) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.lastCheck, u.updateAvailable, u.lastVersion
}

func (u *Updater) Check(ctx context.Context) (*Info, error) {
	info := &Info{CurrentVersion: u.current, Channel: u.channel, CheckedAt: time.Now()}
	if u.channel == "off" {
		info.Message = "updates disabled"
		u.record(info)
		return info, nil
	}

	releases, err := u.fetchReleases(ctx)
	if err != nil {
		return nil, err
	}
	rel, ok := selectRelease(releases, u.channel, u.current)
	if !ok {
		info.Message = "no update"
		u.record(info)
		return info, nil
	}
	asset, ok := selectAsset(rel.Assets)
	info.LatestVersion = rel.TagName
	if !ok {
		info.Message = "release found but no compatible asset"
		u.record(info)
		return info, nil
	}
	info.Available = true
	info.AssetName = asset.Name
	info.DownloadURL = asset.BrowserDownloadURL
	info.Message = "update available"
	u.record(info)
	return info, nil
}

func (u *Updater) Apply(ctx context.Context, info *Info) error {
	if info == nil || !info.Available || info.DownloadURL == "" {
		return errors.New("no update available")
	}
	if err := os.MkdirAll(u.cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	u.logf("found %s on %s", info.LatestVersion, info.Channel)
	archivePath := filepath.Join(u.cacheDir, "update"+assetExt(info.AssetName))
	if err := u.download(ctx, info.DownloadURL, archivePath); err != nil {
		return err
	}
	if err := u.verifySHA256(ctx, info, archivePath); err != nil {
		return err
	}
	candidate, cleanup, err := u.prepareCandidate(archivePath)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(candidate, 0o755); err != nil {
		return fmt.Errorf("chmod candidate: %w", err)
	}
	if err := u.probe(ctx, candidate); err != nil {
		return fmt.Errorf("probe candidate: %w", err)
	}
	if err := u.swap(candidate); err != nil {
		return err
	}
	u.logf("updated to %s", info.LatestVersion)
	if u.restart {
		if err := kickstart(u.launchctlPath); err != nil {
			u.logf("kickstart failed: %v", err)
			return err
		}
	}
	return nil
}

func (u *Updater) CheckAndApply(ctx context.Context) (*Info, error) {
	info, err := u.Check(ctx)
	if err != nil || !info.Available {
		return info, err
	}
	return info, u.Apply(ctx, info)
}

func (u *Updater) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = u.CheckAndApply(ctx)
			}
		}
	}()
}

func (u *Updater) record(info *Info) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastCheck = info.CheckedAt
	u.updateAvailable = info.Available
	u.lastVersion = info.LatestVersion
}

func (u *Updater) fetchReleases(ctx context.Context) ([]release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases", u.apiBase, u.repo)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases status %d", resp.StatusCode)
	}
	var releases []release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return releases, nil
}

func (u *Updater) download(ctx context.Context, rawURL, dst string) error {
	tmp := dst + ".tmp"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	resp, err := u.hc.Do(req)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create download: %w", err)
	}
	_, copyErr := io.Copy(f, resp.Body)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("write download: %w", copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("fsync download: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close download: %w", closeErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("commit download: %w", err)
	}
	return fsyncDir(filepath.Dir(dst))
}

func (u *Updater) verifySHA256(ctx context.Context, info *Info, path string) error {
	shaURL := info.DownloadURL + ".sha256"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, shaURL, nil)
	resp, err := u.hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sha256 status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return fmt.Errorf("read sha256: %w", err)
	}
	want := strings.Fields(string(b))
	if len(want) == 0 {
		return fmt.Errorf("empty sha256")
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want[0], got) {
		return fmt.Errorf("sha256 mismatch")
	}
	return nil
}

func (u *Updater) prepareCandidate(archivePath string) (string, func(), error) {
	extractDir, err := os.MkdirTemp(u.cacheDir, "extract-*")
	if err != nil {
		return "", nil, fmt.Errorf("create extract dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(extractDir) }
	if strings.HasSuffix(archivePath, ".zip") {
		err = extractZip(archivePath, extractDir)
	} else {
		err = extractTarGz(archivePath, extractDir)
	}
	if err != nil {
		return "", cleanup, err
	}
	candidate, err := findBinary(extractDir)
	if err != nil {
		return "", cleanup, err
	}
	return candidate, cleanup, nil
}

func (u *Updater) probe(ctx context.Context, binary string) error {
	addr, err := freeLocalAddr()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, u.probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--probe-update", addr)
	cmd.Env = append(os.Environ(), "AMP_PROXY_NEO_NO_TRAY=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	url := "http://" + addr + "/api/status"
	deadline := time.Now().Add(u.probeTimeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := u.hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("candidate did not become healthy")
}

func (u *Updater) swap(candidate string) error {
	if err := os.MkdirAll(filepath.Dir(u.binaryPath), 0o755); err != nil {
		return fmt.Errorf("create binary dir: %w", err)
	}
	staged := u.binaryPath + ".new"
	backup := u.binaryPath + ".bak"
	_ = os.Remove(staged)
	if err := copyFile(candidate, staged, 0o755); err != nil {
		return fmt.Errorf("stage candidate: %w", err)
	}
	_ = os.Remove(backup)
	hadOld := false
	if _, err := os.Stat(u.binaryPath); err == nil {
		hadOld = true
		if err := os.Rename(u.binaryPath, backup); err != nil {
			_ = os.Remove(staged)
			return fmt.Errorf("backup current binary: %w", err)
		}
	}
	if err := os.Rename(staged, u.binaryPath); err != nil {
		if hadOld {
			_ = os.Rename(backup, u.binaryPath)
		}
		return fmt.Errorf("install new binary: %w", err)
	}
	if err := fsyncDir(filepath.Dir(u.binaryPath)); err != nil {
		return err
	}
	return nil
}

func (u *Updater) logf(format string, args ...any) {
	_ = os.MkdirAll(u.cacheDir, 0o755)
	f, err := os.OpenFile(filepath.Join(u.cacheDir, "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
}

type release struct {
	TagName    string  `json:"tag_name"`
	Prerelease bool    `json:"prerelease"`
	Draft      bool    `json:"draft"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func selectRelease(releases []release, channel, current string) (release, bool) {
	var candidates []release
	for _, r := range releases {
		if r.Draft || !channelAllows(channel, r.TagName, r.Prerelease) {
			continue
		}
		if CompareVersions(r.TagName, current) > 0 {
			candidates = append(candidates, r)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return CompareVersions(candidates[i].TagName, candidates[j].TagName) > 0 })
	if len(candidates) == 0 {
		return release{}, false
	}
	return candidates[0], true
}

func channelAllows(channel, tag string, prerelease bool) bool {
	pre := strings.Contains(strings.TrimPrefix(tag, "v"), "-")
	switch normalizeChannel(channel) {
	case "off":
		return false
	case "stable":
		return !pre && !prerelease
	case "beta":
		return !pre || betaTag(tag)
	case "dev":
		return true
	default:
		return !pre && !prerelease
	}
}

func selectAsset(assets []asset) (asset, bool) {
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, ".sha256") {
			continue
		}
		if !strings.Contains(name, "amp-proxy-neo") {
			continue
		}
		if runtime.GOOS == "darwin" && !(strings.Contains(name, "macos") || strings.Contains(name, "darwin")) {
			continue
		}
		if runtime.GOARCH == "arm64" && !strings.Contains(name, "arm64") {
			continue
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".zip") {
			return a, true
		}
	}
	return asset{}, false
}

func assetExt(name string) string {
	name = strings.ToLower(name)
	if strings.HasSuffix(name, ".zip") {
		return ".zip"
	}
	return ".tar.gz"
}

var betaRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+-beta\d+$`)

func betaTag(tag string) bool { return betaRe.MatchString(tag) }
func normalizeChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case "beta", "dev", "off":
		return strings.ToLower(strings.TrimSpace(ch))
	default:
		return "stable"
	}
}

func CompareVersions(a, b string) int {
	pa := parseVersion(a)
	pb := parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa.num[i] < pb.num[i] {
			return -1
		}
		if pa.num[i] > pb.num[i] {
			return 1
		}
	}
	if pa.pre == pb.pre {
		return 0
	}
	if pa.pre == "" {
		return -1
	}
	if pb.pre == "" {
		return 1
	}
	if pa.pre < pb.pre {
		return -1
	}
	return 1
}

type parsedVersion struct {
	num [3]int
	pre string
}

func parseVersion(v string) parsedVersion {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	base, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(base, ".")
	var out parsedVersion
	out.pre = pre
	for i := 0; i < len(parts) && i < 3; i++ {
		fmt.Sscanf(parts[i], "%d", &out.num[i])
	}
	return out
}

func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		clean := filepath.Clean(h.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		path := filepath.Join(dst, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}

func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		clean := filepath.Clean(zf.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path %q", zf.Name)
		}
		path := filepath.Join(dst, clean)
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, zf.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(f, rc)
		closeReadErr := rc.Close()
		closeWriteErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeReadErr != nil {
			return closeReadErr
		}
		if closeWriteErr != nil {
			return closeWriteErr
		}
	}
	return nil
}

func findBinary(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		if d.Name() == "amp-proxy-neo" {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("amp-proxy-neo binary not found in archive")
	}
	return found, nil
}

func freeLocalAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	return addr, ln.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func kickstart(launchctl string) error {
	uid := os.Getuid()
	return exec.Command(launchctl, "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, LaunchAgentLabel)).Run()
}
