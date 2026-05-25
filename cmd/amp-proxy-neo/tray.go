package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/systray"
	log "github.com/sirupsen/logrus"
)

func setupTray(adminAddr, logPath string) {
	systray.Run(onTrayReady(adminAddr, logPath), onTrayExit)
}

func onTrayReady(adminAddr, logPath string) func() {
	return func() {
		systray.SetIcon(iconPurple)
		systray.SetTitle("AMP Proxy (Neo)")
		systray.SetTooltip("AMP Proxy Neo")

		mDashboard := systray.AddMenuItem("Open Dashboard", "Open AMP Proxy Neo dashboard")
		mChat := systray.AddMenuItem("Open Chat UI", "Open AMP Proxy Neo chat")
		mLogs := systray.AddMenuItem("Open Logs", "Open Neo proxy log in Console")
		mRefresh := systray.AddMenuItem("Refresh Tokens", "Ask token managers to refresh on next use")
		systray.AddSeparator()
		mStatus := systray.AddMenuItem("Status: Initializing", "")
		mStatus.Disable()
		mCheckUpdate := systray.AddMenuItem("Check for Updates", "Check GitHub releases for AMP Proxy Neo")
		mUpdateHistory := systray.AddMenuItem("Show Update History", "Open the updater log")
		mLoginItems := systray.AddMenuItem("Login Items...", "Open macOS Login Items settings")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Quit AMP Proxy Neo")
		stateCh := make(chan string, 1)

		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				status, detail := trayStatus(adminAddr)
				select {
				case state := <-stateCh:
					status, detail = state, ""
				default:
				}
				switch status {
				case "Ready":
					systray.SetIcon(iconPurple)
					mStatus.SetTitle("Status: Ready")
				case "Updating":
					systray.SetIcon(iconYellow)
					mStatus.SetTitle("Status: Updating")
				default:
					systray.SetIcon(iconYellow)
					if detail == "" {
						detail = "admin unhealthy"
					}
					mStatus.SetTitle("Status: Degraded (" + detail + ")")
				}
				<-ticker.C
			}
		}()

		go func() {
			for {
				select {
				case <-mDashboard.ClickedCh:
					if err := exec.Command("open", dashboardURL(adminAddr)).Start(); err != nil {
						log.Errorf("open dashboard failed: %v", err)
					}
				case <-mChat.ClickedCh:
					if err := exec.Command("open", dashboardURL(adminAddr)+"/chat").Start(); err != nil {
						log.Errorf("open chat failed: %v", err)
					}
				case <-mLogs.ClickedCh:
					if err := exec.Command("open", "-a", "Console", logPath).Start(); err != nil {
						log.Errorf("open logs failed: %v", err)
					}
				case <-mRefresh.ClickedCh:
					log.Info("refresh tokens requested from tray; token managers refresh lazily/background")
				case <-mCheckUpdate.ClickedCh:
					setTrayState(stateCh, "Updating")
					go func() {
						msg, err := triggerUpdateCheck(adminAddr)
						if err != nil {
							log.Errorf("update check failed: %v", err)
						} else {
							log.Infof("update check: %s", msg)
						}
					}()
				case <-mUpdateHistory.ClickedCh:
					if err := exec.Command("open", updateLogPath()).Start(); err != nil {
						log.Errorf("open update history failed: %v", err)
					}
				case <-mLoginItems.ClickedCh:
					if err := exec.Command("open", "x-apple.systempreferences:com.apple.LoginItems-Settings.extension").Start(); err != nil {
						log.Errorf("open Login Items failed: %v", err)
					}
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}
}

func onTrayExit() {
	log.Info("amp-proxy-neo shutting down")
}

func dashboardURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		return "http://" + net.JoinHostPort(host, port)
	}
	return "http://localhost:9320"
}

func adminHealthy(addr string) bool {
	status, _ := trayStatus(addr)
	return status == "Ready"
}

func trayStatus(addr string) (string, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(dashboardURL(addr) + "/api/status")
	if err != nil {
		return "Degraded", "admin unhealthy"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "Degraded", "admin status " + resp.Status
	}
	var status struct {
		Version         string `json:"version"`
		UpdateAvailable bool   `json:"update_available"`
		LatestVersion   string `json:"latest_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "Degraded", "bad status json"
	}
	if status.UpdateAvailable {
		return "Degraded", "update " + status.LatestVersion + " available"
	}
	return "Ready", ""
}

func triggerUpdateCheck(addr string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(dashboardURL(addr)+"/api/update/check", "application/json", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Available     bool   `json:"available"`
		LatestVersion string `json:"latest_version"`
		Message       string `json:"message"`
		Error         string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		if out.Error != "" {
			return "", &trayError{out.Error}
		}
		return "", &trayError{resp.Status}
	}
	if out.Available {
		return "found " + out.LatestVersion, nil
	}
	if out.Message != "" {
		return out.Message, nil
	}
	return "no update", nil
}

func updateLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "amp-proxy-neo-update.log")
	}
	return filepath.Join(home, "Library", "Caches", "amp-proxy-neo", "update.log")
}

func setTrayState(ch chan string, state string) {
	select {
	case ch <- state:
	default:
	}
}

type trayError struct{ msg string }

func (e *trayError) Error() string { return e.msg }
