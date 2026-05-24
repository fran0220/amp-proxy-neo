package main

import (
	"net"
	"os/exec"
	"strings"

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
		mLogs := systray.AddMenuItem("Show Logs", "Open Neo proxy log in Console")
		systray.AddSeparator()
		mStatus := systray.AddMenuItem("Status: Running", "")
		mStatus.Disable()
		mQuit := systray.AddMenuItem("Quit", "Quit AMP Proxy Neo")

		go func() {
			for {
				select {
				case <-mDashboard.ClickedCh:
					if err := exec.Command("open", dashboardURL(adminAddr)).Start(); err != nil {
						log.Errorf("open dashboard failed: %v", err)
					}
				case <-mLogs.ClickedCh:
					if err := exec.Command("open", "-a", "Console", logPath).Start(); err != nil {
						log.Errorf("open logs failed: %v", err)
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
