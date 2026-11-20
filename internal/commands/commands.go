// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ziul510/GSXCommsGo/internal/assets"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

//go:embed language.json
var languageJSON []byte

var language = struct {
	NATO        map[string]string   `json:"NATO_PHONETIC_WORDS"`
	Corrections map[string]string   `json:"STT_TEXT_CORRECTIONS"`
	Boost       []string            `json:"STT_DOMAIN_BOOST_WORDS"`
	Numbers     map[string]string   `json:"NUMBER_WORDS"`
	Stop        []string            `json:"MENU_STOP_WORDS"`
	Aliases     map[string][]string `json:"GSX_SERVICE_ALIASES"`
}{}

var natoPattern *regexp.Regexp

func init() {
	if err := json.Unmarshal(languageJSON, &language); err != nil {
		panic(err)
	}
	pattern := make([]string, 0, len(language.NATO))
	for key := range language.NATO {
		pattern = append(pattern, regexp.QuoteMeta(key))
	}
	// Try longer NATO phrases before shorter alternatives sharing a prefix.
	sort.Slice(pattern, func(i, j int) bool { return len(pattern[i]) > len(pattern[j]) })
	natoPattern = regexp.MustCompile("(?i)" + strings.Join(pattern, "|"))
}

type entry struct {
	Name           string
	Keywords, Bias []string
}

type rule struct {
	pattern     *regexp.Regexp
	canonical   string
	specificity int
}

type Vocabulary struct {
	entries  []entry
	rules    []rule
	hotwords []string
}

func Load(dir string) (Vocabulary, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "words.json"))
	if errors.Is(err, os.ErrNotExist) {
		raw, err = assets.Files.ReadFile("words.json")
	}
	if err != nil {
		return Vocabulary{}, err
	}
	return Parse(raw)
}

func list(raw json.RawMessage) []string {
	// Older vocabulary entries can store a single keyword as a string.
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil
		}
		values = []string{value}
	}
	return slices.DeleteFunc(values, func(s string) bool { return strings.TrimSpace(s) == "" })
}

func Parse(raw []byte) (Vocabulary, error) {
	// File order lets DEBOARDING take precedence when "deboard" also matches "board".
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return Vocabulary{}, errors.New("expected vocabulary object")
	}
	var v Vocabulary
	seen, terms := map[string]bool{}, map[string]bool{}
	for _, term := range language.Boost {
		terms[term] = true
	}
	for d.More() {
		name, err := d.Token()
		if err != nil {
			return v, err
		}
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil {
			return v, err
		}
		e := entry{Name: name.(string)}
		var parts map[string]json.RawMessage
		// Bias variants guide recognition while keywords drive service routing.
		if json.Unmarshal(raw, &parts) == nil {
			e.Keywords, e.Bias = list(parts["keywords"]), list(parts["bias_variants"])
		} else {
			e.Keywords = list(raw)
		}
		v.entries = append(v.entries, e)
		for _, term := range append(slices.Clone(e.Keywords), e.Bias...) {
			terms[strings.ToLower(strings.TrimSpace(term))] = true
		}
		if len(e.Keywords) == 0 {
			continue
		}
		// Bias variants normalize to the first service keyword.
		canonical := strings.ToLower(strings.TrimSpace(e.Keywords[0]))
		for _, variant := range e.Bias {
			variant = strings.ToLower(strings.TrimSpace(variant))
			if seen[variant] {
				continue
			}
			seen[variant] = true
			pattern, err := regexp.Compile("(?i)" + regexp.QuoteMeta(variant))
			if err != nil {
				return Vocabulary{}, fmt.Errorf("invalid bias variant for %q: %w", e.Name, err)
			}
			v.rules = append(v.rules, rule{pattern, canonical, utf8.RuneCountInString(variant)})
		}
	}
	if _, err := d.Token(); err != nil {
		return v, err
	}
	if _, err := d.Token(); err != io.EOF {
		return Vocabulary{}, errors.New("unexpected data after vocabulary object")
	}
	sort.SliceStable(v.rules, func(i, j int) bool { return v.rules[i].specificity > v.rules[j].specificity })
	for term := range terms {
		v.hotwords = append(v.hotwords, term)
	}
	slices.Sort(v.hotwords)
	return v, nil
}

func (v Vocabulary) Hotwords() []string { return slices.Clone(v.hotwords) }

