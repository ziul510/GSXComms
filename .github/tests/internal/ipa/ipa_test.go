package ipa

import "testing"

func TestNumbersWithSharedDigits(t *testing.T) {
	// Use a dictionary-only fixture to check that shared digits stay in separate numbers.
	p := Phonemizer{Dictionary: map[string]string{"one": "wʌn", "twelve": "twɛlv"}}
	got, err := p.Convert("1 12 1")
	if err != nil || got != "wʌn twɛlv wʌn" {
		t.Fatalf("pronunciation %q, error %v", got, err)
	}
	if _, err := p.Convert("18446744073709551616"); err == nil {
		t.Fatal("out of range number accepted")
	}
}
