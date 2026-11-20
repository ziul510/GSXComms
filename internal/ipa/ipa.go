// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package ipa

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type tokenizer struct {
	Tokens         map[string]int64
	End, Start     int64
	Repeated       int
	Lower, Bounded bool
	Special        []string
}

type Phonemizer struct {
	Text, Phones tokenizer
	Dictionary   map[string]string
	Infer        func([]int64) ([]int64, error) `json:"-"`
	phones       map[int64]string
	cache        map[string]string
}

func Load(path string, infer func([]int64) ([]int64, error)) (*Phonemizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := &Phonemizer{Infer: infer, phones: make(map[int64]string), cache: make(map[string]string)}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, err
	}
	if out.Text.Repeated < 1 || out.Phones.Repeated < 1 || len(out.Text.Tokens) == 0 || len(out.Phones.Tokens) == 0 {
		return nil, errors.New("invalid phonemizer tokenizers")
	}
	for s, i := range out.Phones.Tokens {
		out.phones[i] = s
	}
	// Keep tokenizer markers out of the spoken IPA.
	for _, s := range out.Phones.Special {
		delete(out.phones, out.Phones.Tokens[s])
	}
	return out, nil
}

var digits = regexp.MustCompile(`[0-9]+`)

const separators = "().,:?!/– "

func (p *Phonemizer) Convert(text string) (string, error) {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.ReplaceAll(strings.ReplaceAll(text, " .", "."), ".", " .")
	var numberErr error
	// Spell numbers out so the phonemizer gets words it can pronounce.
	text = digits.ReplaceAllStringFunc(text, func(literal string) string {
		n, err := strconv.ParseUint(literal, 10, 64)
		if err != nil {
			numberErr = err
			return literal
		}
		return english(n)
	})
	if numberErr != nil {
		return "", numberErr
	}
	var output, word strings.Builder
	flush := func() error {
		if word.Len() == 0 {
			return nil
		}
		value, err := p.word(word.String(), true)
		if err != nil {
			return err
		}
		output.WriteString(value)
		word.Reset()
		return nil
	}
	for _, c := range text {
		if strings.ContainsRune(separators, c) {
			if err := flush(); err != nil {
				return "", err
			}
			output.WriteRune(c)
		} else if c == '-' || unicode.IsLetter(c) || unicode.IsNumber(c) {
			word.WriteRune(c)
		}
	}
	if err := flush(); err != nil {
		return "", err
	}
	return output.String(), nil
}

func (p *Phonemizer) word(word string, expand bool) (string, error) {
	if len(word) <= 1 && strings.Contains(separators+"-", word) {
		return word, nil
	}
	lower := strings.ToLower(word)
	// Prefer the checkpoint's dictionary pronunciations before running inference.
	for _, candidate := range []string{word, lower, cases.Title(language.Und).String(word)} {
		if value, ok := p.Dictionary[candidate]; ok {
			return value, nil
		}
	}
	if expand {
		var expanded strings.Builder
		for i, c := range word {
			// Split capitals so acronyms can be pronounced one letter at a time.
			if i > 0 && unicode.IsUpper(c) && !strings.HasSuffix(expanded.String(), "-") {
				expanded.WriteByte('-')
			}
			expanded.WriteRune(c)
		}
		parts := strings.Split(expanded.String(), "-")
		if len(parts) > 1 {
			for i, part := range parts {
				value, err := p.word(part, false)
				if err != nil {
					return "", err
				}
				parts[i] = value
			}
			return strings.Join(parts, "-"), nil
		}
	}
	if value, ok := p.cache[word]; ok {
		return value, nil
	}
	var ids []int64
	if p.Text.Bounded {
		ids = append(ids, p.Text.Start)
	}
	for _, c := range word {
		if p.Text.Lower {
			c = unicode.ToLower(c)
		}
		if id, ok := p.Text.Tokens[string(c)]; ok {
			// The exported graph takes the checkpoint's repeated-character input.
			for range p.Text.Repeated {
				ids = append(ids, id)
			}
		}
	}
	if p.Text.Bounded {
		ids = append(ids, p.Text.End)
	}
	if len(ids) == 0 {
		return "", nil
	}
	tokens, err := p.Infer(ids)
	if err != nil {
		return "", err
	}
	dedup := make([]int64, 0, len(tokens))
	// Collapse repeated predictions before undoing the tokenizer's own repeats.
	var previous int64
	for _, id := range tokens {
		if id == 0 || id == previous {
			continue
		}
		previous = id
		dedup = append(dedup, id)
		if id == p.Phones.End {
			break
		}
	}
	var result strings.Builder
	if p.Phones.Bounded && len(dedup) >= 2 {
		result.WriteString(p.phones[dedup[0]])
		for i := 1; i < len(dedup)-1; i += p.Phones.Repeated {
			result.WriteString(p.phones[dedup[i]])
		}
		result.WriteString(p.phones[dedup[len(dedup)-1]])
	} else {
		for i := 0; i < len(dedup); i += p.Phones.Repeated {
			result.WriteString(p.phones[dedup[i]])
		}
	}
	value := result.String()
	// Bound cache growth over a long-running worker session.
	if len(p.cache) >= 4096 {
		clear(p.cache)
	}
	p.cache[word] = value
	return value, nil
}

var low = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
var tens = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}

func english(n uint64) string {
	switch {
	case n < 20:
		return low[n]
	case n < 100:
		value := tens[n/10]
		if n%10 > 0 {
			value += "-" + low[n%10]
		}
		return value
	case n < 1000:
		value := low[n/100] + " hundred"
		if n%100 > 0 {
			value += " and " + english(n%100)
		}
		return value
	}
	for _, m := range []struct {
		value uint64
		label string
	}{
		{1e18, "quintillion"}, {1e15, "quadrillion"}, {1e12, "trillion"}, {1e9, "billion"}, {1e6, "million"}, {1e3, "thousand"},
	} {
		if n < m.value {
			continue
		}
		out := english(n/m.value) + " " + m.label
		n %= m.value
		switch {
		case n == 0:
			return out
		case n < 100:
			return out + " and " + english(n)
		default:
			return out + ", " + english(n)
		}
	}
	return ""
}
