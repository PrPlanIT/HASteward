package common

import (
	"os"
	"strconv"
	"strings"
)

const EnvPrefix = "HASTEWARD_"

// Env reads HASTEWARD_<KEY> from the environment with a fallback default.
func Env(key, fallback string) string {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		return v
	}
	return fallback
}

// EnvBool reads HASTEWARD_<KEY> as a boolean.
// Accepts "1", "t", "true", "yes", "on" (case-insensitive).
func EnvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(EnvPrefix + key)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "yes", "on":
		return true
	case "0", "f", "false", "no", "off", "":
		return false
	}
	return fallback
}

// EnvInt reads HASTEWARD_<KEY> as an integer with a fallback default.
func EnvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(EnvPrefix + key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

// EnvRaw reads a raw environment variable (no prefix) with a fallback default.
// Used for standard env vars like KUBECONFIG that don't use the HASTEWARD_ prefix.
func EnvRaw(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
