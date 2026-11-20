// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func addonManager() string {
	// FSDreamTeam's user install and older machine install use different registry keys.
	for _, candidate := range []struct {
		key         registry.Key
		path, value string
	}{
		{registry.CURRENT_USER, `SOFTWARE\FSDreamTeam`, "root"},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\FSDreamTeam\Addon Manager`, "Path"},
	} {
		key, err := registry.OpenKey(candidate.key, candidate.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		value, _, err := key.GetStringValue(candidate.value)
		key.Close()
		if err == nil && filepath.IsAbs(value) {
			return value
		}
	}
	return `C:\Program Files (x86)\Addon Manager`
}

// The console and GUI both need the installed panel's menu file.
func Panel() string {
	if override := os.Getenv("GSX_PANEL_DIR"); filepath.IsAbs(override) {
		return override
	}
	return filepath.Join(addonManager(), "MSFS", "fsdreamteam-gsx-pro", "html_ui", "InGamePanels", "FSDT_GSX_Panel")
}

type connection struct {
	lib         windows.Handle
	handle      uintptr
	api         map[string]uintptr
	definitions map[string]uintptr
	request     uintptr
}

func Connect(ctx context.Context) (Transport, error) {
	// Try the user's DLL first, then installed copies that can satisfy the handshake.
	paths := []string{os.Getenv("GSX_SIMCONNECT_DLL")}
	for _, root := range []string{os.Getenv("MSFS2024_SDK"), os.Getenv("MSFS_SDK"),
		filepath.Join(os.Getenv("SystemDrive")+string(os.PathSeparator), "MSFS 2024 SDK"),
		filepath.Join(os.Getenv("SystemDrive")+string(os.PathSeparator), "MSFS SDK"),
	} {
		if filepath.IsAbs(root) {
			paths = append(paths, filepath.Join(root, "SimConnect SDK", "lib", "SimConnect.dll"))
		}
	}
	paths = append(paths, filepath.Join(addonManager(), "couatl", "SimConnect.dll"))
	if programFiles := os.Getenv("ProgramFiles"); filepath.IsAbs(programFiles) {
		paths = append(paths, filepath.Join(programFiles, "LorbyAxisAndOhs_MSFS24", "SimConnect.dll"))
	}
	var last error
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			continue
		}
		// Skip missing candidates before asking Windows to load the library.
		if _, err := os.Stat(path); err != nil {
			continue
		}
		c, err := connectPath(ctx, path)
		if err == nil {
			return c, nil
		}
		// Keep the native failure if a DLL existed but couldn't connect.
		last = err
	}
	if last == nil {
		last = errors.New("a compatible installed SimConnect client was not found")
	}
	return nil, fmt.Errorf("SimConnect unavailable: %w", last)
}

func connectPath(ctx context.Context, path string) (_ *connection, err error) {
	// Search beside this DLL and in System32 for its native dependencies.
	lib, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return nil, err
	}
	c := &connection{lib: lib, api: make(map[string]uintptr), definitions: make(map[string]uintptr)}
	// Any failed symbol lookup, open, or handshake must release the library.
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	// Resolve the whole transport API before opening a simulator handle.
	for _, name := range []string{"Open", "Close", "GetNextDispatch", "AddToDataDefinition", "RequestDataOnSimObject", "SetDataOnSimObject"} {
		proc, e := windows.GetProcAddress(lib, "SimConnect_"+name)
		if e != nil {
			return nil, e
		}
		c.api[name] = proc
	}
	name := []byte("GSXComms\x00")
	if result, _, _ := syscall.SyscallN(c.api["Open"], uintptr(unsafe.Pointer(&c.handle)), uintptr(unsafe.Pointer(&name[0])), 0, 0, 0, 0); failed(result) {
		return nil, hresult("Open", result)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// A successful Open call still needs the server's OPEN packet before use.
	if _, err = c.receive(ctx, 2, 0); err != nil {
		return nil, err
	}
	return c, nil
}

// HRESULT failure is encoded in the signed low 32 bits.
func failed(result uintptr) bool { return int32(result) < 0 }
func hresult(operation string, result uintptr) error {
	// Keep the original Windows error value for diagnostics.
	return fmt.Errorf("SimConnect %s failed (0x%08x)", operation, uint32(result))
}

func (c *connection) definition(name string) (uintptr, error) {
	// SimConnect data definitions belong to the connection and can be reused across requests.
	if id := c.definitions[name]; id != 0 {
		return id, nil
	}
	id := uintptr(len(c.definitions) + 1)
	// GSX flags are numeric. Aircraft position and speed need explicit units.
	unit := []byte("number\x00")
	if name == "PLANE LATITUDE" || name == "PLANE LONGITUDE" {
		unit = []byte("degrees\x00")
	}
	if name == "PLANE ALTITUDE" {
		unit = []byte("feet\x00")
	}
	if name == Speed {
		unit = []byte("knots\x00")
	}
	label := []byte(name + "\x00")
	// Every definition is one FLOAT64 value with the default datum ID.
	result, _, _ := syscall.SyscallN(c.api["AddToDataDefinition"], c.handle, id, uintptr(unsafe.Pointer(&label[0])), uintptr(unsafe.Pointer(&unit[0])), 4, 0, 0xffffffff)
	if failed(result) {
		return 0, hresult("AddToDataDefinition", result)
	}
	c.definitions[name] = id
	return id, nil
}

// Request one sample for the user aircraft and wait only for its matching reply.
func (c *connection) Get(ctx context.Context, name string) (float64, error) {
	id, err := c.definition(name)
	if err != nil {
		return 0, err
	}
	c.request++
	result, _, _ := syscall.SyscallN(c.api["RequestDataOnSimObject"], c.handle, c.request, id, 0, 1, 0, 0, 0, 0)
	if failed(result) {
		return 0, hresult("RequestDataOnSimObject", result)
	}
	ctx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
	defer cancel()
	return c.receive(ctx, 8, c.request)
}

// Send a single eight-byte value using the connection's cached definition.
func (c *connection) Set(ctx context.Context, name string, value float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := c.definition(name)
	if err != nil {
		return err
	}
	result, _, _ := syscall.SyscallN(c.api["SetDataOnSimObject"], c.handle, id, 0, 0, 0, 8, uintptr(unsafe.Pointer(&value)))
	if failed(result) {
		return hresult("SetDataOnSimObject", result)
	}
	return nil
}

func (c *connection) receive(ctx context.Context, kind uint32, request uintptr) (float64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var ptr *byte
		var size uint32
		r, _, _ := syscall.SyscallN(c.api["GetNextDispatch"], c.handle, uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&size)))
		if !failed(r) && ptr != nil && size >= 12 {
			// The base SimConnect header ends with the packet type after size and version.
			packet := unsafe.Slice(ptr, int(size))
			typeID := uint32(packet[8]) | uint32(packet[9])<<8 | uint32(packet[10])<<16 | uint32(packet[11])<<24
			switch {
			// A QUIT packet means the simulator has closed the connection.
			case typeID == 3:
				return 0, ErrDisconnected
			case typeID == kind && request == 0:
				return 0, nil
			case typeID == kind && size >= 48:
				// A response from a timed-out request can arrive during the next Get.
				id := *(*uint32)(unsafe.Add(unsafe.Pointer(ptr), 12))
				if id == uint32(request) {
					// The packet carries one FLOAT64 datum at dwData after the fixed header.
					bits := *(*uint64)(unsafe.Add(unsafe.Pointer(ptr), 40))
					return math.Float64frombits(bits), nil
				}
			}
			continue
		}
		// Wait briefly when the dispatch queue has no usable packet.
		if err := wait(ctx, 20*time.Millisecond); err != nil {
			return 0, err
		}
	}
}

// Close the simulator handle while the DLL's Close entry point is still loaded.
func (c *connection) Close() error {
	var err error
	if c.handle != 0 {
		result, _, _ := syscall.SyscallN(c.api["Close"], c.handle)
		if failed(result) {
			err = hresult("Close", result)
		}
		c.handle = 0
	}
	if c.lib != 0 {
		err = errors.Join(err, windows.FreeLibrary(c.lib))
		c.lib = 0
	}
	return err
}
