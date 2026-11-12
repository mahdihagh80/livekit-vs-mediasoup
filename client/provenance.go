package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"sfubench/loadgen/internal/client"
)

// sourceHash is set by build.sh (scripts/source-hash.sh).
var sourceHash = "unknown"

var buildSettingKeys = map[string]bool{"GOARCH": true, "GOAMD64": true, "CGO_ENABLED": true, "-ldflags": true, "-tags": true}

// loadgenInfo is run.json's "loadgen": what binary ran, built how, on which CPUs.
func loadgenInfo() map[string]any {
	host, _ := os.Hostname()
	info := map[string]any{
		"binary_sha256":  binarySHA256(),
		"source_sha256":  sourceHash,
		"go_version":     runtime.Version(),
		"nack_generator": client.NackGeneratorID,
		"srtp_profiles":  client.SRTPProfiles(),
		"host":           host,
		"cpus_allowed":   cpusAllowed(),
		"gomaxprocs":     runtime.GOMAXPROCS(0),
		"num_cpu":        runtime.NumCPU(),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info["go_version"] = bi.GoVersion
		settings := map[string]string{}
		for _, s := range bi.Settings {
			if s.Value != "" && (buildSettingKeys[s.Key] || strings.HasPrefix(s.Key, "vcs.")) {
				settings[s.Key] = s.Value
			}
		}
		info["build_settings"] = settings
	}
	return info
}

func binarySHA256() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	f, err := os.Open(exe)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cpusAllowed is the CPU list this process may run on (taskset), from
// /proc/self/status.
func cpusAllowed() string {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Cpus_allowed_list:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}
