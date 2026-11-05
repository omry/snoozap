// Package probe composes the built-in observations through the shared scheduler.
package probe

import (
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
	httpprobe "github.com/omry/snoozap/internal/probe/http"
	powerprobe "github.com/omry/snoozap/internal/probe/power"
	processprobe "github.com/omry/snoozap/internal/probe/process"
	"github.com/omry/snoozap/internal/probe/t3"
)

type Registry struct {
	Processes   model.ProcessReader
	WSL         func(string) model.ProcessReader
	Power       model.PowerReader
	T3          *t3.Registry
	Credentials httpprobe.Credentials
}

func (r Registry) Resolve(p config.Probe) model.Probe {
	switch p.Type {
	case "process":
		reader := r.Processes
		if p.WSLDistribution != "" {
			reader = nil
			if r.WSL != nil {
				reader = r.WSL(p.WSLDistribution)
			}
		}
		return processprobe.Probe{Reader: reader, Name: p.Name, Executable: p.Executable}
	case "t3":
		return t3.Probe{Registry: r.T3, Environment: p.LocalEnvironment}
	case "power":
		return powerprobe.Probe{Reader: r.Power, Source: p.Source}
	case "http":
		p.Value = append([]byte(nil), p.Value...)
		headers := make(map[string]string, len(p.CredentialHeaders))
		for key, value := range p.CredentialHeaders {
			headers[key] = value
		}
		p.CredentialHeaders = headers
		return httpprobe.Probe{Configuration: p, Credentials: r.Credentials}
	default:
		return nil
	}
}
