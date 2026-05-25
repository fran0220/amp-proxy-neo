package util

var (
	BuildVersion = "dev"
	BuildCommit  = "unknown"
	BuildDate    = "unknown"

	// Version is kept for the legacy adminbase updater/status handlers.
	Version = BuildVersion
)
