package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const MaxBytes = 1 << 20

type Config struct {
	SchemaVersion int                        `json:"schema_version"`
	Probes        map[string]json.RawMessage `json:"probes"`
	Rules         []json.RawMessage          `json:"rules"`
}

func Empty() Config {
	return Config{SchemaVersion: 1, Probes: map[string]json.RawMessage{}, Rules: []json.RawMessage{}}
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
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return Config{}, errors.New("configuration root must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Config{}, errors.New("invalid configuration object")
	}
	for name := range fields {
		switch name {
		case "schema_version", "probes", "rules":
		default:
			return Config{}, errors.New("configuration contains invalid or unsupported fields")
		}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, errors.New("configuration contains invalid or unsupported fields")
	}
	if cfg.SchemaVersion != 1 {
		return Config{}, errors.New("unsupported configuration schema_version; expected 1")
	}
	for _, name := range []string{"probes", "rules"} {
		if raw, present := fields[name]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return Config{}, errors.New("probes and rules cannot be null")
		}
	}
	if len(cfg.Probes) != 0 || len(cfg.Rules) != 0 {
		return Config{}, errors.New("this build supports manual controls with empty probes and rules")
	}
	if cfg.Probes == nil {
		cfg.Probes = map[string]json.RawMessage{}
	}
	if cfg.Rules == nil {
		cfg.Rules = []json.RawMessage{}
	}
	return cfg, nil
}

func value(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds 32 levels")
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
