package process

import (
	"context"

	"github.com/omry/snoozap/internal/model"
)

type Probe struct {
	Reader           model.ProcessReader
	Name, Executable string
}

func (p Probe) Check(ctx context.Context) model.Observation {
	if p.Reader == nil {
		return model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	}
	return p.Reader.Match(ctx, p.Name, p.Executable)
}
