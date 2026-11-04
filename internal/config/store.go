package config

import (
	"encoding/json"
	"errors"
)

// Store delegates atomic replacement and protected-path checks to the host.
// The engine activates a candidate only after Save succeeds.
type Store struct{ Replace func([]byte) error }

func (s Store) Save(c Config) error {
	data, err := Export(c)
	if err != nil {
		return err
	}
	if s.Replace == nil {
		return errors.New("protected configuration storage unavailable")
	}
	return s.Replace(data)
}

// Export encodes the validated portable configuration only. Native bindings,
// credential values, observations and runtime state are not Config fields.
func Export(c Config) ([]byte, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, errors.New("cannot encode configuration")
	}
	data = append(data, '\n')
	if len(data) > MaxBytes {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	return data, nil
}
