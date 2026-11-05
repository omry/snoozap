package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"
)

const MaxBytes = 1 << 20

type Config struct {
	SchemaVersion int              `json:"schema_version"`
	Probes        map[string]Probe `json:"probes"`
	Rules         []Rule           `json:"rules"`
	Settings      Settings         `json:"settings"`
}
type Probe struct {
	Type             string `json:"type"`
	Enabled          bool   `json:"enabled"`
	OnStartupUnknown string `json:"on_startup_unknown"`
	OnError          string `json:"on_error"`
	GraceSeconds     int64  `json:"grace_seconds"`
	IntervalMS       int64  `json:"interval_ms"`
	TimeoutMS        int64  `json:"timeout_ms"`
	Name             string `json:"name,omitempty"`
	Executable       string `json:"executable,omitempty"`
	WSLDistribution  string `json:"wsl_distribution,omitempty"`
	LocalEnvironment string `json:"local_environment,omitempty"`
	Source           string `json:"source,omitempty"`
}
type Expression struct {
	Probe string       `json:"probe,omitempty"`
	All   []Expression `json:"all,omitempty"`
	Any   []Expression `json:"any,omitempty"`
	Not   *Expression  `json:"not,omitempty"`
}
type Rule struct {
	ID      string     `json:"id"`
	Enabled bool       `json:"enabled"`
	When    Expression `json:"when"`
}
type Battery struct {
	ACOnly        bool `json:"ac_only"`
	FloorPercent  int  `json:"floor_percent"`
	ResumePercent int  `json:"resume_percent"`
}
type Settings struct {
	Monitoring bool    `json:"monitoring"`
	Battery    Battery `json:"battery"`
}

func Empty() Config {
	return Config{SchemaVersion: 1, Probes: map[string]Probe{}, Rules: []Rule{}, Settings: Settings{Monitoring: true, Battery: Battery{FloorPercent: 20, ResumePercent: 25}}}
}

