package cost

import (
	"fmt"
	"r2sync/internal/config"
	"r2sync/internal/state"
)

const (
	StandardFreeStorageBytes = int64(10 * 1024 * 1024 * 1024)
	StandardFreeClassA       = int64(1_000_000)
	StandardFreeClassB       = int64(10_000_000)
	MultipartThresholdBytes  = int64(4 * 1024 * 1024 * 1024)
	MultipartPartSizeBytes   = int64(64 * 1024 * 1024)
)

type Guard struct {
	Config config.Config
	State  state.Data
}

type Decision struct {
	Allowed  bool     `json:"allowed"`
	Warnings []string `json:"warnings,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

func (g Guard) CheckUpload(oldSize, newSize int64) Decision {
	if g.Config.DisableCostGuards {
		return Decision{Allowed: true}
	}
	var warnings []string
	current := CurrentRemoteBytes(g.State)
	projected := current - oldSize + newSize
	if projected < 0 {
		projected = newSize
	}
	if projected > g.Config.StorageCapBytes {
		return Decision{
			Allowed: false,
			Reason:  fmt.Sprintf("storage cap exceeded: projected %d bytes > cap %d bytes", projected, g.Config.StorageCapBytes),
		}
	}
	if projected > int64(float64(g.Config.StorageCapBytes)*0.80) {
		warnings = append(warnings, "storage is above 80% of configured cap")
	}
	ops := UploadClassAOps(newSize)
	if reason := checkRequestLimit("Class A", g.State.Counters.ClassA+ops, StandardFreeClassA, g.Config.ClassAWarnRatio, g.Config.ClassABlockRatio); reason.block != "" {
		return Decision{Allowed: false, Reason: reason.block}
	} else if reason.warn != "" {
		warnings = append(warnings, reason.warn)
	}
	return Decision{Allowed: true, Warnings: warnings}
}

func UploadClassAOps(size int64) int64 {
	if size < MultipartThresholdBytes {
		return 1
	}
	parts := size / MultipartPartSizeBytes
	if size%MultipartPartSizeBytes != 0 {
		parts++
	}
	return parts + 2
}

func (g Guard) CheckClassB(nextOps int64) Decision {
	if g.Config.DisableCostGuards {
		return Decision{Allowed: true}
	}
	if nextOps < 1 {
		nextOps = 1
	}
	if reason := checkRequestLimit("Class B", g.State.Counters.ClassB+nextOps, StandardFreeClassB, g.Config.ClassBWarnRatio, g.Config.ClassBBlockRatio); reason.block != "" {
		return Decision{Allowed: false, Reason: reason.block}
	} else if reason.warn != "" {
		return Decision{Allowed: true, Warnings: []string{reason.warn}}
	}
	return Decision{Allowed: true}
}

func CurrentRemoteBytes(data state.Data) int64 {
	var total int64
	for _, rec := range data.Targets {
		if rec.Remote.Exists {
			total += rec.Remote.Size
		}
	}
	return total
}

func RegisterClassA(d *state.Data, n int64) {
	if n <= 0 {
		n = 1
	}
	d.Counters.ClassA += n
}

func RegisterClassB(d *state.Data, n int64) {
	if n <= 0 {
		n = 1
	}
	d.Counters.ClassB += n
}

func RegisterFree(d *state.Data, n int64) {
	if n <= 0 {
		n = 1
	}
	d.Counters.FreeOps += n
}

func RegisterUploaded(d *state.Data, bytes int64) {
	if bytes > 0 {
		d.Counters.UploadedBytes += bytes
	}
}

func RegisterDownloaded(d *state.Data, bytes int64) {
	if bytes > 0 {
		d.Counters.DownloadedBytes += bytes
	}
}

type requestReason struct {
	warn  string
	block string
}

func checkRequestLimit(name string, current, limit int64, warnRatio, blockRatio float64) requestReason {
	if limit <= 0 {
		return requestReason{}
	}
	blockAt := int64(float64(limit) * blockRatio)
	warnAt := int64(float64(limit) * warnRatio)
	if current >= blockAt {
		return requestReason{block: fmt.Sprintf("%s request guard exceeded: %d >= %d", name, current, blockAt)}
	}
	if current >= warnAt {
		return requestReason{warn: fmt.Sprintf("%s requests are above %.0f%% of free-tier estimate", name, warnRatio*100)}
	}
	return requestReason{}
}
