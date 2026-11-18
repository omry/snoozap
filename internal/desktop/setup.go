//go:build windows

package desktop

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/history"
	"github.com/omry/snoozap/internal/model"
)

// SetupDocument carries HTTP scalar literals separately from the JSON document.
// The editor must not round a decimal through JavaScript's Number type. These
// are boundary projections of the same configuration, not another stored format.
type SetupDocument struct {
	Configuration         string            `json:"configuration"`
	Comparisons           map[string]string `json:"comparisons"`
	ConfigurationRevision uint64            `json:"configuration_revision"`
}

func (b *StatusBridge) SetupRead() (SetupDocument, error) {
	ctx, cancel := context.WithTimeout(b.controller.ctx, 5*time.Second)
	defer cancel()
	snapshot, err := b.controller.service.Snapshot(ctx)
	if err != nil {
		return SetupDocument{}, errors.New("Configuration unavailable")
	}
	data, err := b.controller.service.ExportConfig(ctx)
	if err != nil {
		return SetupDocument{}, errors.New("Configuration unavailable")
	}
	cfg, err := config.Parse(bytes.NewReader(data))
	if err != nil {
		return SetupDocument{}, errors.New("Configuration unavailable")
	}
	values := make(map[string]string)
	for id, probe := range cfg.Probes {
		if probe.Type == "http" {
			values[id] = string(probe.Value)
		}
	}
	return SetupDocument{Configuration: string(data), Comparisons: values, ConfigurationRevision: snapshot.ConfigurationRevision}, nil
}

func (b *StatusBridge) SetupApply(candidate string, expectedRevision uint64) (StatusView, error) {
	// Parse the original full candidate: do not drop unknown keys, duplicate keys,
	// malformed Unicode or invalid scalar literals by first decoding a loose DTO.
	cfg, err := config.Parse(bytes.NewBufferString(candidate))
	if err != nil {
		return b.Read(), errors.New("Invalid configuration; prior settings remain active")
	}
	snapshot, err := b.controller.ExecuteSnapshot(model.Command{Operation: "config", Configuration: &cfg, ConfigurationRevision: &expectedRevision})
	return b.viewForSnapshot(snapshot), err
}

func (b *StatusBridge) HistoryRead() (history.Snapshot, error) {
	ctx, cancel := context.WithTimeout(b.controller.ctx, 5*time.Second)
	defer cancel()
	value, err := b.controller.service.History(ctx)
	if err != nil {
		return history.Snapshot{}, errors.New("History unavailable")
	}
	return value, nil
}

func (b *StatusBridge) HistoryClear() (history.Snapshot, error) {
	c := b.controller
	if !c.command.TryLock() {
		return history.Snapshot{}, model.ErrUnavailable
	}
	defer c.command.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if err := c.service.ClearHistory(ctx); err != nil {
		return history.Snapshot{}, errors.New("Clear not confirmed; refresh history before trying again")
	}
	value, err := c.service.History(ctx)
	if err != nil {
		return history.Snapshot{}, errors.New("History unavailable; refresh before trying another clear")
	}
	return value, nil
}
