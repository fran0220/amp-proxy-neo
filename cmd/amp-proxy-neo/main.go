package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/rivet"
	"github.com/fran0220/amp-proxy-neo/internal/neo/selfserve"
	"github.com/fran0220/amp-proxy-neo/internal/neo/threadstore"
	neoupdater "github.com/fran0220/amp-proxy-neo/internal/neo/updater"
	"github.com/fran0220/amp-proxy-neo/pkg/adminbase"
	"github.com/fran0220/amp-proxy-neo/pkg/auth"
	"github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/fran0220/amp-proxy-neo/pkg/identity"
	"github.com/fran0220/amp-proxy-neo/pkg/logger"
	"github.com/fran0220/amp-proxy-neo/pkg/token"
	"github.com/fran0220/amp-proxy-neo/pkg/util"
	log "github.com/sirupsen/logrus"
)

const appName = "amp-proxy-neo"

var appVersion = util.BuildVersion

type appState struct {
	started           time.Time
	dir, logPath      string
	listen, admin     string
	threadsDB, logsDB string
	cfg               *config.Config
	threads           threadstore.Store
	resolver          *auth.AuthResolver
	reqLog            *logger.RequestLogger
	gw                *rivet.RivetGateway
	upstream          *upstreamProxy
	updater           *neoupdater.Updater
	chat              fs.FS
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--probe-update" {
		runUpdateProbe(os.Args[2])
		return
	}
	if len(os.Args) > 1 {
		subcmd := os.Args[1]
		if subcmd == "help" || subcmd == "-h" || subcmd == "--help" {
			printCLIHelp()
			return
		}
		if subcmd != "serve" {
			if err := runCLI(subcmd, os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	dir := mustNeoDir()
	installExampleConfigs(dir)
	log.SetOutput(mustLog(filepath.Join(dir, "proxy.log")))
	cfg := loadConfig(dir)
	cfg.Listen = env("AMP_PROXY_NEO_LISTEN", cfg.Listen)
	adminAddr := env("AMP_PROXY_NEO_ADMIN", ":9320")

	claude := token.NewClaudeProfileManager(cfg)
	codex := token.NewCodexTokenManager()
	gemini := token.NewGeminiTokenManager()
	resolver := auth.NewAuthResolver(cfg, claude, codex, gemini)
	ensureNeoLogDB(dir)
	reqLog := logger.NewRequestLoggerInDir(dir)
	defer reqLog.Close()

	threadsDB := filepath.Join(dir, "threads.db")
	baseThreads, err := threadstore.OpenSQLite(threadsDB)
	if err != nil {
		log.Fatalf("open threadstore: %v", err)
	}
	var threads threadstore.Store = baseThreads
	if cfg.Neo.Storage.ICloudSync {
		if cloudDir, ok := threadstore.DetectICloudDir(); ok {
			syncStore := threadstore.NewICloudSyncStore(baseThreads, cloudDir, filepath.Join(dir, "sync-conflicts.log"))
			status, err := syncStore.ImportExisting(context.Background())
			if err != nil {
				log.Warnf("iCloud sync disabled after import error: %v", err)
			} else {
				threads = syncStore
				log.Infof("iCloud sync enabled: %d threads available across devices", status.Count)
				fmt.Printf("iCloud sync enabled: %d threads available across devices\n", status.Count)
			}
		} else {
			log.Warn("iCloud sync requested but iCloud Drive directory was not found")
		}
	}
	defer threads.Close()

	var upstream *upstreamProxy
	if cfg.Amp.UpstreamURL != "" {
		upstream, err = newUpstreamProxy(cfg.Amp.UpstreamURL, cfg.Amp.APIKey)
		if err != nil {
			log.Warnf("amp upstream disabled: %v", err)
		}
	}
	chatFS, _ := fs.Sub(chatUIFiles, "webui-react/dist")
	updater := neoupdater.New(neoupdater.Options{Current: util.BuildVersion, Channel: cfg.Neo.Update.Channel, Restart: true})
	s := &appState{started: time.Now(), dir: dir, logPath: filepath.Join(dir, "proxy.log"),
		listen: cfg.Listen, admin: adminAddr, threadsDB: threadsDB, logsDB: filepath.Join(dir, "amp-proxy-neo.db"),
		cfg: cfg, threads: threads, resolver: resolver, reqLog: reqLog, gw: rivet.New(cfg, resolver, reqLog, threads, cfg.SelfServe()), upstream: upstream, updater: updater, chat: chatFS}
	if cfg.SelfServe() {
		if _, err := selfserve.LoadOrCreateSecret(dir); err != nil {
			log.Fatalf("self-serve jwt secret: %v", err)
		}
		id, err := selfserve.LoadOrCreateUserID(dir)
		if err != nil {
			log.Fatalf("self-serve user id: %v", err)
		}
		cfg.Neo.UserID = id
		s.upstream = nil
		log.Warn("SELF-SERVE MODE — no ampcode.com required")
	} else {
		log.Warn("UPSTREAM MODE — ampcode.com fallback enabled")
	}

	admin := adminbase.NewAdminServer(cfg, claude, reqLog, resolver, chatUIFiles)
	go start("proxy", cfg.Listen, s.proxyMux())
	go start("admin", adminAddr, s.adminMux(admin))
	go flushLoop(reqLog)
	updater.Start(context.Background(), time.Hour)
	log.Infof("%s ready: proxy=%s admin=%s config=%s threads_db=%s logs_db=%s", appName, cfg.Listen, adminAddr, filepath.Join(dir, "config.yaml"), s.threadsDB, s.logsDB)
	fmt.Printf("AMP Proxy Neo ready\n  proxy: %s\n  admin: %s\n  config: %s\n  threads DB: %s\n  logs DB: %s\n", cfg.Listen, adminAddr, filepath.Join(dir, "config.yaml"), s.threadsDB, s.logsDB)
	if os.Getenv("AMP_PROXY_NEO_NO_TRAY") != "1" {
		setupTray(adminAddr, s.logPath)
	}
}

func installExampleConfigs(dir string) {
	out := filepath.Join(dir, "examples")
	_ = os.MkdirAll(out, 0o755)
	entries, err := exampleConfigFiles.ReadDir("examples")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		dst := filepath.Join(out, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		b, err := exampleConfigFiles.ReadFile("examples/" + e.Name())
		if err == nil {
			_ = os.WriteFile(dst, b, 0o644)
		}
	}
}

func runUpdateProbe(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "app": appName, "version": util.BuildVersion})
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func loadConfig(dir string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Listen = ":9319"
	cfg.Amp.UpstreamURL = "https://ampcode.com"
	cfg.UserID = ensureUserID(filepath.Join(dir, "user-id"))
	return config.LoadConfigFromDirWithDefault(dir, cfg)
}

func ensureUserID(path string) string {
	if id := strings.TrimSpace(os.Getenv("AMP_PROXY_NEO_USER_ID")); id != "" {
		_ = os.WriteFile(path, []byte(id+"\n"), 0600)
		return id
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	id := identity.GenerateClaudeUserID()
	_ = os.WriteFile(path, []byte(id+"\n"), 0600)
	return id
}

func runCLI(subcmd string, args []string) error {
	dir := mustNeoDir()
	switch subcmd {
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		out := fs.String("o", "", "output file (default stdout)")
		format := fs.String("format", "json", "json or ndjson")
		_ = fs.Parse(normalizeFlagArgs(args, nil))
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: %s export <thread-id> [-o file] [-format json|ndjson]", appName)
		}
		return cliExport(dir, fs.Arg(0), *out, *format)
	case "export-all":
		fs := flag.NewFlagSet("export-all", flag.ExitOnError)
		out := fs.String("o", "", "output tar.gz file (default stdout)")
		_ = fs.Parse(normalizeFlagArgs(args, nil))
		return cliExportAll(dir, *out)
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		format := fs.String("format", "auto", "auto, json, ndjson, or tar.gz")
		_ = fs.Parse(normalizeFlagArgs(args, nil))
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: %s import <file> [-format auto|json|ndjson|tar.gz]", appName)
		}
		return cliImport(dir, fs.Arg(0), *format)
	case "list-threads":
		fs := flag.NewFlagSet("list-threads", flag.ExitOnError)
		limit := fs.Int("limit", 50, "maximum threads")
		format := fs.String("format", "table", "table or json")
		_ = fs.Parse(normalizeFlagArgs(args, nil))
		return cliListThreads(dir, *limit, *format)
	case "delete-thread":
		fs := flag.NewFlagSet("delete-thread", flag.ExitOnError)
		_ = fs.Parse(normalizeFlagArgs(args, nil))
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: %s delete-thread <id>", appName)
		}
		return withThreadStore(dir, func(store threadstore.Store) error {
			if err := store.DeleteThread(context.Background(), fs.Arg(0)); err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", fs.Arg(0))
			return nil
		})
	case "db-info":
		return cliDBInfo(dir)
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ExitOnError)
		outDir := fs.String("o", "", "output directory")
		_ = fs.Parse(normalizeFlagArgs(args, nil))
		return cliBackup(dir, *outDir)
	case "restore":
		fs := flag.NewFlagSet("restore", flag.ExitOnError)
		yes := fs.Bool("yes", false, "overwrite without prompting")
		_ = fs.Parse(normalizeFlagArgs(args, map[string]bool{"yes": true}))
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: %s restore <backup.tar.gz> [-yes]", appName)
		}
		return cliRestore(dir, fs.Arg(0), *yes)
	default:
		printCLIHelp()
		return fmt.Errorf("unknown subcommand %q", subcmd)
	}
}

