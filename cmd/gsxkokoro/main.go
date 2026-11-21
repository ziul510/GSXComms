//go:build windows && cgo

// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	ort "github.com/yalue/onnxruntime_go"
	"github.com/ziul510/GSXCommsGo/internal/ipa"
	"github.com/ziul510/GSXCommsGo/internal/speech"
	"golang.org/x/sys/windows"
)

type runtimeState struct {
	gpu         *ort.SessionOptions
	phonemes    *ipa.Phonemizer
	g2p, kokoro *ort.DynamicAdvancedSession
	opts        *ort.SessionOptions
	vocab       map[string]int64
	voice       []float32
	base        string
	runtime     string
}

func main() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// The parent starts this worker from its own directory with the matching native runtime
	state := &runtimeState{base: filepath.Dir(exe)}
	ready := state.initialize()
	defer state.close()
	reader := bufio.NewReader(os.Stdin)
	for {
		f, err := speech.Read(reader)
		if err != nil {
			return
		}
		// Echo the ID and operation so the parent can associate this reply with a request
		result := speech.Frame{ID: f.ID, Op: f.Op}
		switch f.Op {
		case "shutdown":
			_ = speech.Write(os.Stdout, result)
			return
		// Requests are handled serially, so this opcode only acknowledges the caller
		case "cancel":
		case "hello":
			err = ready
		case "load":
			if err = ready; err == nil {
				err = state.load(f.Paths)
				result.Runtime = state.runtime
			}
		// The phonemizer can run independently of the downloaded voice model
		case "phonemize":
			if err = ready; err == nil {
				result.Text, err = state.phonemize(f.Text)
			}
		case "synthesize":
			if err = ready; err == nil {
				result.PCM, err = state.say(f.Text)
				result.SampleRate = 24000
			}
		default:
			err = errors.New("unknown Kokoro worker operation")
		}
		if err != nil {
			result.Error = err.Error()
		}
		if err := speech.Write(os.Stdout, result); err != nil {
			return
		}
	}
}

func (s *runtimeState) initialize() error {
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	set, add := kernel.NewProc("SetDefaultDllDirectories"), kernel.NewProc("AddDllDirectory")
	// Resolve native dependencies from Windows and the worker's own directory
	const restricted = windows.LOAD_LIBRARY_SEARCH_SYSTEM32 | windows.LOAD_LIBRARY_SEARCH_USER_DIRS
	if ok, _, err := set.Call(restricted); ok == 0 {
		return err
	}
	base, err := windows.UTF16PtrFromString(s.base)
	if err != nil {
		return err
	}
	if ok, _, err := add.Call(uintptr(unsafe.Pointer(base))); ok == 0 {
		return err
	}
	// Load ONNX Runtime from beside the worker
	ort.SetSharedLibraryPath(filepath.Join(s.base, "onnxruntime.dll"))
	if err := ort.InitializeEnvironment(); err != nil {
		return err
	}
	options, err := ort.NewSessionOptions()
	if err != nil {
		return err
	}
	s.opts = options
	// Limit CPU inference threads to leave capacity for the simulator
	if err := options.SetIntraOpNumThreads(max(1, min(4, runtime.NumCPU()-2))); err != nil {
		return err
	}
	if err := options.SetInterOpNumThreads(1); err != nil {
		return err
	}
	// Keep separate options so a failed CUDA setup leaves CPU inference available
	gpu, err := ort.NewSessionOptions()
	if err == nil {
		if addCUDA(gpu) && gpu.SetIntraOpNumThreads(2) == nil && gpu.SetInterOpNumThreads(1) == nil {
			s.gpu = gpu
		} else {
			_ = gpu.Destroy()
		}
	}
	return nil
}

// Dispose sessions and their options before tearing down the shared ONNX environment
func (s *runtimeState) close() {
	if s.kokoro != nil {
		_ = s.kokoro.Destroy()
	}
	if s.g2p != nil {
		_ = s.g2p.Destroy()
	}
	if s.gpu != nil {
		_ = s.gpu.Destroy()
	}
	if s.opts != nil {
		_ = s.opts.Destroy()
	}
	if ort.IsInitialized() {
		_ = ort.DestroyEnvironment()
	}
}

func (s *runtimeState) phonemize(text string) (string, error) {
	// Create the bundled phonemizer only when the first request needs it
	if s.phonemes == nil {
		g2p, err := ort.NewDynamicAdvancedSession(filepath.Join(s.base, "phonemizer.onnx"), []string{"sequence"}, []string{"tokens"}, s.opts)
		if err != nil {
			return "", err
		}
		p, err := ipa.Load(filepath.Join(s.base, "phonemizer.json"), func(seq []int64) ([]int64, error) {
			const contextSize = 256
			if len(seq) > contextSize {
				return nil, errors.New("word exceeds the phonemizer context window")
			}
			// The exported ONNX graph expects 256 tokens even for short words
			padded := make([]int64, contextSize)
			copy(padded, seq)
			input, err := ort.NewTensor(ort.NewShape(1, contextSize), padded)
			if err != nil {
				return nil, err
			}
			defer input.Destroy()
			output, err := ort.NewEmptyTensor[int64](ort.NewShape(1, contextSize))
			if err != nil {
				return nil, err
			}
			defer output.Destroy()
			if err := g2p.Run([]ort.Value{input}, []ort.Value{output}); err != nil {
				return nil, err
			}
			// Keep only predictions for the original tokens The rest are padding
			return append([]int64(nil), output.GetData()[:len(seq)]...), nil
		})
		if err != nil {
			_ = g2p.Destroy()
			return "", err
		}
		// Keep the inference session alive because the phonemizer callback captures it
		s.g2p, s.phonemes = g2p, p
	}
	return s.phonemes.Convert(text)
}

