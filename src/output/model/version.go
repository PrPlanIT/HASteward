package model

// VersionInfo is the build metadata printed by `hasteward version`.
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}
