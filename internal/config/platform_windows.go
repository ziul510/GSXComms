// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// LockSettings allows one app to write a settings directory at a time.
func LockSettings(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(dir, ".gsxcomms.lock"))
	if err != nil {
		return nil, err
	}
	// An exclusive handle also releases the lock if the app crashes.
	handle, err := windows.CreateFile(path, windows.GENERIC_WRITE|windows.DELETE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, fmt.Errorf("could not lock settings in %s. Check whether GSXComms is already open or the directory is writable: %w", dir, err)
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = windows.CloseHandle(handle) }, nil
}

// Protect malformed legacy settings for recovery by the current Windows user.
func protectSettingsBackup(data []byte) ([]byte, error) {
	if uint64(len(data)) > uint64(^uint32(0)) {
		return nil, errors.New("settings backup is too large")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: unsafe.SliceData(data)}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...), nil
}

func Replace(source, destination string) error {
	return moveFile(source, destination, windows.MOVEFILE_REPLACE_EXISTING)
}

func moveNew(source, destination string) error {
	return moveFile(source, destination, 0)
}

func moveFile(source, destination string, flags uint32) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// Flush the same volume move before reporting success
	return windows.MoveFileEx(from, to, flags|windows.MOVEFILE_WRITE_THROUGH)
}

var (
	advapi     = windows.NewLazySystemDLL("advapi32.dll")
	credRead   = advapi.NewProc("CredReadW")
	credWrite  = advapi.NewProc("CredWriteW")
	credDelete = advapi.NewProc("CredDeleteW")
	credFree   = advapi.NewProc("CredFree")
)

const keyTarget = "GSXCommsGo/Voxtral"

// Match CREDENTIALW layout for the Windows credential calls
type credential struct {
	Flags       uint32
	Type        uint32
	Target      *uint16
	Comment     *uint16
	LastWritten windows.Filetime
	Size        uint32
	Blob        *byte
	Persist     uint32
	Count       uint32
	Attributes  uintptr
	Alias       *uint16
	User        *uint16
}

var credentialWrites sync.Mutex

// Serialize explicit saves with implicit migration so an older key can't win a race
func SaveKey(token string) error {
	credentialWrites.Lock()
	defer credentialWrites.Unlock()
	return writeKey(keyTarget, token)
}

// Only seed the vault when no current credential exists
func migrateLegacyKey(token string) error {
	return migrateKeyIfMissing(token, ReadKey, func(key string) error { return writeKey(keyTarget, key) })
}

func migrateKeyIfMissing(token string, read func() (string, error), write func(string) error) error {
	credentialWrites.Lock()
	defer credentialWrites.Unlock()
	// Implicit migration must preserve credentials the user already saved in the vault
	// Read and seed under the same lock so a user save cannot slip between them.
	stored, err := read()
	if err != nil || stored != "" {
		return err
	}
	return write(token)
}

func writeKey(name, token string) error {
	const generic = 1
	const localMachine = 2
	target := windows.StringToUTF16Ptr(name)
	// An empty value removes the stored credential An absent entry is already clear
	if token == "" {
		ok, _, err := credDelete.Call(uintptr(unsafe.Pointer(target)), generic, 0)
		if ok == 0 && !errors.Is(err, windows.ERROR_NOT_FOUND) {
			return err
		}
		return nil
	}
	// Pass the token as a byte blob and clear the temporary copy after the call
	blob := []byte(token)
	defer clear(blob)
	// The credential limit applies to bytes, so multibyte keys can reach it sooner.
	if len(blob) > 2560 {
		return errors.New("Voxtral key exceeds the credential size limit")
	}
	entry := credential{Type: generic, Target: target, Size: uint32(len(blob)), Blob: &blob[0], Persist: localMachine}
	ok, _, err := credWrite.Call(uintptr(unsafe.Pointer(&entry)), 0)
	// The entry contains pointers that Windows uses during CredWrite
	runtime.KeepAlive(entry)
	if ok == 0 {
		return err
	}
	return nil
}

func ReadKey() (string, error) {
	return readKey(keyTarget)
}

func readKey(name string) (string, error) {
	target := windows.StringToUTF16Ptr(name)
	var entry *credential
	ok, _, err := credRead.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		// Treat an empty vault as a normal state when setting up or migrating the key
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			return "", nil
		}
		return "", err
	}
	defer credFree.Call(uintptr(unsafe.Pointer(entry)))
	// Copy the credential before freeing the Windows buffer
	return string(unsafe.Slice(entry.Blob, int(entry.Size))), nil
}
