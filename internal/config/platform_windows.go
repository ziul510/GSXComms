// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func Replace(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// Replace the existing path in one operation and ask Windows to flush the move
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
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
	// Copy the credential out of Windows-owned memory before CredFree runs
	return string(unsafe.Slice(entry.Blob, int(entry.Size))), nil
}
