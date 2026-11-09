//go:build windows

package desktop

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/omry/snoozap/internal/model"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type Presentation struct {
	Title, Reason, Selected, Request, Tooltip, Icon string
	Limitations                                     []string
}

// Describe never derives an accepted hold from a selected mode or a stale view.
func Describe(state State) Presentation {
	s := state.Snapshot
	p := Presentation{Title: "Activity unconfirmed", Icon: "unknown", Reason: s.Reason, Request: "No accepted sleep-prevention request"}
	if !state.Connected {
		p.Title, p.Reason, p.Selected = "Owner unavailable", "Protection is unconfirmed", "Mode unavailable"
	} else {
		switch {
		case s.NativeError != "":
			p.Title, p.Icon, p.Reason = "Sleep prevention failed", "failed", s.NativeError
		case s.Battery.Blocked:
			p.Title, p.Icon, p.Reason = "Sleep prevention paused", "paused", s.Battery.Reason
		case s.Hold.OSAccepted && s.NativeRequestAccepted && s.NativeRequestActive && s.Hold.Requested:
			p.Title, p.Icon = "Keeping awake", "accepted"
			p.Request = "Windows accepted the request; Sleep and lid policies still apply"
			if s.Activity == model.Unknown && s.Mode == model.Auto {
				p.Reason = unknownHoldReason(s)
			}
		case s.DesiredHold || s.Hold.Requested:
			p.Title, p.Reason = "Protection unconfirmed", "Sleep-prevention request is not accepted"
		case s.Mode == model.AllowSleep || s.Activity != model.Unknown && s.ConfigurationError == "":
			p.Title, p.Icon = "Normal sleep allowed", "allowed"
		}
		if s.ConfigurationError != "" {
			p.Reason = s.ConfigurationError
		}
		mode := map[model.Mode]string{model.Auto: "Auto", model.Awake: "Stay awake", model.AllowSleep: "Allow sleep"}[s.Mode]
		p.Selected = "Selected: " + mode
		if s.Deadline != nil {
			seconds := max(int64(0), (s.RemainingMS+999)/1000)
			remaining := fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
			p.Selected += " · " + remaining + " remaining; then Auto"
			p.Reason += " · " + remaining + " remaining"
		}
	}
	p.Limitations = limitationText(s.Limitations)
	if state.Message != "" {
		p.Reason = state.Message
	}
	p.Tooltip = nativeText("SnooZap · "+p.Title+" · "+p.Reason, 120)
	p.Reason = nativeText(p.Reason, 160)
	return p
}

func unknownHoldReason(s model.Snapshot) string {
	startup, retained := false, false
	for _, rule := range s.Rules {
		startup = startup || rule.Startup
		retained = retained || rule.Retained
	}
	// RuleStatus is authoritative, but the reason list keeps this truthful for
	// older or partially populated snapshots that still carry the policy cause.
	if !startup && !retained {
		for _, reason := range s.Hold.Reasons {
			startup = startup || strings.HasPrefix(reason, "startup:")
			retained = retained || strings.HasPrefix(reason, "retained:")
		}
	}
	switch {
	case startup && retained:
		return "Activity unconfirmed; startup protection and last confirmed work retained"
	case startup:
		return "Activity unconfirmed; startup protection applied"
	default:
		return "Activity unconfirmed; last confirmed work retained"
	}
}

func limitationText(limitations []string) []string {
	const maxItemUnits = 120
	parts := make([]string, 0, min(len(limitations), maxLimitationItems))
	for _, limitation := range limitations {
		limitation = strings.TrimSpace(nativeText(limitation, maxItemUnits))
		if limitation == "" {
			continue
		}
		parts = append(parts, limitation)
		if len(parts) == maxLimitationItems {
			break
		}
	}
	if len(parts) == 0 {
		return []string{"Runtime limitations unavailable"}
	}
	return parts
}

const maxLimitationItems = 8

func nativeText(s string, limit int) string {
	var out strings.Builder
	units := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			r = ' '
		}
		n := utf16.RuneLen(r)
		if units+n > limit {
			break
		}
		out.WriteRune(r)
		units += n
	}
	return out.String()
}

type trayView struct {
	tray                    *application.SystemTray
	title, reason, selected *application.MenuItem
	request                 *application.MenuItem
	limitations             []*application.MenuItem
	auto, awake, allow      *application.MenuItem
	presets                 []*application.MenuItem
	custom                  *application.MenuItem
	icons                   map[string][]byte
	icon                    string
}

