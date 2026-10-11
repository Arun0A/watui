package emoji

import (
	"strings"
	"sync"
)

type trieNode struct {
	children map[rune]*trieNode
	name     string
}

var (
	rootNode *trieNode
	initOnce sync.Once
)

func initTrie() {
	initOnce.Do(func() {
		rootNode = &trieNode{children: make(map[rune]*trieNode)}
		for _, entry := range CLDREmojis {
			curr := rootNode
			for _, r := range entry.Emoji {
				next, ok := curr.children[r]
				if !ok {
					next = &trieNode{children: make(map[rune]*trieNode)}
					curr.children[r] = next
				}
				curr = next
			}
			curr.name = entry.Name
		}
	})
}

// ReplaceWithCLDR replaces all emoji occurrences in text with their official CLDR short name,
// formatted as ":<cldr name>:", e.g. "👍" -> ":thumbs up:", "😀" -> ":grinning face:".
func ReplaceWithCLDR(text string) string {
	if text == "" {
		return ""
	}
	initTrie()

	runes := []rune(text)
	var sb strings.Builder
	sb.Grow(len(text) + 16)

	i := 0
	for i < len(runes) {
		curr := rootNode
		lastMatchLen := 0
		var lastMatchName string

		for j := i; j < len(runes); j++ {
			next, ok := curr.children[runes[j]]
			if !ok {
				break
			}
			curr = next
			if curr.name != "" {
				lastMatchLen = j - i + 1
				lastMatchName = curr.name
			}
		}

		if lastMatchLen > 0 {
			sb.WriteString(":")
			sb.WriteString(lastMatchName)
			sb.WriteString(":")
			i += lastMatchLen
		} else {
			sb.WriteRune(runes[i])
			i++
		}
	}
	return sb.String()
}

// FormatText formats text according to the display_emoji configuration.
// If mode is "text" (case-insensitive), emojis are replaced with their CLDR short names.
// Otherwise ("icon" or default), the text is returned as-is.
func FormatText(text string, mode string) string {
	if !strings.EqualFold(strings.TrimSpace(mode), "text") {
		return text
	}
	return ReplaceWithCLDR(text)
}
