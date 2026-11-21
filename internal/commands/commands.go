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
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ziul510/GSXCommsGo/internal/assets"
	"github.com/ziul510/GSXCommsGo/internal/config"
)

//go:embed language.json
var languageJSON []byte

var language = struct {
	NATO        map[string]string `json:"NATO_PHONETIC_WORDS"`
	Corrections map[string]string `json:"STT_TEXT_CORRECTIONS"`
	Boost       []string          `json:"STT_DOMAIN_BOOST_WORDS"`
	Numbers     map[string]string `json:"NUMBER_WORDS"`
	Stop        []string          `json:"MENU_STOP_WORDS"`
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
	// Try longer NATO phrases before shorter alternatives sharing a prefix
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
	// Surface read errors so a broken local vocabulary override is visible
	raw, err := os.ReadFile(filepath.Join(dir, "words.json"))
	if errors.Is(err, os.ErrNotExist) {
		raw, err = assets.Files.ReadFile("words.json")
	}
	if err != nil {
		return Vocabulary{}, err
	}
	return Parse(raw)
}

// Loader shares the last valid vocabulary across command routing and speech
type Loader struct {
	mu      sync.Mutex
	good    *Vocabulary
	warning string
}

// Load reports a changed warning once and keeps a usable vocabulary available
func (l *Loader) Load(dir string) (Vocabulary, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	vocab, err := Load(dir)
	if err == nil {
		l.good, l.warning = &vocab, ""
		return vocab, ""
	}
	if l.good == nil {
		raw, _ := assets.Files.ReadFile("words.json")
		stock, err := Parse(raw)
		if err != nil {
			panic("invalid embedded vocabulary")
		}
		l.good = &stock
	}
	message := "words.json: could not read the override"
	var parse *vocabularyError
	if errors.As(err, &parse) {
		message = fmt.Sprintf("words.json: %s at byte %d", parse.reason, parse.offset)
	}
	if message == l.warning {
		return *l.good, ""
	}
	l.warning = message
	return *l.good, message + ". Using last valid vocabulary."
}

type vocabularyError struct {
	reason string
	offset int64
}

func (e *vocabularyError) Error() string { return fmt.Sprintf("%s at byte %d", e.reason, e.offset) }

func list(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var values []string
	switch raw[0] {
	case '"':
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, false
		}
		values = []string{value}
	case '[':
		var entries []*string
		if json.Unmarshal(raw, &entries) != nil {
			return nil, false
		}
		for _, entry := range entries {
			if entry == nil {
				return nil, false
			}
			values = append(values, *entry)
		}
	default:
		return nil, false
	}
	return slices.DeleteFunc(values, func(s string) bool { return strings.TrimSpace(s) == "" }), true
}

