//go:build windows

package windows_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/history"
	"github.com/omry/snoozap/internal/model"
)

func TestNativeOwnerDiagnosticsPreserveHoldAndExport(t *testing.T) {
	requireNative(t)
	service, err := nativeFixtureServiceFor(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	before, err := service.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Minute})
	if err != nil || !before.NativeRequestAccepted {
		t.Fatal("native manual hold failed", err)
	}
	data, err := service.ExportConfig(context.Background())
	if err != nil || !json.Valid(data) {
		t.Fatal("native config export failed", err)
	}
	h, err := service.History(context.Background())
	if err != nil || h.SchemaVersion != 1 {
		t.Fatal("native history unavailable", err)
	}
	mode, native := false, false
	for _, r := range h.Records {
		if r.Operation == "mode" && r.Result == "awake" {
			mode = true
		}
		if r.Operation == "native" && r.Result == "accepted" {
			native = true
		}
	}
	if !mode || !native {
		t.Fatal("native control/acceptance absent from history", h)
	}
	if err := service.ClearHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err = service.History(context.Background())
	if err != nil || len(h.Records) != 0 {
		t.Fatal("native history clear", err)
	}
	copy := history.Snapshot{SchemaVersion: h.SchemaVersion, Records: h.Records}
	if _, err := json.Marshal(copy); err != nil {
		t.Fatal(err)
	}
	after, err := service.Snapshot(context.Background())
	if err != nil || !after.NativeRequestAccepted || !after.Deadline.Equal(*before.Deadline) {
		t.Fatal("diagnostics disturbed native hold", err)
	}
}
