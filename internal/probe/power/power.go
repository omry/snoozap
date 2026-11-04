package power

import (
	"context"

	"github.com/omry/snoozap/internal/model"
)

type Probe struct {
	Reader model.PowerReader
	Source string
}

func (p Probe) Check(ctx context.Context) model.Observation {
	if p.Reader == nil {
		return model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	}
	b, err := p.Reader.ReadPower(ctx)
	if err != nil || !b.Known || b.Source == "unknown" {
		return model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	}
	if b.Source == p.Source {
		return model.Observation{State: model.Active}
	}
	return model.Observation{State: model.Inactive}
}