func (s *runtimeState) load(paths map[string]string) error {
	// The load frame gives absolute paths for the graph, voice data, and tokenizer
	for _, file := range []string{"model.onnx", "am_adam.bin", "tokenizer.json"} {
		if !filepath.IsAbs(paths[file]) {
			return fmt.Errorf("invalid Kokoro path for %s", file)
		}
	}
	data, err := os.ReadFile(paths["tokenizer.json"])
	if err != nil {
		return err
	}
	var tok struct {
		Model struct{ Vocab map[string]int64 }
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return err
	}
	if len(tok.Model.Vocab) == 0 {
		return errors.New("Kokoro tokenizer is empty")
	}
	raw, err := os.ReadFile(paths["am_adam.bin"])
	if err != nil {
		return err
	}
	// Each style vector is 256 floats and index zero is reserved for empty input
	if len(raw) < 2048 || len(raw)%1024 != 0 {
		return errors.New("Kokoro voice has an invalid size")
	}
	voice := make([]float32, len(raw)/4)
	for i := range voice {
		voice[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	// Read the pinned graph's waveform output name instead of hardcoding it
	_, output, err := ort.GetInputOutputInfoWithOptions(paths["model.onnx"], s.opts)
	if err != nil {
		return err
	}
	if len(output) == 0 {
		return errors.New("Kokoro graph has no output")
	}
	open := func(options *ort.SessionOptions) (*ort.DynamicAdvancedSession, error) {
		return ort.NewDynamicAdvancedSession(paths["model.onnx"], []string{"input_ids", "style", "speed"}, []string{output[0].Name}, options)
	}
	var session *ort.DynamicAdvancedSession
	// The probe can succeed even when the CUDA provider can't load this graph
	if s.gpu != nil {
		session, err = open(s.gpu)
	}
	engine := "CUDA"
	if session == nil {
		engine = "CPU"
		session, err = open(s.opts)
	}
	if err != nil {
		return err
	}
	if s.kokoro != nil {
		_ = s.kokoro.Destroy()
	}
	// Swap the model bundle only after all of its parts have opened
	s.kokoro, s.vocab, s.voice = session, tok.Model.Vocab, voice
	s.runtime = engine
	return nil
}

func (s *runtimeState) say(text string) ([]byte, error) {
	if s.kokoro == nil {
		return nil, errors.New("Kokoro model is not loaded")
	}
	var output []byte
	// Use sentence boundaries for voice chunks and pauses
	sentences := strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(".!?;", r) })
	for i, sentence := range sentences {
		phonemes, err := s.phonemize(strings.TrimSpace(sentence))
		if err != nil {
			return nil, err
		}
		// Drop IPA characters the Kokoro vocabulary cannot encode
		valid := []rune(strings.TrimSpace(strings.Map(func(r rune) rune {
			if _, ok := s.vocab[string(r)]; ok {
				return r
			}
			return -1
		}, phonemes)))
		// Leave room for two boundary tokens and stay within the available style vectors
		context := min(510, len(s.voice)/256-1)
		for len(valid) > 0 {
			n := min(len(valid), context)
			// Prefer a word or punctuation boundary when a sentence needs another chunk
			if n < len(valid) {
				for j := n - 1; j > 0; j-- {
					if strings.ContainsRune(".,!?; ", valid[j]) {
						n = j + 1
						break
					}
				}
			}
			first := []rune(strings.TrimSpace(string(valid[:n])))
			valid = valid[n:]
			if len(first) == 0 {
				continue
			}
			// Leave a zero token at each edge of the Kokoro input
			tokens := make([]int64, len(first)+2)
			for j, r := range first {
				tokens[j+1] = s.vocab[string(r)]
			}
			// Pick the style vector for the unpadded token count
			styleIndex := len(first) * 256
			audio, err := s.run(tokens, s.voice[styleIndex:styleIndex+256])
			if err != nil {
				return nil, err
			}
			// Convert samples to signed PCM16 for playback
			for _, sample := range audio {
				normalized := max(-32768, min(32767, float64(sample)*32768))
				output = binary.LittleEndian.AppendUint16(output, uint16(int16(normalized)))
			}
		}
		if i < len(sentences)-1 && len(output) > 0 {
			// At 24 kHz mono PCM16, 16800 bytes gives a 350 ms pause
			output = append(output, make([]byte, 16800)...)
		}
	}
	if len(output) == 0 {
		return nil, errors.New("Kokoro did not produce speech")
	}
	return output, nil
}

func (s *runtimeState) run(tokens []int64, style []float32) ([]float32, error) {
	// The graph takes a batch of tokens, one style vector, and a speed multiplier
	ids, err := ort.NewTensor(ort.NewShape(1, int64(len(tokens))), tokens)
	if err != nil {
		return nil, err
	}
	defer ids.Destroy()
	voice, err := ort.NewTensor(ort.NewShape(1, 256), style)
	if err != nil {
		return nil, err
	}
	defer voice.Destroy()
	speed, err := ort.NewTensor(ort.NewShape(1), []float32{1})
	if err != nil {
		return nil, err
	}
	defer speed.Destroy()
	// Let ONNX allocate the waveform tensor since the sample count varies with the input
	outputs := []ort.Value{nil}
	if err := s.kokoro.Run([]ort.Value{ids, voice, speed}, outputs); err != nil {
		return nil, err
	}
	defer outputs[0].Destroy()
	audio, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, errors.New("Kokoro returned an unexpected waveform type")
	}
	// Keep the waveform data independent of the tensor's lifetime
	return append([]float32(nil), audio.GetData()...), nil
}
