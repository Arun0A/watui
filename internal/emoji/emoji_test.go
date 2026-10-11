package emoji

import (
	"testing"
)

func TestReplaceWithCLDR(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "Hello 😀 world! 👍",
			expected: "Hello :grinning face: world! :thumbs up:",
		},
		{
			input:    "[Reaction: 👍]",
			expected: "[Reaction: :thumbs up:]",
		},
		{
			input:    "Group 💀-s-💀",
			expected: "Group :skull:-s-:skull:",
		},
		{
			input:    "Feeling ❤️‍🔥 right now!",
			expected: "Feeling :heart on fire: right now!",
		},
		{
			input:    "Skin tone: 👍🏽",
			expected: "Skin tone: :thumbs up (medium skin tone):",
		},
		{
			input:    "Plain text without emojis",
			expected: "Plain text without emojis",
		},
		{
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		got := ReplaceWithCLDR(tt.input)
		if got != tt.expected {
			t.Errorf("ReplaceWithCLDR(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestFormatText(t *testing.T) {
	msg := "Hello 👍"

	// Icon mode (default)
	if got := FormatText(msg, "icon"); got != msg {
		t.Errorf("FormatText(msg, 'icon') = %q; want %q", got, msg)
	}
	if got := FormatText(msg, ""); got != msg {
		t.Errorf("FormatText(msg, '') = %q; want %q", got, msg)
	}

	// Text mode
	expected := "Hello :thumbs up:"
	if got := FormatText(msg, "text"); got != expected {
		t.Errorf("FormatText(msg, 'text') = %q; want %q", got, expected)
	}
	if got := FormatText(msg, "TEXT"); got != expected {
		t.Errorf("FormatText(msg, 'TEXT') = %q; want %q", got, expected)
	}
}

func TestAllListAndFilter(t *testing.T) {
	all := AllList()
	if len(all) == 0 {
		t.Fatalf("Expected non-empty emoji list")
	}

	// First item should be a popular emoji (e.g. thumbs up or red heart)
	if all[0].Emoji != "👍" {
		t.Errorf("Expected popular emoji first, got %v (%s)", all[0].Emoji, all[0].Name)
	}

	// Filter by exact name
	resRocket := Filter("rocket")
	if len(resRocket) == 0 || resRocket[0].Emoji != "🚀" {
		t.Errorf("Expected rocket emoji as first result for 'rocket', got %+v", resRocket)
	}

	// Filter with colons
	resColon := Filter(":rocket:")
	if len(resColon) == 0 || resColon[0].Emoji != "🚀" {
		t.Errorf("Expected rocket emoji for ':rocket:', got %+v", resColon)
	}

	// Filter with words
	resThumb := Filter("thumbs up")
	if len(resThumb) == 0 || resThumb[0].Emoji != "👍" {
		t.Errorf("Expected thumbs up for 'thumbs up', got %+v", resThumb)
	}

	// Empty query returns all
	resEmpty := Filter("")
	if len(resEmpty) != len(all) {
		t.Errorf("Expected empty query to return all (%d), got %d", len(all), len(resEmpty))
	}
}
