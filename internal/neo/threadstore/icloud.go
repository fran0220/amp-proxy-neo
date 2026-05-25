package threadstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const ICloudRelativeDir = "Library/Mobile Documents/com~apple~CloudDocs/AmpProxyNeo"

type ICloudSyncStore struct {
	base        Store
	dir         string
	conflictLog string
	mu          sync.Mutex
}

type ICloudStatus struct {
	Enabled bool
	Dir     string
	Count   int
}

func DetectICloudDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	dir := filepath.Join(home, ICloudRelativeDir)
	st, err := os.Stat(dir)
	return dir, err == nil && st.IsDir()
}

func NewICloudSyncStore(base Store, dir, conflictLog string) *ICloudSyncStore {
	return &ICloudSyncStore{base: base, dir: dir, conflictLog: conflictLog}
}

func (s *ICloudSyncStore) UploadThread(ctx context.Context, thread *Thread) error {
	if err := s.base.UploadThread(ctx, thread); err != nil {
		return err
	}
	go func() { _ = s.WriteThread(context.Background(), thread) }()
	return nil
}

func (s *ICloudSyncStore) GetThread(ctx context.Context, id string) (*Thread, error) {
	return s.base.GetThread(ctx, id)
}

func (s *ICloudSyncStore) ListThreads(ctx context.Context, opts ListOptions) ([]*ThreadSummary, error) {
	return s.base.ListThreads(ctx, opts)
}

func (s *ICloudSyncStore) DeleteThread(ctx context.Context, id string) error {
	if err := s.base.DeleteThread(ctx, id); err != nil {
		return err
	}
	_ = os.Remove(s.threadPath(id))
	return nil
}

func (s *ICloudSyncStore) Close() error { return s.base.Close() }

func (s *ICloudSyncStore) ImportExisting(ctx context.Context) (ICloudStatus, error) {
	status := ICloudStatus{Enabled: true, Dir: s.dir}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return status, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		cloud, err := readThreadFile(path)
		if err != nil {
			s.logConflict("read %s failed: %v", path, err)
			continue
		}
		info, _ := entry.Info()
		if changed, err := s.mergeCloudThread(ctx, cloud, info.ModTime()); err != nil {
			s.logConflict("merge thread=%s failed: %v", cloud.ID, err)
		} else if changed {
			status.Count++
		}
	}
	return status, nil
}

func (s *ICloudSyncStore) WriteThread(ctx context.Context, thread *Thread) error {
	if thread == nil {
		return errors.New("nil thread")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.threadPath(thread.ID)
	cloud, err := readThreadFile(path)
	if err == nil && cloud.ID == thread.ID {
		if cloud.V > thread.V {
			s.logConflict("thread=%s local_v=%d cloud_v=%d decision=keep-cloud", thread.ID, thread.V, cloud.V)
			return nil
		}
		if cloud.V != thread.V {
			s.logConflict("thread=%s local_v=%d cloud_v=%d decision=write-local", thread.ID, thread.V, cloud.V)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := thread.rawJSON()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	out := append([]byte(nil), raw...)
	out = append(out, '\n')
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *ICloudSyncStore) mergeCloudThread(ctx context.Context, cloud *Thread, cloudMTime time.Time) (bool, error) {
	local, err := s.base.GetThread(ctx, cloud.ID)
	if errors.Is(err, ErrNotFound) {
		ok, err := ImportThread(s.base, cloud)
		return ok, err
	}
	if err != nil {
		return false, err
	}
	if cloud.V > local.V {
		s.logConflict("thread=%s local_v=%d cloud_v=%d decision=import-cloud", cloud.ID, local.V, cloud.V)
		_ = s.base.DeleteThread(ctx, cloud.ID)
		ok, err := ImportThread(s.base, cloud)
		return ok, err
	}
	if cloud.V < local.V {
		s.logConflict("thread=%s local_v=%d cloud_v=%d decision=keep-local", cloud.ID, local.V, cloud.V)
		return false, s.WriteThread(ctx, local)
	}
	localMTime := millisToTime(local.UpdatedAt)
	if cloudMTime.After(localMTime) {
		s.logConflict("thread=%s local_v=%d cloud_v=%d decision=import-cloud-newer-mtime", cloud.ID, local.V, cloud.V)
		_ = s.base.DeleteThread(ctx, cloud.ID)
		ok, err := ImportThread(s.base, cloud)
		return ok, err
	}
	return false, s.WriteThread(ctx, local)
}

func (s *ICloudSyncStore) threadPath(id string) string {
	return filepath.Join(s.dir, safeThreadFilename(id)+".json")
}

func (s *ICloudSyncStore) logConflict(format string, args ...any) {
	if s.conflictLog == "" {
		return
	}
	line := time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n"
	_ = os.MkdirAll(filepath.Dir(s.conflictLog), 0o755)
	f, err := os.OpenFile(s.conflictLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

func readThreadFile(path string) (*Thread, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseThread(b)
}

func ReadConflictLog(path string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	sort.Sort(sort.Reverse(sort.StringSlice(lines)))
	return lines, nil
}

func millisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