func Parse(raw []byte) (Vocabulary, error) {
	// File order lets DEBOARDING take precedence when "deboard" also matches "board"
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	d := json.NewDecoder(bytes.NewReader(raw))
	invalid := func(reason string) error { return &vocabularyError{reason, d.InputOffset()} }
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return Vocabulary{}, invalid("expected a vocabulary object")
	}
	var v Vocabulary
	// Track the first rule for each bias phrase separately from the full hotword set
	seen, terms := map[string]bool{}, map[string]bool{}
	for _, term := range language.Boost {
		terms[term] = true
	}
	for d.More() {
		name, err := d.Token()
		if err != nil {
			return Vocabulary{}, invalid("invalid JSON")
		}
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil {
			return Vocabulary{}, invalid("invalid JSON")
		}
		e := entry{Name: name.(string)}
		var ok, biasOK bool
		if len(raw) > 0 && raw[0] == '{' {
			var parts map[string]json.RawMessage
			_ = json.Unmarshal(raw, &parts)
			e.Keywords, ok = list(parts["keywords"])
			e.Bias, biasOK = list(parts["bias_variants"])
		} else {
			e.Keywords, ok = list(raw)
			biasOK = true
		}
		if !ok || !biasOK {
			return Vocabulary{}, invalid("expected strings or a list of strings in vocabulary entry")
		}
		v.entries = append(v.entries, e)
		for _, term := range append(slices.Clone(e.Keywords), e.Bias...) {
			terms[strings.ToLower(strings.TrimSpace(term))] = true
		}
		if len(e.Keywords) == 0 {
			continue
		}
		// Bias variants normalize to the first service keyword
		canonical := strings.ToLower(strings.TrimSpace(e.Keywords[0]))
		for _, variant := range e.Bias {
			variant = strings.ToLower(strings.TrimSpace(variant))
			// The first service in file order owns a duplicate bias phrase
			if seen[variant] {
				continue
			}
			seen[variant] = true
			pattern, err := regexp.Compile("(?i)" + regexp.QuoteMeta(variant))
			if err != nil {
				return Vocabulary{}, invalid("invalid bias variant")
			}
			v.rules = append(v.rules, rule{pattern, canonical, utf8.RuneCountInString(variant)})
		}
	}
	if _, err := d.Token(); err != nil {
		return Vocabulary{}, invalid("invalid JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return Vocabulary{}, invalid("unexpected data after vocabulary object")
	}
	// Replace longer variants first so a shorter phrase can't consume their prefix
	sort.SliceStable(v.rules, func(i, j int) bool { return v.rules[i].specificity > v.rules[j].specificity })
	for term := range terms {
		v.hotwords = append(v.hotwords, term)
	}
	// A stable order keeps the speech worker's hotword signature from changing between reads
	slices.Sort(v.hotwords)
	return v, nil
}

// Return a copy so callers can't change compiled vocabulary state
func (v Vocabulary) Hotwords() []string { return slices.Clone(v.hotwords) }

