//go:build windows

package windows

import (
	"context"
	"errors"
	"runtime"
	"unsafe"

	"github.com/omry/snoozap/internal/model"
)

type SystemPower struct{}
type systemPowerStatus struct {
	ACLineStatus, BatteryFlag, BatteryLifePercent, SystemStatusFlag byte
	BatteryLifeTime, BatteryFullLifeTime                            uint32
}

func (SystemPower) ReadPower(ctx context.Context) (model.Battery, error) {
	if err := ctx.Err(); err != nil {
		return model.Battery{}, err
	}
	var status systemPowerStatus
	ok, _, _ := kernel.NewProc("GetSystemPowerStatus").Call(uintptr(unsafe.Pointer(&status)))
	runtime.KeepAlive(&status)
	if ok == 0 {
		return model.Battery{}, errors.New("power telemetry unavailable")
	}
	b := model.Battery{Source: "unknown", Known: true, Present: status.BatteryFlag != 128}
	if status.ACLineStatus == 0 {
		b.Source = "battery"
	} else if status.ACLineStatus == 1 {
		b.Source = "ac"
	}
	if status.BatteryFlag == 255 {
		return model.Battery{}, errors.New("battery presence unavailable")
	}
	if b.Present && status.BatteryLifePercent <= 100 {
		percent := int(status.BatteryLifePercent)
		b.Percent = &percent
	}
	return b, nil
}
