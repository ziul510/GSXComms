package ipa

import "testing"

func TestNumbersWithSharedDigits(t *testing.T) {
	// Use a dictionary fixture to check that shared digits stay in separate numbers
	p := Phonemizer{Dictionary: map[string]string{"one": "wʌn", "twelve": "twɛlv"}}
	// Put the single digit on both sides to catch replacements inside twelve.
	got, err := p.Convert("1 12 1")
	if err != nil || got != "wʌn twɛlv wʌn" {
		t.Fatalf("pronunciation %q, error %v", got, err)
	}
	// Use one past the largest uint64 to catch overflow before phonemization.
	if _, err := p.Convert("18446744073709551616"); err == nil {
		t.Fatal("out of range number accepted")
	}
}