func word(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func replaceWhole(text string, re *regexp.Regexp, noHyphen bool, replace func(string) string) string {
	var out strings.Builder
	end := 0
	for _, match := range re.FindAllStringIndex(text, -1) {
		left, _ := utf8.DecodeLastRuneInString(text[:match[0]])
		right, _ := utf8.DecodeRuneInString(text[match[1]:])
		// A match bordered by Unicode word characters is part of another word.
		if word(left) || word(right) || (noHyphen && left == '-') {
			continue
		}
		out.WriteString(text[end:match[0]])
		out.WriteString(replace(text[match[0]:match[1]]))
		end = match[1]
	}
	if end == 0 {
		return text
	}
	out.WriteString(text[end:])
	return out.String()
}

var aground = regexp.MustCompile("(?i)aground")

func (v Vocabulary) Normalize(text string) string {
	text = replaceWhole(text, aground, false, func(string) string { return language.Corrections["aground"] })
	text = replaceWhole(text, natoPattern, false, func(match string) string { return language.NATO[strings.ToLower(match)] })
	for _, r := range v.rules {
		text = replaceWhole(text, r.pattern, true, func(string) string { return r.canonical })
	}
	return text
}

type Action struct {
	Kind     string
	Preset   string
	Services []string
}

func (v Vocabulary) Route(text, raw string, presets []config.Preset, menuMatching bool) Action {
	// Normalization may rewrite a preset codeword, so try the original words too.
	for _, candidate := range []string{text, raw} {
		for _, p := range presets {
			if p.Enabled && p.Codeword != "" && strings.Contains(strings.ToLower(candidate), p.Codeword) {
				return Action{Kind: "preset", Preset: p.Name, Services: slices.Clone(p.Services)}
			}
		}
	}
	var matches []string
	greeting, deboard := false, false
	for _, e := range v.entries {
		if len(e.Keywords) == 0 {
			continue
		}
		if e.Name == "GREETING" {
			greeting = true
			for _, keyword := range e.Keywords {
				greeting = greeting && strings.Contains(text, keyword)
			}
			continue
		}
		if e.Name == "BOARDING" && deboard {
			continue
		}
		for _, keyword := range e.Keywords {
			if strings.Contains(text, keyword) {
				matches = append(matches, e.Name)
				deboard = deboard || e.Name == "DEBOARDING"
				break
			}
		}
	}
	switch {
	case len(matches) > 1:
		return Action{Kind: "ambiguous"}
	case len(matches) == 1:
		return Action{Kind: "service", Services: matches}
	case greeting:
		return Action{Kind: "greeting"}
	case menuMatching:
		return Action{Kind: "menu"}
	default:
		return Action{Kind: "unknown"}
	}
}

func Aliases(service string) []string { return slices.Clone(language.Aliases[service]) }

var (
	deice    = regexp.MustCompile(`\b(de|anti)[\s_-]+ic`)
	pushback = regexp.MustCompile(`\bpush[\s_-]+back\b`)
	jetway   = regexp.MustCompile(`\bjet[\s_-]+(ways?)\b`)
	punct    = regexp.MustCompile(`[-_/\\.,;:!?"'()\[\]]`)
)

func Tokens(text string) []string {
	text = deice.ReplaceAllString(strings.ToLower(text), "deic")
	text = pushback.ReplaceAllString(text, "pushback")
	text = jetway.ReplaceAllString(text, "jet$1")
	words := strings.Fields(punct.ReplaceAllString(text, " "))
	var tokens []string
	for _, word := range words {
		if n := language.Numbers[word]; n != "" {
			// Keep the spoken word for text matching and add its digit for numbered choices.
			tokens = append(tokens, n)
		}
		tokens = append(tokens, word)
	}
	return tokens
}

func stem(w string) string {
	if utf8.RuneCountInString(w) <= 3 {
		return w
	}
	for _, suffix := range []string{"ing", "ed", "es", "s"} {
		if strings.HasSuffix(w, suffix) {
			w = strings.TrimSuffix(w, suffix)
			break
		}
	}
	return strings.TrimRight(w, "e")
}

// ratio uses the same longest contiguous matching-block ratio as difflib.
func ratio(left, right string) float64 {
	a, b := []rune(left), []rune(right)
	if len(a)+len(b) == 0 {
		return 1
	}
	positions := make(map[rune][]int)
	for j, char := range b {
		positions[char] = append(positions[char], j)
	}
	if len(b) >= 200 {
		limit := len(b)/100 + 1
		for char, idx := range positions {
			if len(idx) > limit {
				delete(positions, char)
			}
		}
	}
	var count func(int, int, int, int) int
	count = func(al, ah, bl, bh int) int {
		bestI, bestJ, size := al, bl, 0
		prev := map[int]int{}
		for i := al; i < ah; i++ {
			next := map[int]int{}
			for _, j := range positions[a[i]] {
				if j < bl {
					continue
				}
				if j >= bh {
					break
				}
				n := prev[j-1] + 1
				next[j] = n
				if n > size {
					bestI, bestJ, size = i-n+1, j-n+1, n
				}
			}
			prev = next
		}
		for bestI > al && bestJ > bl && a[bestI-1] == b[bestJ-1] {
			bestI--
			bestJ--
			size++
		}
		for bestI+size < ah && bestJ+size < bh && a[bestI+size] == b[bestJ+size] {
			size++
		}
		if size == 0 {
			return 0
		}
		return size + count(al, bestI, bl, bestJ) + count(bestI+size, ah, bestJ+size, bh)
	}
	return 2 * float64(count(0, len(a), 0, len(b))) / float64(len(a)+len(b))
}

func wordRatio(a, b string) float64 {
	if a == b {
		return 1
	}
	if utf8.RuneCountInString(a) < 4 || utf8.RuneCountInString(b) < 4 {
		return 0
	}
	return ratio(a, b)
}

func matches(a, b string) bool {
	x, y := stem(a), stem(b)
	return a == b || x == y || (len(x) >= 3 && len(y) >= 3 && (strings.HasPrefix(x, y) || strings.HasPrefix(y, x))) || wordRatio(a, b) >= 0.8
}

func meaningful(all []string) []string {
	result := slices.DeleteFunc(slices.Clone(all), func(s string) bool { return slices.Contains(language.Stop, s) })
	if len(result) == 0 {
		return all
	}
	return result
}

func phrase(needle, haystack string) bool {
	return strings.Contains(" "+haystack+" ", " "+needle+" ")
}

func opposed(a, b string) bool {
	a, b = stem(a), stem(b)
	for _, prefix := range []string{"dis", "un"} {
		if a == prefix+b || b == prefix+a {
			return true
		}
	}
	return false
}

func score(text, option string, index int) int {
	a, b := Tokens(text), Tokens(option)
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	x, y := strings.Join(a, " "), strings.Join(b, " ")
	// Leave implicit submenu navigation to the GSX session.
	if strings.Contains(y, "additional services") && !strings.Contains(x, "additional") {
		return 0
	}
	n := 0
	if len(y) >= 3 && phrase(y, x) {
		n += 150 + utf8.RuneCountInString(y)
	} else if len(x) >= 3 && phrase(x, y) {
		n += 120 + utf8.RuneCountInString(x)
	}
	a, b = meaningful(a), meaningful(b)
	covered := map[string]bool{}
	for _, sw := range a {
		negated := false
		for _, ow := range b {
			if matches(sw, ow) {
				covered[ow] = true
				n += 30 + utf8.RuneCountInString(sw)
			}
			negated = negated || opposed(sw, ow)
		}
		// Opposite actions like connect and disconnect can share a fuzzy match.
		if negated {
			n -= 60
		}
	}
	n += len(covered) * 40 / len(b)
	for _, prefix := range []string{"option ", "number ", "choice "} {
		if phrase(prefix+strconv.Itoa(index+1), x) {
			return n + 200
		}
	}
	return n
}

func Match(text string, options []string) (index, points int) {
	index = -1
	for i, option := range options {
		if n := score(text, option, i); n >= 25 && n > points {
			index, points = i, n
		}
	}
	return
}

func CorrectMenu(text string, options []string) (string, float64) {
	collapse := func(text string) []string {
		return slices.DeleteFunc(Tokens(text), func(word string) bool { return language.Numbers[word] != "" })
	}
	a := collapse(text)
	if len(a) == 0 {
		return "", 0
	}
	best, bestRatio, bestChars := "", 0.0, -1.0
	for _, option := range options {
		b := collapse(option)
		if len(b) == 0 {
			continue
		}
		chars := ratio(strings.Join(a, " "), strings.Join(b, " "))
		combined := chars
		for _, x := range a {
			for _, y := range b {
				combined = max(combined, wordRatio(x, y))
			}
		}
		// Use phrase similarity to break ties from equally close words.
		if combined >= 0.75 && (combined > bestRatio || (combined == bestRatio && chars > bestChars)) {
			best, bestRatio, bestChars = option, combined, chars
		}
	}
	return best, bestRatio
}

var menuNumber = regexp.MustCompile(`^(?:(?:option|number|choice)\s+)?([1-9])$`)

func Number(text string) (int, bool) {
	text = strings.TrimSpace(strings.ToLower(text))
	if n := language.Numbers[text]; n != "" {
		text = n
	}
	m := menuNumber.FindStringSubmatch(text)
	if len(m) == 0 {
		return 0, false
	}
	return int(m[1][0] - '1'), true
}

func (v Vocabulary) HotwordTable() string {
	var lines []string
	for _, e := range v.entries {
		if len(e.Keywords) > 0 {
			lines = append(lines, e.Name+": "+strings.Join(e.Keywords, ", "))
		}
	}
	return strings.Join(lines, "\n")
}

func (v Vocabulary) Keywords(service string) []string {
	for _, e := range v.entries {
		if e.Name == service {
			return slices.Clone(e.Keywords)
		}
	}
	return nil
}
