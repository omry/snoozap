//go:build windows

package windows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/config"
)

func TestCredentialsDenialIsSanitizedAndHasNoFileFallback(t *testing.T) {
	secret := []byte("private-header-value")
	readBuffer := append([]byte(nil), secret...)
	var writeBuffer []byte
	var names []string
	denied := errors.New("token=private-header-value")
	c := &Credentials{read: func(name string) ([]byte, error) { names = append(names, name); return readBuffer, denied }, write: func(name string, data []byte) error { names = append(names, name); writeBuffer = data; return denied }, remove: func(name string) error { names = append(names, name); return denied }}
	if v, err := c.Read(context.Background(), "probe-1"); v != nil || err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe denied read", err)
	}
	if err := c.Write(context.Background(), "probe-1", secret); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe denied write", err)
	}
	if err := c.Delete(context.Background(), "probe-1"); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe denied delete", err)
	}
	if !bytes.Equal(secret, []byte("private-header-value")) {
		t.Fatal("store changed caller secret")
	}
	for _, b := range append(readBuffer, writeBuffer...) {
		if b != 0 {
			t.Fatal("temporary credential copy not cleared")
		}
	}
	for _, name := range names {
		if name != "SnooZap/credential/probe-1" {
			t.Fatal("unscoped credential target", name)
		}
	}
}

func TestCredentialReferencesSizesAndCancellation(t *testing.T) {
	calls := 0
	c := &Credentials{read: func(string) ([]byte, error) { calls++; return []byte("ok"), nil }, write: func(string, []byte) error { calls++; return nil }, remove: func(string) error { calls++; return nil }}
	for _, ref := range []string{"", "../other", "a/b", "line\nfeed", strings.Repeat("a", 129)} {
		if _, err := c.Read(context.Background(), ref); err == nil {
			t.Fatal("invalid read ref")
		}
		if err := c.Write(context.Background(), ref, []byte("x")); err == nil {
			t.Fatal("invalid write ref")
		}
		if err := c.Delete(context.Background(), ref); err == nil {
			t.Fatal("invalid delete ref")
		}
	}
	for _, data := range [][]byte{nil, make([]byte, maxCredentialBytes+1)} {
		if err := c.Write(context.Background(), "valid", data); err == nil {
			t.Fatal("invalid size")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Read(ctx, "valid"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.Write(ctx, "valid", []byte("ok")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "valid"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("rejected request called native store")
	}
	var empty *Credentials
	if _, err := empty.Read(context.Background(), "valid"); err == nil {
		t.Fatal("missing store accepted")
	}
	if err := empty.Write(context.Background(), "valid", []byte("x")); err == nil {
		t.Fatal("missing store accepted")
	}
	if err := empty.Delete(context.Background(), "valid"); err == nil {
		t.Fatal("missing store accepted")
	}
	if err := c.Write(context.Background(), "valid", make([]byte, maxCredentialBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(context.Background(), "valid"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), "valid"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("valid native operations missing")
	}
}

func TestCredentialReadCancelsAndClearsTemporaryResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	buffer := []byte("synthetic-private-value")
	c := &Credentials{read: func(string) ([]byte, error) { cancel(); return buffer, nil }}
	if data, err := c.Read(ctx, "valid"); data != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled read exposed data", err)
	}
	for _, b := range buffer {
		if b != 0 {
			t.Fatal("cancelled read retained secret bytes")
		}
	}
	for _, size := range []int{0, maxCredentialBytes + 1} {
		buffer := bytes.Repeat([]byte{1}, size)
		c := &Credentials{read: func(string) ([]byte, error) { return buffer, nil }}
		if data, err := c.Read(context.Background(), "valid"); data != nil || err == nil {
			t.Fatal("invalid native result accepted")
		}
		for _, b := range buffer {
			if b != 0 {
				t.Fatal("invalid native result retained bytes")
			}
		}
	}
}

func TestNativeCredentialRoundtripAndDelete(t *testing.T) {
	if os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("native credential acceptance opt-in required")
	}
	c := NewCredentials()
	ref := fmt.Sprintf("test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		if err := c.Delete(context.Background(), ref); err != nil {
			t.Error("credential fixture cleanup failed")
		}
	})
	first := []byte("SnooZap synthetic credential fixture")
	if err := c.Write(context.Background(), ref, first); err != nil {
		t.Fatal(err)
	}
	data, err := c.Read(context.Background(), ref)
	if err != nil || !bytes.Equal(data, first) {
		t.Fatal("native roundtrip failed", err)
	}
	clear(data)
	second := []byte("SnooZap replacement synthetic fixture")
	if err := c.Write(context.Background(), ref, second); err != nil {
		t.Fatal(err)
	}
	data, err = c.Read(context.Background(), ref)
	if err != nil || !bytes.Equal(data, second) {
		t.Fatal("native replacement failed", err)
	}
	clear(data)
	if err := c.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(context.Background(), ref); err == nil {
		t.Fatal("deleted credential remained readable")
	}
	if err := c.Delete(context.Background(), ref); err != nil {
		t.Fatal("delete not idempotent")
	}
}

func TestNativeAtomicSettingsIgnoreInterruptedCandidate(t *testing.T) {
	if os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("native filesystem acceptance opt-in required")
	}
	dir := filepath.Join(t.TempDir(), "private-settings")
	old, err := config.Export(config.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(dir, old); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dir, ".snoozap-config-interrupted")
	if err := os.WriteFile(partial, []byte(`{"schema_version":`), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(dir)
	if err != nil || !loaded.Settings.Monitoring {
		t.Fatal("partial candidate displaced prior file", err)
	}
	candidate := config.Empty()
	candidate.Settings.Monitoring = false
	data, err := config.Export(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(dir)
	if err != nil || loaded.Settings.Monitoring {
		t.Fatal("new atomic file did not activate", err)
	}
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "blocked-destination"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(filepath.Join(dir, "blocked-destination", "missing-parent", "state"), old); err == nil {
		t.Fatal("unwritable path accepted")
	}
	loaded, err = LoadConfig(dir)
	if err != nil || loaded.Settings.Monitoring {
		t.Fatal("failed write changed prior file", err)
	}
}
