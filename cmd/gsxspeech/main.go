//go:build windows && cgo

// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

/*
#include <stdlib.h>
#include <windows.h>
#include "c-api.h"

// Take function signatures from the header even though symbol lookup happens at runtime
#define FN(name) static __typeof__(&name) p_##name;
FN(SherpaOnnxCreateOfflineRecognizer)
FN(SherpaOnnxDestroyOfflineRecognizer)
FN(SherpaOnnxCreateOfflineStream)
FN(SherpaOnnxDestroyOfflineStream)
FN(SherpaOnnxAcceptWaveformOffline)
FN(SherpaOnnxDecodeOfflineStream)
FN(SherpaOnnxGetOfflineStreamResult)
FN(SherpaOnnxDestroyOfflineRecognizerResult)
#undef FN

static int load_library(const wchar_t *dir, const wchar_t *path) {
  // Keep DLL dependencies in the worker directory or System32
  if (!SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32 | LOAD_LIBRARY_SEARCH_USER_DIRS)) return 0;
  if (!AddDllDirectory(dir)) return 0;
  // Keep the library loaded for the lifetime of the worker's function pointers
  HMODULE dll=LoadLibraryExW(path,NULL,LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32|LOAD_LIBRARY_SEARCH_USER_DIRS);
  if (!dll) return 0;
#define FN(name) p_##name = (__typeof__(&name))GetProcAddress(dll,#name); if (!p_##name) return 0;
  FN(SherpaOnnxCreateOfflineRecognizer)
  FN(SherpaOnnxDestroyOfflineRecognizer)
  FN(SherpaOnnxCreateOfflineStream)
  FN(SherpaOnnxDestroyOfflineStream)
  FN(SherpaOnnxAcceptWaveformOffline)
  FN(SherpaOnnxDecodeOfflineStream)
  FN(SherpaOnnxGetOfflineStreamResult)
  FN(SherpaOnnxDestroyOfflineRecognizerResult)
#undef FN
  return 1;
}

static const SherpaOnnxOfflineRecognizer *recognizer(
    char *enc,char *dec,char *join,char *tokens,char *words,char *vocab,int threads) {
  SherpaOnnxOfflineRecognizerConfig cfg = {0};
  // These two NeMo transducer models use 16 kHz audio and 128 features
  cfg.feat_config.sample_rate=16000;
  cfg.feat_config.feature_dim=128;
  cfg.model_config.transducer.encoder=enc;
  cfg.model_config.transducer.decoder=dec;
  cfg.model_config.transducer.joiner=join;
  cfg.model_config.tokens=tokens;
  cfg.model_config.provider="cpu";
  cfg.model_config.model_type="nemo_transducer";
  cfg.model_config.num_threads=threads;
  // Use beam search so the recognizer can apply hotword bias
  cfg.decoding_method="modified_beam_search";
  cfg.max_active_paths=4;
  // BPE phrase bias needs both the hotwords and the recognizer's token vocabulary
  if (words[0] && vocab[0]) {
    cfg.hotwords_file=words;
    // Higher scores inserted unrelated service words in Parakeet smoke tests
    cfg.hotwords_score=2.5f;
    cfg.model_config.modeling_unit="bpe";
    cfg.model_config.bpe_vocab=vocab;
  }
  return p_SherpaOnnxCreateOfflineRecognizer(&cfg);
}

// Go drops the prior recognizer after a successful model swap and the current one at shutdown
static void destroy(const SherpaOnnxOfflineRecognizer *r) { p_SherpaOnnxDestroyOfflineRecognizer(r); }
static const SherpaOnnxOfflineRecognizerResult *transcribe(const SherpaOnnxOfflineRecognizer *r, float *samples, int n) {
  // Create a new stream per PTT recording so utterances stay independent
  const SherpaOnnxOfflineStream *stream = p_SherpaOnnxCreateOfflineStream(r);
  if (!stream) return NULL;
  p_SherpaOnnxAcceptWaveformOffline(stream,16000,samples,n);
  p_SherpaOnnxDecodeOfflineStream(r,stream);
  // The result has its own lifetime and survives destruction of the stream
  const SherpaOnnxOfflineRecognizerResult *result = p_SherpaOnnxGetOfflineStreamResult(stream);
  p_SherpaOnnxDestroyOfflineStream(stream);
  return result;
}
// Free the standalone result after Go has copied its text
static void destroy_result(const SherpaOnnxOfflineRecognizerResult *r) { p_SherpaOnnxDestroyOfflineRecognizerResult(r); }
*/
import "C"

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ziul510/GSXCommsGo/internal/speech"
	"golang.org/x/sys/windows"
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	wide, _ := windows.UTF16PtrFromString(dir)
	dll, _ := windows.UTF16PtrFromString(filepath.Join(dir, "sherpa-onnx-c-api.dll"))
	// The worker stays available to report a missing native runtime during hello
	ready := C.load_library((*C.wchar_t)(unsafe.Pointer(wide)), (*C.wchar_t)(unsafe.Pointer(dll))) != 0
	var r *C.SherpaOnnxOfflineRecognizer
	defer func() {
		if r != nil {
			C.destroy(r)
		}
	}()
	reader := bufio.NewReader(os.Stdin)
	for {
		request, err := speech.Read(reader)
		if err != nil {
			return
		}
		resp := speech.Frame{ID: request.ID, Op: request.Op}
		switch request.Op {
		case "hello":
			if !ready {
				resp.Error = "app-local sherpa-onnx runtime is unavailable"
			}
		case "load":
			if !ready {
				resp.Error = "app-local sherpa-onnx runtime is unavailable"
				break
			}
			// The parent still tracks the old model if loading its replacement fails
			next, err := load(request)
			if err != nil {
				resp.Error = err.Error()
				break
			}
			if r != nil {
				C.destroy(r)
			}
			r = next
		case "transcribe":
			if r == nil {
				resp.Error = "no model loaded"
				break
			}
			resp.Text, err = recognize(r, request)
			if err != nil {
				resp.Error = err.Error()
			}
		// Native inference is synchronous The parent can stop an unresponsive worker
		case "cancel":
		case "shutdown":
			_ = speech.Write(os.Stdout, resp)
			return
		default:
			resp.Error = "unknown speech worker operation"
		}
		if err := speech.Write(os.Stdout, resp); err != nil {
			return
		}
	}
}