// strict also rejects case aliases, which encoding/json normally accepts.
func strict(data []byte, target any) error {
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' || json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("expected an object")
	}
	t := reflect.TypeOf(target).Elem()
	allowed := map[string]bool{}
	for n := 0; n < t.NumField(); n++ {
		allowed[strings.Split(t.Field(n).Tag.Get("json"), ",")[0]] = true
	}
	for key, raw := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid, null or unsupported field")
		}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("invalid configuration value")
	}
	return nil
}
func (p *Probe) UnmarshalJSON(data []byte) error {
	type plain Probe
	v := plain{Enabled: true, OnStartupUnknown: "wait", OnError: "retain", GraceSeconds: 60, IntervalMS: 5000, TimeoutMS: 2000}
	if err := strict(data, &v); err != nil {
		return err
	}
	*p = Probe(v)
	return nil
}
func (r *Rule) UnmarshalJSON(data []byte) error {
	type plain Rule
	v := plain{Enabled: true}
	if err := strict(data, &v); err != nil {
		return err
	}
	*r = Rule(v)
	return nil
}
func (s *Settings) UnmarshalJSON(data []byte) error {
	type plain Settings
	v := plain(Empty().Settings)
	if err := strict(data, &v); err != nil {
		return err
	}
	*s = Settings(v)
	return nil
}
func (b *Battery) UnmarshalJSON(data []byte) error {
	type plain Battery
	v := plain(Empty().Settings.Battery)
	if err := strict(data, &v); err != nil {
		return err
	}
	*b = Battery(v)
	return nil
}
func (e *Expression) UnmarshalJSON(data []byte) error {
	type plain Expression
	var v plain
	var fields map[string]json.RawMessage
	if err := strict(data, &v); err != nil {
		return err
	}
	_ = json.Unmarshal(data, &fields)
	if len(fields) != 1 {
		return errors.New("expression requires exactly one operator")
	}
	*e = Expression(v)
	return nil
}
func Parse(r io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return Config{}, errors.New("cannot read configuration")
	}
	if len(data) > MaxBytes {
		return Config{}, errors.New("configuration exceeds 1 MiB")
	}
	tokens := json.NewDecoder(bytes.NewReader(data))
	tokens.UseNumber()
	if err := value(tokens, 0); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if _, err := tokens.Token(); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one JSON object")
	}
	cfg := Empty()
	cfg.SchemaVersion = 0
	if err := strict(data, &cfg); err != nil {
		return Config{}, err
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
func safeID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func clean(s string) bool { return !strings.ContainsFunc(s, unicode.IsControl) && len(s) <= 32767 }
func Validate(c Config) error {
	if c.SchemaVersion != 1 {
		return errors.New("unsupported configuration schema_version; expected 1")
	}
	if c.Probes == nil || c.Rules == nil || len(c.Probes) > 256 || len(c.Rules) > 256 {
		return errors.New("invalid or excessive probes or rules")
	}
	b := c.Settings.Battery
	if b.FloorPercent <= 0 || b.ResumePercent <= b.FloorPercent || b.ResumePercent > 100 {
		return errors.New("invalid battery thresholds")
	}
	for id, p := range c.Probes {
		if !safeID(id) || p.IntervalMS <= 0 || p.TimeoutMS <= 0 || p.TimeoutMS >= p.IntervalMS || p.IntervalMS > int64(time.Duration(1<<63-1)/time.Millisecond) || p.GraceSeconds <= 0 || p.GraceSeconds > int64(time.Duration(1<<63-1)/time.Second) {
			return errors.New("invalid probe identifier or timing")
		}
		if p.OnStartupUnknown != "wait" && p.OnStartupUnknown != "hold" || p.OnError != "retain" && p.OnError != "release_after" {
			return errors.New("invalid probe failure policy")
		}
		switch p.Type {
		case "process":
			if (p.Name == "") == (p.Executable == "") || p.Source != "" || p.LocalEnvironment != "" || !clean(p.Name) || !clean(p.Executable) || strings.ContainsAny(p.Name, `/\`) {
				return errors.New("invalid process probe selector")
			}
			if p.WSLDistribution != "" {
				if len(p.WSLDistribution) > 255 || !clean(p.WSLDistribution) || strings.HasPrefix(p.WSLDistribution, "-") || p.Executable != "" && !strings.HasPrefix(p.Executable, "/") {
					return errors.New("invalid WSL process selector")
				}
			} else if p.Executable != "" && !(len(p.Executable) > 2 && p.Executable[1] == ':' && (p.Executable[2] == '\\' || p.Executable[2] == '/') || strings.HasPrefix(p.Executable, `\\`)) {
				return errors.New("executable path must be absolute")
			}
		case "power":
			if p.Source != "ac" && p.Source != "battery" || p.Name != "" || p.Executable != "" || p.WSLDistribution != "" || p.LocalEnvironment != "" {
				return errors.New("invalid power probe")
			}
		case "t3":
			if !safeID(p.LocalEnvironment) || p.Name != "" || p.Executable != "" || p.Source != "" || p.WSLDistribution != "" {
				return errors.New("invalid T3 local environment reference")
			}
		default:
			return errors.New("unsupported probe type")
		}
	}
	ids := map[string]bool{}
	nodes := 0
	var check func(Expression, int) error
	check = func(e Expression, depth int) error {
		nodes++
		if depth > 32 || nodes > 4096 {
			return errors.New("expression resource limit exceeded")
		}
		ops := 0
		if e.Probe != "" {
			ops++
			if _, ok := c.Probes[e.Probe]; !ok {
				return errors.New("rule references a missing probe")
			}
		}
		for _, group := range [][]Expression{e.All, e.Any} {
			if group != nil {
				ops++
				if len(group) == 0 {
					return errors.New("empty expression group")
				}
				for _, child := range group {
					if err := check(child, depth+1); err != nil {
						return err
					}
				}
			}
		}
		if e.Not != nil {
			ops++
			if err := check(*e.Not, depth+1); err != nil {
				return err
			}
		}
		if ops != 1 {
			return errors.New("expression requires exactly one operator")
		}
		return nil
	}
	for _, r := range c.Rules {
		if !safeID(r.ID) || ids[r.ID] {
			return errors.New("invalid or duplicate rule identifier")
		}
		ids[r.ID] = true
		if err := check(r.When, 1); err != nil {
			return err
		}
	}
	return nil
}
func Clone(c Config) Config {
	data, _ := json.Marshal(c)
	var result Config
	_ = json.Unmarshal(data, &result)
	return result
}
func value(d *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("JSON nesting limit exceeded")
	}
	t, err := d.Token()
	if err != nil {
		return errors.New("malformed JSON")
	}
	delim, nested := t.(json.Delim)
	if !nested {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			t, err := d.Token()
			key, ok := t.(string)
			if err != nil || !ok {
				return errors.New("invalid object key")
			}
			if keys[key] {
				return errors.New("duplicate object key")
			}
			keys[key] = true
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected closing delimiter")
	}
	if _, err := d.Token(); err != nil {
		return errors.New("unclosed JSON container")
	}
	return nil
}
