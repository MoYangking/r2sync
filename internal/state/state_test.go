package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatusMarshalOmitsZeroTimes(t *testing.T) {
	data, err := json.Marshal(Status{Stage: "complete", Progress: 100, Ready: true})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "0001-01-01") {
		t.Fatalf("zero time leaked into JSON: %s", text)
	}
	if strings.Contains(text, "next_scheduled_at") {
		t.Fatalf("zero next_scheduled_at was not omitted: %s", text)
	}
}

func TestStatusMarshalIncludesNonZeroNextScheduledAt(t *testing.T) {
	next := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	data, err := json.Marshal(Status{
		Stage:           "complete",
		Progress:        100,
		Ready:           true,
		NextScheduledAt: next,
	})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if !strings.Contains(string(data), "next_scheduled_at") {
		t.Fatalf("non-zero next_scheduled_at was omitted: %s", data)
	}
}
