package util

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

type UpdateInfo struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	DownloadURL    string `json:"download_url"`
	AssetName      string `json:"asset_name"`
	Available      bool   `json:"available"`
}

type Updater struct {
	repo    string
	current string
}

func NewUpdater() *Updater {
	return &Updater{
		repo:    "fran0220/amp-proxy-neo",
		current: Version,
	}
}

func (u *Updater) Check() (*UpdateInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", u.repo)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release info: %w", err)
	}

	info := &UpdateInfo{
		CurrentVersion: u.current,
		LatestVersion:  release.TagName,
	}

	if compareVersions(release.TagName, u.current) <= 0 {
		return info, nil
	}

	// Find matching asset
	var wantAsset string
	if isBundleMode() {
		wantAsset = "amp-proxy-macos-arm64.app.zip"
	} else {
		wantAsset = "amp-proxy-macos-arm64.zip"
	}

	for _, asset := range release.Assets {
		if asset.Name == wantAsset {
			info.Available = true
			info.DownloadURL = asset.BrowserDownloadURL
			info.AssetName = asset.Name
			break
		}
	}

	if !info.Available {
		log.Warnf("[UPDATE] new version %s found but no matching asset %q", release.TagName, wantAsset)
	}

	return info, nil
}

func (u *Updater) Apply(info *UpdateInfo) error {
	if !info.Available || info.DownloadURL == "" {
		return fmt.Errorf("no update available")
	}

	// Download zip to temp file
	log.Infof("[UPDATE] downloading %s", info.AssetName)
	resp, err := http.Get(info.DownloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpZip, err := os.CreateTemp("", "amp-proxy-update-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpZip.Name())

	if _, err := io.Copy(tmpZip, resp.Body); err != nil {
		tmpZip.Close()
		return fmt.Errorf("failed to save download: %w", err)
	}
	tmpZip.Close()

	// Extract with ditto
	extractDir, err := os.MkdirTemp("", "amp-proxy-extract-")
	if err != nil {
		return fmt.Errorf("failed to create extract dir: %w", err)
	}

	log.Infof("[UPDATE] extracting to %s", extractDir)
	cmd := exec.Command("ditto", "-x", "-k", tmpZip.Name(), extractDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(extractDir)
		return fmt.Errorf("extraction failed: %w: %s", err, out)
	}

	// Determine source and destination
	var src, dst, mode string
	pid := os.Getpid()

	if isBundleMode() {
		mode = "bundle"
		dst = appBundlePath()
		// Find extracted .app
		entries, _ := os.ReadDir(extractDir)
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
				src = filepath.Join(extractDir, e.Name())
				break
			}
		}
		if src == "" {
			os.RemoveAll(extractDir)
			return fmt.Errorf("no .app found in archive")
		}
	} else {
		mode = "binary"
		exe, _ := os.Executable()
		dst = exe
		// Find extracted binary
		entries, _ := os.ReadDir(extractDir)
		for _, e := range entries {
			if !e.IsDir() {
				src = filepath.Join(extractDir, e.Name())
				break
			}
		}
		if src == "" {
			os.RemoveAll(extractDir)
			return fmt.Errorf("no binary found in archive")
		}
	}

	// Write update script
	script := fmt.Sprintf(`#!/bin/bash
# Wait for the current process to exit
while kill -0 %d 2>/dev/null; do sleep 0.5; done

MODE="%s"
SRC="%s"
DST="%s"
EXTRACT_DIR="%s"

if [ "$MODE" = "bundle" ]; then
    rm -rf "$DST"
    cp -R "$SRC" "$DST"
    xattr -dr com.apple.quarantine "$DST" 2>/dev/null
    open "$DST"
else
    cp -f "$SRC" "$DST"
    chmod +x "$DST"
    xattr -dr com.apple.quarantine "$DST" 2>/dev/null
    "$DST" &
fi

rm -rf "$EXTRACT_DIR"
rm -f /tmp/amp-proxy-update.sh
`, pid, mode, src, dst, extractDir)

	scriptPath := "/tmp/amp-proxy-update.sh"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		os.RemoveAll(extractDir)
		return fmt.Errorf("failed to write update script: %w", err)
	}

	// Launch detached
	log.Infof("[UPDATE] launching update script (pid=%d, mode=%s)", pid, mode)
	shellCmd := exec.Command("bash", scriptPath)
	shellCmd.Stdout = nil
	shellCmd.Stderr = nil
	shellCmd.Stdin = nil
	if err := shellCmd.Start(); err != nil {
		os.RemoveAll(extractDir)
		os.Remove(scriptPath)
		return fmt.Errorf("failed to launch update script: %w", err)
	}
	// Detach: don't wait for the script
	go func() { _ = shellCmd.Wait() }()

	return nil
}

// compareVersions compares two semver strings (with optional "v" prefix).
// Returns -1 if a < b, 0 if equal, 1 if a > b.
func compareVersions(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	partsA := strings.SplitN(a, ".", 3)
	partsB := strings.SplitN(b, ".", 3)

	for i := 0; i < 3; i++ {
		var va, vb int
		if i < len(partsA) {
			va, _ = strconv.Atoi(partsA[i])
		}
		if i < len(partsB) {
			vb, _ = strconv.Atoi(partsB[i])
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

// isBundleMode returns true if running inside a macOS .app bundle.
func isBundleMode() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(exe, ".app/Contents/MacOS/")
}

// appBundlePath returns the .app directory path when running in bundle mode.
func appBundlePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	idx := strings.Index(exe, ".app/Contents/MacOS/")
	if idx < 0 {
		return ""
	}
	return exe[:idx+4] // includes ".app"
}
