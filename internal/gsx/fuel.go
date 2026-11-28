// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package gsx

import (
	"html"
	"regexp"
	"strings"

	"github.com/ziul510/GSXCommsGo/internal/commands"
)

var (
	fuelTitle    = regexp.MustCompile(`(?i)\b(?:fuel|refuell?(?:ing)?)\b`)
	fuelPercent  = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(?:%|per\s*cent\b)`)
	percentInput = regexp.MustCompile(`^(\d+(?:\.\d+)?)%?$`)
	oneHundred   = regexp.MustCompile(`\b(?:one |a )?hundred\b`)
)

// IsFuelQuantity identifies a fuel prompt by its title and percentage choices
func (m Menu) IsFuelQuantity() bool {
	if !fuelTitle.MatchString(m.Title) {
		return false
	}
	// A service or operator menu can mention fuel without offering quantities
	for _, option := range m.Options {
		if fuelPercent.MatchString(normalizeFuelText(option)) {
			return true
		}
	}
	return false
}

// normalizeFuelText treats spoken and displayed percentages as the same label
func normalizeFuelText(text string) string {
	text = strings.Join(strings.Fields(strings.ToLower(html.UnescapeString(text))), " ")
	text = oneHundred.ReplaceAllString(text, "100")
	return fuelPercent.ReplaceAllString(text, "${1}%")
}

// FuelChoice resolves a slot, percentage, or exact label on a fuel prompt
func (m Menu) FuelChoice(text string) int {
	// Bare single digits and number words refer to slots, as in the GSX panel
	index, numbered := commands.Number(text)
	if !numbered {
		index, numbered = commands.ExplicitNumber(text)
	}
	if numbered {
		if index >= 0 && index < len(m.Options) {
			return index
		}
		return -1
	}

	text = normalizeFuelText(strings.TrimRight(strings.TrimSpace(text), ".,!?"))
	percent := percentInput.FindStringSubmatch(text)
	label := strings.Join(commands.Tokens(text), " ")
	index = -1
	for i, option := range m.Options {
		option = normalizeFuelText(option)
		match := label == strings.Join(commands.Tokens(option), " ")
		// Match only complete percentages so a partial value cannot select a row
		if percent != nil {
			values := fuelPercent.FindAllStringSubmatch(option, -1)
			match = match || (len(values) == 1 && values[0][1] == percent[1])
		}
		if match {
			// Duplicate percentages or labels require an explicit slot
			if index >= 0 {
				return -1
			}
			index = i
		}
	}
	return index
}
