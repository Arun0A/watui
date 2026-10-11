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

var (
	canonicalEntries []Entry
	canonicalOnce    sync.Once
	popularList      = []string{
		"👍", "❤️", "😂", "🔥", "🎉", "😀", "😍", "🙏", "👏", "😊",
		"✨", "🤔", "🚀", "💯", "😎", "🥳", "😭", "👀", "🙌", "💪",
		"🤝", "💖", "💡", "⭐", "👌", "💀", "🥺", "🥰", "😴", "🫡",
	}
)

// AllList returns a deduplicated list of canonical emojis with popular ones first.
func AllList() []Entry {
	canonicalOnce.Do(func() {
		seen := make(map[string]int) // name -> index in raw
		var raw []Entry
		for _, e := range CLDREmojis {
			if idx, exists := seen[e.Name]; exists {
				if strings.ContainsRune(e.Emoji, '\uFE0F') && !strings.ContainsRune(raw[idx].Emoji, '\uFE0F') {
					raw[idx] = e
				}
				continue
			}
			seen[e.Name] = len(raw)
			raw = append(raw, e)
		}

		byEmoji := make(map[string]Entry, len(raw))
		for _, e := range raw {
			byEmoji[e.Emoji] = e
		}

		inPopular := make(map[string]bool)
		for _, pop := range popularList {
			if e, ok := byEmoji[pop]; ok {
				canonicalEntries = append(canonicalEntries, e)
				inPopular[e.Name] = true
			}
		}

		for _, e := range raw {
			if !inPopular[e.Name] {
				canonicalEntries = append(canonicalEntries, e)
			}
		}
	})
	return canonicalEntries
}

// Filter searches emojis by their CLDR name (case-insensitive), matching keywords.
func Filter(query string) []Entry {
	all := AllList()
	query = strings.TrimSpace(strings.ToLower(query))
	query = strings.TrimPrefix(query, ":")
	query = strings.TrimSuffix(query, ":")
	query = strings.TrimPrefix(query, "!")
	if query == "" {
		return all
	}

	words := strings.Fields(query)
	var exactMatches []Entry
	var prefixMatches []Entry
	var wordMatches []Entry
	var containMatches []Entry

	for _, e := range all {
		lowerName := strings.ToLower(e.Name)
		if lowerName == query {
			exactMatches = append(exactMatches, e)
			continue
		}
		if strings.HasPrefix(lowerName, query) {
			prefixMatches = append(prefixMatches, e)
			continue
		}
		// Check word-boundary matches
		if strings.Contains(" "+lowerName+" ", " "+query+" ") {
			wordMatches = append(wordMatches, e)
			continue
		}
		allWords := true
		for _, w := range words {
			if !strings.Contains(lowerName, w) {
				allWords = false
				break
			}
		}
		if allWords {
			containMatches = append(containMatches, e)
		}
	}

	res := make([]Entry, 0, len(exactMatches)+len(prefixMatches)+len(wordMatches)+len(containMatches))
	res = append(res, exactMatches...)
	res = append(res, prefixMatches...)
	res = append(res, wordMatches...)
	res = append(res, containMatches...)
	return res
}
