package cost

import (
	"testing"

	"r2sync/internal/config"
	"r2sync/internal/state"
)

func TestStorageCapBlocksUpload(t *testing.T) {
	cfg := config.Defaults()
	cfg.StorageCapBytes = 100
	g := Guard{Config: cfg, State: state.Data{}}
	decision := g.CheckUpload(0, 101)
	if decision.Allowed {
		t.Fatal("expected upload to be blocked")
	}
}

func TestDeleteOpsAreTrackedAsFree(t *testing.T) {
	data := state.Data{}
	RegisterFree(&data, 1)
	if data.Counters.FreeOps != 1 || data.Counters.ClassA != 0 || data.Counters.ClassB != 0 {
		t.Fatalf("unexpected counters: %#v", data.Counters)
	}
}

func TestUploadClassAOpsAccountsForMultipart(t *testing.T) {
	if got := UploadClassAOps(1024); got != 1 {
		t.Fatalf("small upload ops = %d", got)
	}
	if got := UploadClassAOps(MultipartThresholdBytes + 1); got <= 2 {
		t.Fatalf("multipart upload ops too small: %d", got)
	}
}