func normalizeFlagArgs(args []string, boolFlags map[string]bool) []string {
	if len(args) == 0 {
		return args
	}
	var flags []string
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue
		}
		if boolFlags[name] {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

func printCLIHelp() {
	fmt.Printf(`AMP Proxy Neo %s

Usage:
  %s [serve]
  %s export <thread-id> [-o file] [-format json|ndjson]
  %s export-all [-o file]
  %s import <file> [-format auto|json|ndjson|tar.gz]
  %s list-threads [-limit N] [-format json|table]
  %s delete-thread <id>
  %s db-info
  %s backup [-o dir]
  %s restore <backup.tar.gz> [-yes]

No arguments keeps the current behavior and starts the proxy/admin servers.
`, appVersion, appName, appName, appName, appName, appName, appName, appName, appName, appName)
}

func withThreadStore(dir string, fn func(threadstore.Store) error) error {
	store, err := threadstore.OpenSQLite(filepath.Join(dir, "threads.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(store)
}

func cliExport(dir, id, outPath, format string) error {
	return withThreadStore(dir, func(store threadstore.Store) error {
		var w io.Writer = os.Stdout
		var f *os.File
		var err error
		if outPath != "" {
			f, err = os.Create(outPath)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		switch strings.ToLower(format) {
		case "json", "thread-json", "":
			data, err := threadstore.ExportThread(store, id)
			if err != nil {
				return err
			}
			_, err = w.Write(data)
			if err == nil && outPath != "" {
				fmt.Printf("exported %s to %s\n", id, outPath)
			}
			return err
		case "ndjson", "jsonl":
			err := threadstore.ExportThreadMessagesNDJSON(store, id, w)
			if err == nil && outPath != "" {
				fmt.Printf("exported %s messages to %s\n", id, outPath)
			}
			return err
		default:
			return fmt.Errorf("unknown export format %q", format)
		}
	})
}

func cliExportAll(dir, outPath string) error {
	return withThreadStore(dir, func(store threadstore.Store) error {
		var w io.Writer = os.Stdout
		var f *os.File
		var err error
		if outPath != "" {
			f, err = os.Create(outPath)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		if err := threadstore.ExportAll(store, w); err != nil {
			return err
		}
		if outPath != "" {
			fmt.Printf("exported all threads to %s\n", outPath)
		}
		return nil
	})
}

func cliImport(dir, path, format string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return withThreadStore(dir, func(store threadstore.Store) error {
		imported, err := threadstore.ImportFromReader(store, f, format)
		if err != nil {
			return err
		}
		fmt.Printf("imported %d thread(s)\n", imported)
		return nil
	})
}

func cliListThreads(dir string, limit int, format string) error {
	return withThreadStore(dir, func(store threadstore.Store) error {
		threads, err := store.ListThreads(context.Background(), threadstore.ListOptions{Limit: limit})
		if err != nil {
			return err
		}
		if strings.EqualFold(format, "json") {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"threads": threads})
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTITLE\tMODE\tMESSAGES\tUPDATED")
		for _, t := range threads {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", t.ID, t.Title, t.AgentMode, t.MessageCount, formatMillis(t.UpdatedAt))
		}
		return tw.Flush()
	})
}

func cliDBInfo(dir string) error {
	path := filepath.Join(dir, "threads.db")
	var size int64
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	return withThreadStore(dir, func(store threadstore.Store) error {
		threads, err := store.ListThreads(context.Background(), threadstore.ListOptions{Limit: int(^uint(0) >> 1)})
		if err != nil {
			return err
		}
		var last int64
		for _, t := range threads {
			if t.UpdatedAt > last {
				last = t.UpdatedAt
			}
		}
		fmt.Printf("DB path: %s\nSize: %d bytes\nThreads: %d\nLast updated: %s\n", path, size, len(threads), formatMillis(last))
		return nil
	})
}

func cliBackup(dir, outDir string) error {
	if outDir == "" {
		outDir = dir
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(outDir, "amp-proxy-neo-backup-"+time.Now().Format("20060102-150405")+".tar.gz")
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, name := range []string{"threads.db", "config.yaml", "user-id", "jwt.key"} {
		path := filepath.Join(dir, name)
		if err := addBackupFile(tw, dir, path, name); err != nil {
			return err
		}
	}
	fmt.Printf("backup written to %s\n", outPath)
	return nil
}

func cliRestore(dir, path string, yes bool) error {
	if !yes {
		fmt.Printf("Restore will overwrite files in %s. Type 'restore' to continue: ", dir)
		var answer string
		_, _ = fmt.Scanln(&answer)
		if answer != "restore" {
			return fmt.Errorf("restore cancelled")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	allowed := map[string]bool{"threads.db": true, "config.yaml": true, "user-id": true, "jwt.key": true}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if !allowed[name] || hdr.Typeflag != tar.TypeReg {
			continue
		}
		mode := os.FileMode(0o644)
		if name == "user-id" || name == "jwt.key" {
			mode = 0o600
		}
		outPath := filepath.Join(dir, name)
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		_ = os.Chmod(outPath, mode)
	}
	fmt.Printf("restored backup into %s\n", dir)
	return nil
}

func addBackupFile(tw *tar.Writer, baseDir, path, name string) error {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	mode := int64(0o644)
	if name == "user-id" || name == "jwt.key" {
		mode = 0o600
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}
