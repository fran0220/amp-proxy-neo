package remoteagent

import "github.com/fran0220/amp-proxy-neo/pkg/identity"

func truncateStr(s string, n int) string { return identity.TruncateStr(s, n) }

// TODO(PR4): wire these hooks to the Neo Rivet gateway via explicit
// constructor interfaces. They are intentionally local no-ops while the
// legacy and Neo packages are being isolated and must not import each other.
func RegisterThreadTap(threadID string, cb func(direction string, frame map[string]any)) func() {
	return func() {}
}

func EmitFrameTap(threadID, direction string, frame map[string]any) {}