func load(f speech.Frame) (*C.SherpaOnnxOfflineRecognizer, error) {
	// The worker expects one transducer model bundle with three graphs and its token table
	files := []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"}
	argv := make([]*C.char, 0, 6)
	// Each C string is only needed while the recognizer is being created
	defer func() {
		for _, s := range argv {
			C.free(unsafe.Pointer(s))
		}
	}()
	for _, name := range files {
		path := f.Paths[name]
		if !filepath.IsAbs(path) {
			return nil, errors.New("invalid recognition model path")
		}
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("missing recognition model file %s", name)
		}
		argv = append(argv, C.CString(path))
	}
	words, vocab := "", ""
	if len(f.Hotwords) > 0 {
		dir, err := os.MkdirTemp("", "gsxcomms-bias-")
		if err != nil {
			return nil, err
		}
		// Keep the generated bias files only through recognizer setup
		defer os.RemoveAll(dir)
		words, vocab = filepath.Join(dir, "words.txt"), filepath.Join(dir, "bpe.txt")
		if err = os.WriteFile(words, []byte(strings.Join(f.Hotwords, "\n")+"\n"), 0600); err != nil {
			return nil, err
		}
		var b strings.Builder
		tokens, err := os.ReadFile(f.Paths["tokens.txt"])
		if err != nil {
			return nil, err
		}
		// Reuse the recognizer's token spellings when building the bias vocabulary
		for _, line := range strings.Split(string(tokens), "\n") {
			line = strings.TrimSuffix(line, "\r")
			token := line
			if i := strings.LastIndex(line, " "); i >= 0 {
				token = line[:i]
			}
			if token != "" && token != "<unk>" {
				fmt.Fprintf(&b, "%s\t-0.5\n", token)
			}
		}
		if err := os.WriteFile(vocab, []byte(b.String()), 0600); err != nil {
			return nil, err
		}
	}
	argv = append(argv, C.CString(words), C.CString(vocab))
	// Cap recognizer threads and try to leave CPU time for the simulator
	threads := C.int(max(1, min(4, runtime.NumCPU()-2)))
	r := C.recognizer(argv[0], argv[1], argv[2], argv[3], argv[4], argv[5], threads)
	// Recognition can still run if Sherpa rejects the optional bias setup
	if r == nil && words != "" {
		empty := C.CString("")
		defer C.free(unsafe.Pointer(empty))
		r = C.recognizer(argv[0], argv[1], argv[2], argv[3], empty, empty, threads)
	}
	if r == nil {
		return nil, errors.New("sherpa-onnx could not open the selected model")
	}
	return r, nil
}

func recognize(r *C.SherpaOnnxOfflineRecognizer, f speech.Frame) (string, error) {
	// The pipe payload must match the Windows capture format
	if f.SampleRate != 16000 || len(f.PCM)%2 != 0 {
		return "", errors.New("expected 16 kHz PCM16 mono")
	}
	if len(f.PCM) < 8000 {
		return "", nil
	}
	samples := make([]float32, len(f.PCM)/2)
	peak := 0
	// Convert PCM16 to normalized float samples and measure peak loudness in one pass
	for i := range samples {
		n := int(int16(binary.LittleEndian.Uint16(f.PCM[i*2:])))
		peak = max(peak, n, -n)
		samples[i] = float32(n) / 32768
	}
	// Keep the same silence gate for callers that bypass the UI
	if peak < 350 {
		return "", nil
	}
	res := C.transcribe(r, (*C.float)(&samples[0]), C.int(len(samples)))
	if res == nil {
		return "", errors.New("recognizer returned no result")
	}
	defer C.destroy_result(res)
	// Copy the native text before destroying the result
	return strings.TrimSpace(C.GoString(res.text)), nil
}
