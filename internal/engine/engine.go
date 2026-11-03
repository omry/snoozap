package engine

import (
	"context"
	"errors"
	"time"

	"github.com/omry/snoozap/internal/model"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type response struct {
	snapshot model.Snapshot
	err      error
}

type request struct {
	ctx   context.Context
	cmd   model.Command
	reply chan response
}

type Engine struct {
	requests chan request
	done     chan struct{}
	closeErr error
}

func New(ctx context.Context, power model.PowerBackend, clock model.Clock, configError string) *Engine {
	if clock == nil {
		clock = realClock{}
	}
	e := &Engine{requests: make(chan request), done: make(chan struct{})}
	go e.run(ctx, power, clock, configError)
	return e
}

func (e *Engine) Snapshot(ctx context.Context) (model.Snapshot, error) {
	return e.Execute(ctx, model.Command{Operation: "status"})
}

func (e *Engine) Execute(ctx context.Context, cmd model.Command) (model.Snapshot, error) {
	r := request{ctx: ctx, cmd: cmd, reply: make(chan response, 1)}
	if err := ctx.Err(); err != nil {
		return model.Snapshot{}, err
	}
	select {
	case <-ctx.Done():
		return model.Snapshot{}, ctx.Err()
	case <-e.done:
		return model.Snapshot{}, model.ErrStopped
	case e.requests <- r:
	}
	select {
	case v := <-r.reply:
		return v.snapshot, v.err
	case <-ctx.Done():
		return model.Snapshot{}, ctx.Err()
	case <-e.done:
		select {
		case v := <-r.reply:
			return v.snapshot, v.err
		default:
			return model.Snapshot{}, model.ErrStopped
		}
	}
}

func (e *Engine) Close(ctx context.Context) error {
	_, err := e.Execute(ctx, model.Command{Operation: "stop"})
	if errors.Is(err, model.ErrStopped) {
		return e.closeErr
	}
	return err
}

func (e *Engine) run(ctx context.Context, power model.PowerBackend, clock model.Clock, configError string) {
	defer close(e.done)
	s := model.Snapshot{
		Sequence: 1, Mode: model.Auto, Monitoring: true, UpdatedAt: clock.Now(),
		ConfigurationError: configError, Reason: "No automatic rules are configured",
		Limitations: []string{
			"An accepted request does not guarantee protection from explicit Sleep or lid close.",
			"Windows Modern Standby and battery policies can limit SystemRequired requests.",
			"SnooZap does not request that the display stay on.",
		},
	}
	snapshot := func() model.Snapshot {
		v := s
		v.Limitations = append([]string(nil), s.Limitations...)
		return v
	}
	cleanup := func() error {
		var err error
		if s.NativeRequestActive {
			err = power.SetRequired(context.Background(), false)
		}
		closeErr := power.Close()
		if closeErr == nil {
			s.NativeRequestActive = false
		}
		s.NativeRequestAccepted = false
		s.DesiredHold = false
		s.Mode = model.Auto
		s.Reason = "Runtime stopped"
		s.Sequence++
		s.UpdatedAt = clock.Now()
		if err != nil || closeErr != nil {
			s.NativeError = "Native request cleanup failed"
		}
		return errors.Join(err, closeErr)
	}
	for {
		select {
		case <-ctx.Done():
			e.closeErr = cleanup()
			return
		case r := <-e.requests:
			if err := r.ctx.Err(); err != nil {
				r.reply <- response{err: err}
				continue
			}
			switch r.cmd.Operation {
			case "status":
				r.reply <- response{snapshot: snapshot()}
			case "stop":
				e.closeErr = cleanup()
				r.reply <- response{snapshot: snapshot(), err: e.closeErr}
				return
			case "mode":
				if !r.cmd.Mode.Valid() {
					r.reply <- response{err: errors.New("invalid manual mode")}
					continue
				}
				s.Mode = r.cmd.Mode
				s.DesiredHold = s.Mode == model.Awake
				s.Reason = "No automatic rules are configured"
				if s.Mode == model.Awake {
					s.Reason = "Manual Stay awake"
				} else if s.Mode == model.AllowSleep {
					s.Reason = "Allow sleep releases SnooZap requests; it does not issue Sleep"
				}
				var err error
				if s.NativeRequestActive != s.DesiredHold {
					err = power.SetRequired(r.ctx, s.DesiredHold)
					if err == nil {
						s.NativeRequestActive = s.DesiredHold
					}
				}
				s.NativeError = ""
				if err != nil {
					s.NativeError = "Windows power request failed: " + err.Error()
				}
				s.NativeRequestAccepted = s.DesiredHold && s.NativeRequestActive && err == nil
				s.Sequence++
				s.UpdatedAt = clock.Now()
				r.reply <- response{snapshot: snapshot(), err: err}
			default:
				r.reply <- response{err: errors.New("unknown runtime operation")}
			}
		}
	}
}
