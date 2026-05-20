package version

import "time"

const Version = "6.5.19"
const ReleasesURL = "https://github.com/macgaver/zfsnas-chezmoi/releases"

var experimentalMode bool

// StartedAt is the wall-clock time the binary was loaded. Captured at
// package init so the same value is returned for the lifetime of the
// process. Used by the frontend to detect server restarts/upgrades — a
// changed StartedAt across two polls means the server bounced, and the
// portal shows the "Server Restarted" refresh popup.
var StartedAt = time.Now()

func SetExperimental(v bool) { experimentalMode = v }
func IsExperimental() bool   { return experimentalMode }
