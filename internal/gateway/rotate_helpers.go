package gateway

import (
	"encoding/base64"
	"os"
	"path/filepath"

	"opencode2api/internal/config"
)

// Small helpers the rotation controller needs. They live here rather than in
// rotate.go so that file reads as one policy, and so an upstream merge that
// touches the controller does not have to also reconcile imports.

// configRedactURL is config.RedactURL under a shorter name, used in the
// snapshot so an agent URL with embedded credentials never reaches the admin
// API.
func configRedactURL(raw string) string {
	return config.RedactURL(raw)
}

// configParseProxyEntry is config.ParseProxyEntry under a shorter name. The
// pool marker (#pool / pool+) is irrelevant to rotation, but parsing through
// the same function keeps both readers agreeing on what the exit's URL is.
func configParseProxyEntry(raw string) (string, bool) {
	return config.ParseProxyEntry(raw)
}

// base64Encode is stdlib base64 for the Basic credential the agent expects.
func base64Encode(raw string) string {
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory and a rename, so a crash mid-write cannot leave a half-written
// snapshot that the next start would refuse to parse.
func writeFileAtomic(path string, data []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return
	}
	if err := temp.Close(); err != nil {
		return
	}
	_ = os.Rename(name, path)
}

// readFileIfExists reads path, reporting a missing file as an empty result
// rather than as an error: no snapshot is the normal first-run state.
func readFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
