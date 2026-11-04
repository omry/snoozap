package config

import (
	"encoding/json"
	"errors"
)

// Store delegates atomic replacement and protected-path checks to the host.
// The engine activates a candidate only after Save succeeds.
type Store struct{ Replace func([]byte) error }

func (s Store) Save(c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	if s.Replace == nil {
		return errors.New("protected configuration storage unavailable")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return errors.New("cannot encode configuration")
	}
	data = append(data, '\n')
	if len(data) > MaxBytes {
		return errors.New("configuration exceeds 1 MiB")
	}
	return s.Replace(data)
}