// Use the same Unicode word characters for both sides of a replacement
func word(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func replaceWhole(text string, re *regexp.Regexp, noHyphen bool, replace func(string) string) string {
	var out strings.Builder
	end := 0
	for _, match := range re.FindAllStringIndex(text, -1) {
		left, _ := utf8.DecodeLastRuneInString(text[:match[0]])
		right, _ := utf8.DecodeRuneInString(text[match[1]:])
		// Keep Unicode word boundaries and leave bias variants after a hyphen alone
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

// Match whole phrases to keep ordinary words and identifiers intact
var localCorrections = []struct {
	pattern   *regexp.Regexp
	canonical string
}{
	{regexp.MustCompile(`(?i)(?:(?:cock\s+pitch|cock\s*pit|clock\s+pit)\s+(?:to|two|too|a)\s+(?:the\s+)?|clock\s+picture\s+)ground`), "cockpit to ground"},
	{regexp.MustCompile(`(?i)ground\s+powder`), "ground power"},
	{regexp.MustCompile(`(?i)(?:g[.\s-]+p[.\s-]+u|gee\s+pee\s+you)`), "GPU"},
	{regexp.MustCompile(`(?i)jet[\s-]+way`), "jetway"},
	{regexp.MustCompile(`(?i)jet\s+breach`), "jet bridge"},
	{regexp.MustCompile(`(?i)air\s+(?:stares|stainers)`), "air stairs"},
	{regexp.MustCompile(`(?i)(?:fuel\s+track|fool\s+truck)`), "fuel truck"},
	{regexp.MustCompile(`(?i)de[\s-]+sing`), "deicing"},
	{regexp.MustCompile(`(?i)de[\s-]+boarding`), "deboarding"},
	{regexp.MustCompile(`(?i)laboratory\s+truck`), "lavatory truck"},
	{regexp.MustCompile(`(?i)laboratory\s+service`), "lavatory service"},
}

var localRequest = regexp.MustCompile(`(?i)^(\s*(?:(?:cockpit to ground|ground)[\s,.:;!?]+)?(?:please[\s,]+)?)(?:x(?:-|\s+)?ray|re[\s-]+quest)($|[^\p{L}\p{N}_])`)

// CorrectLocalTranscript repairs known Parakeet and Orukeet mishearings
func CorrectLocalTranscript(text string) string {
	for _, correction := range localCorrections {
		text = replaceWhole(text, correction.pattern, false, func(string) string { return correction.canonical })
	}
	// Correct only the opening request phrase so NATO gate names stay intact
	return localRequest.ReplaceAllString(text, "${1}request${2}")
}

// Fix common transcript errors and NATO words before applying service bias rules
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
	// Prefer the original codeword when correction matches a different preset
	for _, candidate := range []string{raw, text} {
		for _, p := range presets {
			if p.Enabled && len(p.Services) > 0 && p.Codeword != "" && strings.Contains(strings.ToLower(candidate), p.Codeword) {
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
		// Greetings require all their configured words Services can use any one keyword
		if e.Name == "GREETING" {
			greeting = true
			for _, keyword := range e.Keywords {
				greeting = greeting && strings.Contains(text, keyword)
			}
			continue
		}
		// A deboarding match takes priority over the overlapping boarding substring
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
	// Avoid acting on an utterance that matches several services
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

// Normalize common compounds before matching commands or menu options
var (
	deice    = regexp.MustCompile(`\b(de|anti)[\s_-]+ic`)
	pushback = regexp.MustCompile(`\bpush[\s_-]+back\b`)
	jetway   = regexp.MustCompile(`\bjet[\s_-]+(ways?)\b`)
	punct    = regexp.MustCompile(`[-_/\\.,;:!?"'()\[\]]`)
)

func Tokens(text string) []string {
	// Normalize spaced spellings before punctuation breaks them into separate tokens
	text = deice.ReplaceAllString(strings.ToLower(text), "deic")
	text = pushback.ReplaceAllString(text, "pushback")
	text = jetway.ReplaceAllString(text, "jet$1")
	words := strings.Fields(punct.ReplaceAllString(text, " "))
	var tokens []string
	for _, word := range words {
		if n := language.Numbers[word]; n != "" {
			// Keep the spoken word for text matching and add its digit for numbered choices
			tokens = append(tokens, n)
		}
		tokens = append(tokens, word)
	}
	return tokens
}

// Trim common endings for close matches while keeping short command words intact
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

// Score matching blocks with the same ratio as difflib
func ratio(left, right string) float64 {
	a, b := []rune(left), []rune(right)
	if len(a)+len(b) == 0 {
		return 1
	}
	positions := make(map[rune][]int)
	for j, char := range b {
		positions[char] = append(positions[char], j)
	}
	// Mirror difflib's frequent character cutoff on longer inputs
	if len(b) >= 200 {
		limit := len(b)/100 + 1
		for char, idx := range positions {
			if len(idx) > limit {
				delete(positions, char)
			}
		}
	}
	// Repeat the longest match search on each side of the matching block
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
				// Extend the diagonal match ending one rune back in both inputs
				n := prev[j-1] + 1
				next[j] = n
				if n > size {
					bestI, bestJ, size = i-n+1, j-n+1, n
				}
			}
			prev = next
		}
		// Extend the chosen block through matching characters excluded from the seed index
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
		// Only compare the ranges before and after the best block so matches stay in order
		return size + count(al, bestI, bl, bestJ) + count(bestI+size, ah, bestJ+size, bh)
	}
	return 2 * float64(count(0, len(a), 0, len(b))) / float64(len(a)+len(b))
}

// Require exact matches for short words that are easy to confuse
func wordRatio(a, b string) float64 {
	if a == b {
		return 1
	}
	if utf8.RuneCountInString(a) < 4 || utf8.RuneCountInString(b) < 4 {
		return 0
	}
	return ratio(a, b)
}

// Accept close inflections and prefixes before trying the fuzzy word score
func matches(a, b string) bool {
	x, y := stem(a), stem(b)
	return a == b || x == y || (len(x) >= 3 && len(y) >= 3 && (strings.HasPrefix(x, y) || strings.HasPrefix(y, x))) || wordRatio(a, b) >= 0.8
}

// Drop filler words for scoring unless that would remove the entire label
func meaningful(all []string) []string {
	result := slices.DeleteFunc(slices.Clone(all), func(s string) bool { return slices.Contains(language.Stop, s) })
	if len(result) == 0 {
		return all
	}
	return result
}

// Tokens are already spaced Pad both ends to avoid substring matches within words
func phrase(needle, haystack string) bool {
	return strings.Contains(" "+haystack+" ", " "+needle+" ")
}

// Look for prefixes that reverse a command's meaning even when the stem is similar
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
	// Leave implicit submenu navigation to the GSX session
	if strings.Contains(y, "additional services") && !strings.Contains(x, "additional") {
		return 0
	}
	n := 0
	// Give an exact option phrase more weight than individual overlapping words
	if len(y) >= 3 && phrase(y, x) {
		n += 150 + utf8.RuneCountInString(y)
	} else if len(x) >= 3 && phrase(x, y) {
		n += 120 + utf8.RuneCountInString(x)
	}
	a, b = meaningful(a), meaningful(b)
	// Track option coverage separately from how many transcript words match
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
		// Opposite actions like connect and disconnect can share a fuzzy match
		if negated {
			n -= 60
		}
	}
	n += len(covered) * 40 / len(b)
	// Give explicit numbered choices a strong bonus using whole number tokens
	for _, prefix := range []string{"option ", "number ", "choice "} {
		if phrase(prefix+strconv.Itoa(index+1), x) {
			return n + 200
		}
	}
	return n
}

// Apply the minimum score and keep the earlier option when scores tie
func Match(text string, options []string) (index, points int) {
	if index, explicit := ExplicitNumber(text); explicit {
		if index >= 0 && index < len(options) {
			return index, max(200, score(text, options[index], index))
		}
		return -1, 0
	}
	index = -1
	for i, option := range options {
		if n := score(text, option, i); n >= 25 && n > points {
			index, points = i, n
		}
	}
	return
}

func ExplicitNumber(text string) (int, bool) {
	words := strings.Fields(strings.ToLower(text))
	if len(words) != 2 || !slices.Contains([]string{"option", "number", "choice"}, strings.TrimSuffix(words[0], ":")) {
		return -1, false
	}
	number := strings.TrimRight(words[1], ".,!?")
	if digit := language.Numbers[number]; digit != "" {
		number = digit
	}
	digits := strings.TrimPrefix(number, "-")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return -1, false
	}
	index, err := strconv.Atoi(number)
	if err != nil || index < 1 || index > 9 {
		return -1, true
	}
	return index - 1, true
}

func CorrectMenu(text string, options []string) (string, float64) {
	// Tokens already add a digit for each number word Drop the duplicate word for correction
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
		// A single strongly matching word can make a longer label a correction candidate
		combined := chars
		for _, x := range a {
			for _, y := range b {
				combined = max(combined, wordRatio(x, y))
			}
		}
		// Use phrase similarity to break ties from equally close words
		if combined >= 0.75 && (combined > bestRatio || (combined == bestRatio && chars > bestChars)) {
			best, bestRatio, bestChars = option, combined, chars
		}
	}
	return best, bestRatio
}

// Subtract one from standalone menu numbers
func Number(text string) (int, bool) {
	text = strings.TrimRight(strings.TrimSpace(strings.ToLower(text)), ".,!?")
	if len(text) == 1 && text[0] >= '1' && text[0] <= '9' {
		return int(text[0] - '1'), true
	}
	index := slices.Index([]string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}, text)
	return index, index >= 0
}

// Return the configured service aliases
func (v Vocabulary) Keywords(service string) []string {
	for _, e := range v.entries {
		if e.Name == service {
			return slices.Clone(e.Keywords)
		}
	}
	return nil
}
