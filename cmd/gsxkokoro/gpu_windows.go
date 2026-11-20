//go:build windows && cgo

// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/csv"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/sys/windows"
)

func addCUDA(options *ort.SessionOptions) bool {
	// Probe only the driver tool installed in the Windows system directory
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return false
	}
	// A slow GPU probe would delay the worker handshake
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "nvidia-smi.exe"), "--query-gpu=index,memory.free,utilization.gpu", "--format=csv,noheader,nounits")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW, HideWindow: true}
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	stats, err := csv.NewReader(strings.NewReader(string(output))).ReadAll()
	if err != nil {
		return false
	}
	device, free, load := -1, 0, 101
	for _, row := range stats {
		if len(row) != 3 {
			continue
		}
		id, a := strconv.Atoi(strings.TrimSpace(row[0]))
		memory, b := strconv.Atoi(strings.TrimSpace(row[1]))
		util, c := strconv.Atoi(strings.TrimSpace(row[2]))
		// Require 1 GiB free and skip busy cards because speech may share a GPU with MSFS
		if a != nil || b != nil || c != nil || id < 0 || memory < 1024 || util > 80 {
			continue
		}
		// Prefer more free memory Break ties with the current GPU load
		if memory > free || (memory == free && util < load) {
			device, free, load = id, memory, util
		}
	}
	if device < 0 {
		return false
	}
	cuda, err := ort.NewCUDAProviderOptions()
	if err != nil {
		return false
	}
	// SessionOptions keeps the configured provider after the temporary CUDA options are freed
	defer cuda.Destroy()
	return cuda.Update(map[string]string{"device_id": strconv.Itoa(device)}) == nil && options.AppendExecutionProviderCUDA(cuda) == nil
}
