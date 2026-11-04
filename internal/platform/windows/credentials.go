//go:build windows

package windows

import (
	"context"
	"errors"
	"runtime"
	"syscall"
	"unsafe"
)

// Credential Manager scopes these generic credentials to the current user's
// logon token. No file-backed storage or plaintext fallback is provided.
// https://learn.microsoft.com/en-us/windows/win32/api/wincred/ns-wincred-credentialw
const maxCredentialBytes = 5 * 512

var errCredential = errors.New("credential store unavailable or access denied")

type Credentials struct {
	read   func(string) ([]byte, error)
	write  func(string, []byte) error
	remove func(string) error
}

func NewCredentials() *Credentials {
	return &Credentials{read: readCredential, write: writeCredential, remove: deleteCredential}
}

func credentialTarget(ref string) (string, error) {
	if len(ref) == 0 || len(ref) > 128 {
		return "", errors.New("invalid credential reference")
	}
	for _, c := range ref {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "", errors.New("invalid credential reference")
		}
	}
	return "SnooZap/credential/" + ref, nil
}

func (c *Credentials) Read(ctx context.Context, ref string) ([]byte, error) {
	target, err := credentialTarget(ref)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.read == nil {
		return nil, errCredential
	}
	data, err := c.read(target)
	if err != nil || len(data) == 0 || len(data) > maxCredentialBytes {
		clear(data)
		return nil, errCredential
	}
	if err := ctx.Err(); err != nil {
		clear(data)
		return nil, err
	}
	return data, nil
}

func (c *Credentials) Write(ctx context.Context, ref string, data []byte) error {
	target, err := credentialTarget(ref)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxCredentialBytes {
		return errors.New("invalid credential size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.write == nil {
		return errCredential
	}
	copy := append([]byte(nil), data...)
	defer clear(copy)
	if err := c.write(target, copy); err != nil {
		return errCredential
	}
	return nil
}

func (c *Credentials) Delete(ctx context.Context, ref string) error {
	target, err := credentialTarget(ref)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.remove == nil {
		return errCredential
	}
	if err := c.remove(target); err != nil {
		return errCredential
	}
	return nil
}

// Match CREDENTIALW's native layout; pointers never enter exported values.
type nativeCredential struct {
	Flags, Type             uint32
	TargetName, Comment     *uint16
	LastWritten             syscall.Filetime
	BlobSize                uint32
	Blob                    *byte
	Persist, AttributeCount uint32
	Attributes              unsafe.Pointer
	TargetAlias, UserName   *uint16
}

func readCredential(target string) ([]byte, error) {
	name, _ := syscall.UTF16PtrFromString(target)
	var cred *nativeCredential
	ok, _, _ := security.NewProc("CredReadW").Call(uintptr(unsafe.Pointer(name)), 1, 0, uintptr(unsafe.Pointer(&cred)))
	runtime.KeepAlive(name)
	if ok == 0 || cred == nil {
		return nil, errCredential
	}
	defer security.NewProc("CredFree").Call(uintptr(unsafe.Pointer(cred)))
	if cred.Type != 1 || cred.BlobSize == 0 || cred.BlobSize > maxCredentialBytes || cred.Blob == nil {
		return nil, errCredential
	}
	return append([]byte(nil), unsafe.Slice(cred.Blob, int(cred.BlobSize))...), nil
}

func writeCredential(target string, data []byte) error {
	name, _ := syscall.UTF16PtrFromString(target)
	cred := nativeCredential{Type: 1, TargetName: name, BlobSize: uint32(len(data)), Blob: &data[0], Persist: 2}
	ok, _, _ := security.NewProc("CredWriteW").Call(uintptr(unsafe.Pointer(&cred)), 0)
	runtime.KeepAlive(name)
	runtime.KeepAlive(data)
	runtime.KeepAlive(cred)
	if ok == 0 {
		return errCredential
	}
	return nil
}

func deleteCredential(target string) error {
	name, _ := syscall.UTF16PtrFromString(target)
	ok, _, err := security.NewProc("CredDeleteW").Call(uintptr(unsafe.Pointer(name)), 1, 0)
	runtime.KeepAlive(name)
	if ok == 0 && err != syscall.Errno(1168) {
		return errCredential
	}
	return nil
}
