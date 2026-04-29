package version

const Version = "6.4.28"
const ReleasesURL = "https://github.com/macgaver/zfsnas-chezmoi/releases"

var experimentalMode bool

func SetExperimental(v bool) { experimentalMode = v }
func IsExperimental() bool   { return experimentalMode }
