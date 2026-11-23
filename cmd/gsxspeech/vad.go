//go:build windows && cgo

// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"path/filepath"

	ort "github.com/yalue/onnxruntime_go"
	"github.com/ziul510/GSXCommsGo/internal/speech"
)

//go:embed silero_vad.onnx
var sileroModel []byte

const (
	vadRate    = 16000
	vadWindow  = 512
	vadContext = 64
)

type voiceDetector struct {
	session *ort.AdvancedSession
	tensors []*ort.Tensor[float32]
	rate    *ort.Scalar[int64]
}

func openVoiceDetector(dir string) (_ *voiceDetector, err error) {
	ort.SetSharedLibraryPath(filepath.Join(dir, "onnxruntime.dll"))
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, err
	}
	v := &voiceDetector{}
	// Clean up any tensors already created if a later initialization step fails.
	defer func() {
		if err != nil {
			v.close()
		}
	}()
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	if err := opts.SetIntraOpNumThreads(1); err != nil {
		return nil, err
	}
	// Bind reusable audio, recurrent state, probability, and next state tensors
	shapes := []ort.Shape{{1, vadWindow + vadContext}, {2, 1, 128}, {1, 1}, {2, 1, 128}}
	for _, shape := range shapes {
		tensor, err := ort.NewEmptyTensor[float32](shape)
		if err != nil {
			return nil, err
		}
		v.tensors = append(v.tensors, tensor)
	}
	v.rate, err = ort.NewScalar(int64(vadRate))
	if err != nil {
		return nil, err
	}
	v.session, err = ort.NewAdvancedSessionWithONNXData(sileroModel,
		[]string{"input", "state", "sr"}, []string{"output", "stateN"},
		[]ort.Value{v.tensors[0], v.tensors[1], v.rate}, []ort.Value{v.tensors[2], v.tensors[3]}, opts)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (v *voiceDetector) close() {
	// Destroy the session before the tensors it uses for input and output.
	if v.session != nil {
		_ = v.session.Destroy()
	}
	for _, tensor := range v.tensors {
		_ = tensor.Destroy()
	}
	if v.rate != nil {
		_ = v.rate.Destroy()
	}
	_ = ort.DestroyEnvironment()
}

func (v *voiceDetector) filter(f speech.Frame) ([]byte, error) {
	if f.SampleRate != vadRate || len(f.PCM)%2 != 0 {
		return nil, errors.New("expected 16 kHz PCM16 mono")
	}
	if !speech.Usable(f.PCM) {
		return nil, nil
	}
	audio, state := v.tensors[0].GetData(), v.tensors[1].GetData()
	prob, next := v.tensors[2].GetData(), v.tensors[3].GetData()
	// Reset context and recurrent state between PTT recordings
	clear(audio)
	clear(state)
	var spans speechSpans
	samples := len(f.PCM) / 2
	for offset := 0; offset < samples; offset += vadWindow {
		window := audio[vadContext:]
		n := min(vadWindow, samples-offset)
		for i := range n {
			window[i] = float32(int16(binary.LittleEndian.Uint16(f.PCM[2*(offset+i):]))) / 32768
		}
		// Zero the tail of a short final window so old samples cannot affect its score.
		clear(window[n:])
		if err := v.session.Run(); err != nil {
			return nil, err
		}
		spans.observe(offset, prob[0])
		// Silero reads the previous window's last 64 samples with each new window.
		copy(audio[:vadContext], audio[vadWindow:])
		copy(state, next)
	}
	// Do not count the padded final frame or pending silence as speech
	end := samples
	if spans.quiet > 0 {
		end = (samples+vadWindow-1)/vadWindow*vadWindow - spans.quiet
	}
	spans.finish(end)
	var pcm []byte
	for _, span := range spans.kept {
		pcm = append(pcm, f.PCM[2*span.start:2*min(span.end, samples)]...)
	}
	return pcm, nil
}

type sampleSpan struct{ start, end int }

type speechSpans struct {
	start, quiet int
	active       bool
	kept         []sampleSpan
}

func (s *speechSpans) observe(offset int, probability float32) {
	const endSilence = 5 * vadWindow // 160 ms
	if !s.active {
		if probability >= 0.5 {
			s.start, s.active = offset, true
		}
		return
	}
	// Silero's lower offset threshold keeps weak syllables inside speech
	if probability >= 0.35 {
		s.quiet = 0
		return
	}
	s.quiet += vadWindow
	if s.quiet >= endSilence {
		s.finish(offset + vadWindow - s.quiet)
	}
}

func (s *speechSpans) finish(end int) {
	const minSpeech, padding = 5 * vadWindow, 5 * vadWindow // 160 ms each
	if s.active && end-s.start >= minSpeech {
		span := sampleSpan{max(0, s.start-padding), end + padding}
		// Keep short pauses and copy each sample at most once
		if last := len(s.kept) - 1; last >= 0 && span.start <= s.kept[last].end {
			s.kept[last].end = span.end
		} else {
			s.kept = append(s.kept, span)
		}
	}
	s.active, s.quiet = false, 0
}