func newTray(a *application.App, execute func(model.Command), show func(bool)) *trayView {
	v := &trayView{tray: a.SystemTray.New(), icons: map[string][]byte{}}
	for _, kind := range []string{"accepted", "allowed", "unknown", "paused", "failed"} {
		v.icons[kind] = trayIcon(kind)
	}
	menu := a.Menu.New()
	v.title = menu.Add("SnooZap · Connecting").SetEnabled(false)
	v.reason = menu.Add("Protection is unconfirmed").SetEnabled(false)
	v.selected = menu.Add("Mode unavailable").SetEnabled(false)
	v.request = menu.Add("No accepted sleep-prevention request").SetEnabled(false)
	menu.AddSeparator()
	mode := func(label string, value model.Mode) *application.MenuItem {
		// Plain items avoid optimistic native radio/checkbox changes before the
		// Service confirms the selected mode. SetChecked reflects snapshots only.
		return menu.Add(label).OnClick(func(*application.Context) {
			execute(model.Command{Operation: "mode", Mode: value})
		})
	}
	v.auto = mode("Auto", model.Auto)
	v.awake = mode("Stay awake", model.Awake)
	sub := menu.AddSubmenu("Stay awake for…")
	for _, preset := range []struct {
		label    string
		duration time.Duration
	}{{"15 minutes", 15 * time.Minute}, {"1 hour", time.Hour}, {"4 hours", 4 * time.Hour}} {
		v.presets = append(v.presets, sub.Add(preset.label).OnClick(func(*application.Context) {
			execute(model.Command{Operation: "mode", Mode: model.Awake, Duration: preset.duration})
		}))
	}
	v.custom = sub.Add("Custom…").OnClick(func(*application.Context) { show(true) })
	v.allow = mode("Allow sleep", model.AllowSleep)
	menu.AddSeparator()
	for i := 0; i < maxLimitationItems; i++ {
		item := menu.Add("Runtime limitations unavailable").SetEnabled(false)
		if i > 0 {
			item.SetHidden(true)
		}
		v.limitations = append(v.limitations, item)
	}
	menu.Add("Open SnooZap").OnClick(func(*application.Context) { show(false) })
	menu.Add("Quit SnooZap").OnClick(func(*application.Context) { execute(model.Command{Operation: "stop"}) })
	v.tray.SetMenu(menu)
	v.tray.OnClick(func() { show(false) }).OnRightClick(v.tray.OpenMenu)
	v.tray.SetIcon(v.icons["unknown"])
	v.tray.SetTooltip("SnooZap · Connecting · Protection is unconfirmed")
	return v
}

// render runs on the UI thread. Item setters update the current native menu;
// rebuilding it during TrackPopupMenu would invalidate keyboard/mouse selection.
// Skip unchanged setters: their native state rewrite can clear the highlight.
func (v *trayView) render(state State) {
	p := Describe(state)
	label := func(item *application.MenuItem, text string) {
		text = strings.ReplaceAll(text, "&", "&&")
		if item.Label() != text {
			item.SetLabel(text)
		}
	}
	label(v.title, "SnooZap · "+p.Title)
	label(v.reason, p.Reason)
	label(v.selected, p.Selected)
	label(v.request, p.Request)
	for i, item := range v.limitations {
		hidden := i >= len(p.Limitations)
		if item.Hidden() != hidden {
			item.SetHidden(hidden)
		}
		if hidden {
			continue
		}
		text := p.Limitations[i]
		if i == 0 {
			text = "Limitations: " + text
		}
		label(item, text)
	}
	s := state.Snapshot
	enabled := state.Connected && !state.Pending
	for item, selected := range map[*application.MenuItem]bool{v.auto: s.Mode == model.Auto, v.awake: s.Mode == model.Awake, v.allow: s.Mode == model.AllowSleep} {
		if checked := state.Connected && selected; item.Checked() != checked {
			item.SetChecked(checked)
		}
		if item.Enabled() != enabled {
			item.SetEnabled(enabled)
		}
	}
	for _, item := range v.presets {
		if item.Enabled() != enabled {
			item.SetEnabled(enabled)
		}
	}
	if v.custom.Enabled() != enabled {
		v.custom.SetEnabled(enabled)
	}
	if p.Icon != v.icon {
		v.tray.SetIcon(v.icons[p.Icon])
		v.icon = p.Icon
	}
	v.tray.SetTooltip(p.Tooltip)
}

// Shapes distinguish status without relying on colour: check, hollow circle,
// question mark, pause bars and cross. Keep the icon local and dependency-free.
func trayIcon(kind string) []byte {
	im := image.NewNRGBA(image.Rect(0, 0, 24, 24))
	ink := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	background := map[string]color.NRGBA{
		"accepted": {R: 0, G: 105, B: 66, A: 255}, "allowed": {R: 60, G: 70, B: 85, A: 255},
		"unknown": {R: 105, G: 72, B: 0, A: 255}, "paused": {R: 105, G: 72, B: 0, A: 255},
		"failed": {R: 170, G: 25, B: 35, A: 255},
	}[kind]
	for y := 1; y < 23; y++ {
		for x := 1; x < 23; x++ {
			im.SetNRGBA(x, y, background)
			dx, dy := x-12, y-12
			dot := false
			switch kind {
			case "accepted":
				dot = x >= 5 && x <= 10 && abs(y-x-5) <= 1 || x >= 10 && x <= 19 && abs(y+x-25) <= 1
			case "allowed":
				dot = dx*dx+dy*dy >= 35 && dx*dx+dy*dy <= 60
			case "paused":
				dot = y >= 6 && y <= 18 && (x >= 7 && x <= 9 || x >= 15 && x <= 17)
			case "failed":
				dot = x >= 6 && x <= 18 && (abs(y-x) <= 1 || abs(y+x-24) <= 1)
			default:
				dot = y >= 5 && y <= 7 && x >= 8 && x <= 16 || x >= 15 && x <= 17 && y >= 7 && y <= 11 || x >= 11 && x <= 15 && y >= 11 && y <= 13 || x >= 11 && x <= 13 && y >= 13 && y <= 15 || x >= 11 && x <= 13 && y >= 18 && y <= 20
			}
			if dot {
				im.SetNRGBA(x, y, ink)
			}
		}
	}
	var data bytes.Buffer
	_ = png.Encode(&data, im)
	return data.Bytes()
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
