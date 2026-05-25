package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.3.0", "v0.4.0", -1},
		{"v0.4.0", "v0.4.0-beta1", -1},
		{"v0.4.0-beta1", "v0.4.1", -1},
		{"v0.4.1", "v0.4.1", 0},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCheckChannels(t *testing.T) {
	releases := []release{
		{TagName: "v0.4.0", Assets: []asset{{Name: assetName(), BrowserDownloadURL: "http://example/stable"}}},
		{TagName: "v0.4.0-beta1", Prerelease: true, Assets: []asset{{Name: assetName(), BrowserDownloadURL: "http://example/beta"}}},
		{TagName: "v9.9.9-test", Prerelease: true, Assets: []asset{{Name: assetName(), BrowserDownloadURL: "http://example/dev"}}},
	}
	if rel, ok := selectRelease(releases, "stable", "v0.3.0"); !ok || rel.TagName != "v0.4.0" {
		t.Fatalf("stable selected %q ok=%v", rel.TagName, ok)
	}
	if rel, ok := selectRelease(releases, "beta", "v0.3.0"); !ok || rel.TagName != "v0.4.0-beta1" {
		t.Fatalf("beta selected %q ok=%v", rel.TagName, ok)
	}
	if rel, ok := selectRelease(releases, "dev", "v0.3.0"); !ok || rel.TagName != "v9.9.9-test" {
		t.Fatalf("dev selected %q ok=%v", rel.TagName, ok)
	}
	if _, ok := selectRelease(releases, "off", "v0.3.0"); ok {
		t.Fatalf("off selected a release")
	}
}

func TestCheckAndApplyDownloadsProbesAndSwaps(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("asset selection is macOS arm64 specific")
	}
	tmp := t.TempDir()
	archive := filepath.Join(tmp, assetName())
	if err := writeUpdateArchive(archive, helperScript(t)); err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sha := sha256.Sum256(archiveBytes)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/fran0220/amp-proxy-neo/releases":
			fmt.Fprintf(w, `[{"tag_name":"v0.4.0","assets":[{"name":%q,"browser_download_url":%q}]}]`, assetName(), serverURL(r)+"/download/"+assetName())
		case "/download/" + assetName():
			http.ServeFile(w, r, archive)
		case "/download/" + assetName() + ".sha256":
			fmt.Fprintln(w, hex.EncodeToString(sha[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	binaryPath := filepath.Join(tmp, "Applications", "AmpProxyNeo.app", "Contents", "MacOS", "amp-proxy-neo")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(Options{Current: "v0.3.0", Channel: "stable", APIBase: server.URL, CacheDir: filepath.Join(tmp, "cache"), BinaryPath: binaryPath, ProbeTimeout: 3 * time.Second})
	info, err := u.CheckAndApply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Available || info.LatestVersion != "v0.4.0" {
		t.Fatalf("info = %+v", info)
	}
	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != helperScript(t) {
		t.Fatalf("binary was not swapped")
	}
	if _, err := os.Stat(binaryPath + ".bak"); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" && len(args) > i+2 && args[i+1] == "--probe-update" {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
			_ = http.ListenAndServe(args[i+2], mux)
		}
	}
	os.Exit(2)
}

func writeUpdateArchive(path, script string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	name := "AmpProxyNeo.app/Contents/MacOS/amp-proxy-neo"
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(script))}); err != nil {
		return err
	}
	_, err = tw.Write([]byte(script))
	return err
}

func helperScript(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("#!/bin/sh\nGO_WANT_HELPER_PROCESS=1 exec %q -test.run=TestHelperProcess -- \"$@\"\n", exe)
}

func assetName() string { return "amp-proxy-neo-macos-arm64.tar.gz" }

func serverURL(r *http.Request) string { return "http://" + r.Host }
