package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type keybindEntry struct {
	key  string
	desc string
}

type keybindGroup struct {
	title string
	items []keybindEntry
}

var allKeybindGroups = []keybindGroup{
	{
		title: "NAVIGATION & GLOBAL",
		items: []keybindEntry{
			{key: "Ctrl+H / Ctrl+/ (F1)", desc: "Toggle this all-keybinds menu from anywhere"},
			{key: "Esc / q", desc: "Close this menu and return to previous view"},
			{key: "Ctrl+C", desc: "Quit watui cleanly"},
			{key: "Alt+X", desc: "Stop active external media / document viewer"},
		},
	},
	{
		title: "CHATS & UNREAD LIST",
		items: []keybindEntry{
			{key: "j / Down / Ctrl+N", desc: "Move cursor down to next chat"},
			{key: "k / Up / Ctrl+P", desc: "Move cursor up to previous chat"},
			{key: "g / Home", desc: "Jump to top of chat list"},
			{key: "G / End", desc: "Jump to bottom of chat list"},
			{key: "Enter / l / Right", desc: "Open selected chat conversation"},
			{key: "r", desc: "Dismiss unread messages (mark chat as read)"},
			{key: "a", desc: "Toggle view of archived conversations"},
			{key: "Shift+A", desc: "Toggle chat archive status (archive / unarchive)"},
			{key: "Alt+P", desc: "Quick-preview latest media/doc without opening"},
			{key: "n / c", desc: "Start new chat (open contact & group picker)"},
			{key: "q", desc: "Quit application"},
		},
	},
	{
		title: "CHAT WINDOW - COMPOSING & NAVIGATION",
		items: []keybindEntry{
			{key: "Enter", desc: "Send message (or hover messages if input is empty)"},
			{key: "Shift+Enter", desc: "Insert newline in message input"},
			{key: "Esc", desc: "Exit chat to list (or cancel active reply / prompt)"},
			{key: "Ctrl+V / Alt+V", desc: "Paste image/file or text from clipboard into message box"},
			{key: "Alt+F", desc: "Open file selector to attach and send media/file"},
			{key: "Alt+P", desc: "Preview latest media / document in chat"},
			{key: "Ctrl+U", desc: "Fetch 5 older messages from history (multi-press)"},
			{key: "@ (group chat)", desc: "Show group contact mention hints"},
			{key: "Ctrl+N / Ctrl+P", desc: "Select next / previous contact mention hint"},
			{key: "Tab / Enter", desc: "Insert selected mention tag into input box"},
			{key: "Up / Down", desc: "Scroll chat history (when input box is empty)"},
			{key: "PgUp / Ctrl+Y", desc: "Scroll chat history up 5 lines"},
			{key: "PgDn / Ctrl+D / Ctrl+E", desc: "Scroll chat history down 5 lines"},
			{key: "Alt+J / Alt+K", desc: "Enter message navigation / hover mode"},
			{key: "Alt+M / Alt+Shift+J/K", desc: "Jump to next / previous media message"},
		},
	},
	{
		title: "MESSAGE HOVER MODE (ALT+J / ALT+K)",
		items: []keybindEntry{
			{key: "j / Down / Ctrl+N", desc: "Hover next message down"},
			{key: "k / Up / Ctrl+P", desc: "Hover previous message up"},
			{key: "J / K (or m / M)", desc: "Jump to next / previous media message"},
			{key: "r", desc: "Reply to hovered message (focuses input box)"},
			{key: "y", desc: "Copy hovered message text to system clipboard"},
			{key: "Ctrl+V / Alt+V", desc: "Paste from clipboard into input (focuses input)"},
			{key: "l", desc: "Open link(s) from hovered message in OS default browser"},
			{key: "p", desc: "Preview hovered media or document in viewer"},
			{key: "d", desc: "Delete hovered message for you (confirms y/N)"},
			{key: "Shift+D", desc: "Delete hovered message for everyone (if sender/admin, confirms y/N)"},
			{key: "PgUp / Ctrl+Y", desc: "Scroll chat history up 5 lines"},
			{key: "PgDn / Ctrl+D / Ctrl+E", desc: "Scroll chat history down 5 lines"},
			{key: "Ctrl+U", desc: "Fetch 5 older messages from history"},
			{key: "Esc", desc: "Return focus to message input box"},
		},
	},
	{
		title: "DOCUMENT & MEDIA ACTIONS",
		items: []keybindEntry{
			{key: "o", desc: "Open document directly in configured viewer"},
			{key: "w", desc: "Open with custom application command prompt"},
			{key: "s", desc: "Save document directly to Downloads folder"},
			{key: "y / n", desc: "Confirm / cancel download prompt"},
		},
	},
	{
		title: "CONTACT PICKER / NEW CHAT",
		items: []keybindEntry{
			{key: "Type characters", desc: "Filter contacts and groups in real time"},
			{key: "Down / Ctrl+N", desc: "Move down contact list"},
			{key: "Up / Ctrl+P", desc: "Move up contact list"},
			{key: "Enter", desc: "Select contact/group and open compose"},
			{key: "Esc", desc: "Cancel and return to chat list"},
		},
	},
}

func (m *Model) getKeybindContentLines(cw int) []string {
	groupTitleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAB387"))
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#89DCEB"))
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#CDD6F4"))

	var contentLines []string
	keyColW := 28
	if cw < 65 {
		keyColW = max(14, cw/2-4)
	}

	for gIdx, group := range allKeybindGroups {
		if gIdx > 0 {
			contentLines = append(contentLines, "")
		}
		titleBarLen := max(2, cw-len([]rune(group.title))-6)
		titleBar := strings.Repeat("─", titleBarLen)
		contentLines = append(contentLines, groupTitleStyle.Render("── "+group.title+" "+titleBar))

		for _, item := range group.items {
			keyText := item.key
			descText := item.desc

			availDesc := max(10, cw-keyColW-4)
			rDesc := []rune(descText)
			if len(rDesc) > availDesc {
				descText = string(rDesc[:availDesc-3]) + "..."
			}

			keyPart := keyStyle.Render(fmt.Sprintf("  %-*s", keyColW, keyText))
			descPart := descStyle.Render(descText)
			contentLines = append(contentLines, keyPart+"  "+descPart)
		}
	}
	return contentLines
}

func (m *Model) renderKeybindsHelpView() []string {
	cw := m.contentWidth()
	var lines []string

	leftPart := fmt.Sprintf("%s %s  %s",
		titleStyle.Render("watui"),
		selectedTitleStyle.Render("Keyboard Shortcuts"),
		statusStyle.Render("· [Esc/q] Back"),
	)
	rightPart := jidStyle.Render("[Ctrl+H / Ctrl+/] Close")
	leftW := lipgloss.Width(leftPart)
	rightW := lipgloss.Width(rightPart)

	var headerText string
	if leftW+2+rightW <= cw {
		spaces := strings.Repeat(" ", max(1, cw-leftW-rightW))
		headerText = leftPart + spaces + rightPart
	} else {
		headerText = leftPart
	}

	divider := dividerStyle.Render(strings.Repeat("─", cw))
	lines = append(lines, headerText, divider, "")

	contentLines := m.getKeybindContentLines(cw)
	totalContent := len(contentLines)

	maxH := max(4, m.height-4)
	maxVisible := max(3, maxH-5)

	maxScroll := max(0, totalContent-maxVisible)
	if m.keybindScrollOffset > maxScroll {
		m.keybindScrollOffset = maxScroll
	}
	if m.keybindScrollOffset < 0 {
		m.keybindScrollOffset = 0
	}

	start := m.keybindScrollOffset
	end := min(totalContent, start+maxVisible)
	for i := start; i < end; i++ {
		lines = append(lines, contentLines[i])
	}
	for k := end - start; k < maxVisible; k++ {
		lines = append(lines, "")
	}

	lines = append(lines, divider)

	scrollInfo := ""
	if totalContent > maxVisible {
		scrollPct := 0
		if maxScroll > 0 {
			scrollPct = int((float64(start) / float64(maxScroll)) * 100)
		}
		scrollInfo = fmt.Sprintf(" [%d%% - %d/%d]", scrollPct, start+1, totalContent)
	}
	footerText := helpStyle.Render("[Esc/q/Ctrl+H] Back · [j/k/PgUp/PgDn] Scroll") + statusStyle.Render(scrollInfo)
	lines = append(lines, footerText)

	return lines
}

func (m *Model) updateKeybindsHelp(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "ctrl+/", "ctrl+_", "ctrl+h", "f1":
		m.view = m.prevView
		return tea.ClearScreen

	case "ctrl+c":
		m.stopActiveViewer()
		m.CleanupOnExit()
		return tea.Quit

	case "j", "down", "ctrl+n":
		cw := m.contentWidth()
		total := len(m.getKeybindContentLines(cw))
		maxH := max(4, m.height-4)
		maxVisible := max(3, maxH-5)
		maxScroll := max(0, total-maxVisible)
		if m.keybindScrollOffset < maxScroll {
			m.keybindScrollOffset++
		}
		return nil

	case "k", "up", "ctrl+p":
		if m.keybindScrollOffset > 0 {
			m.keybindScrollOffset--
		}
		return nil

	case "pgdown", "ctrl+d", "ctrl+e", " ":
		cw := m.contentWidth()
		total := len(m.getKeybindContentLines(cw))
		maxH := max(4, m.height-4)
		maxVisible := max(3, maxH-5)
		maxScroll := max(0, total-maxVisible)
		m.keybindScrollOffset = min(maxScroll, m.keybindScrollOffset+5)
		return nil

	case "pgup", "ctrl+y", "ctrl+u", "b":
		m.keybindScrollOffset = max(0, m.keybindScrollOffset-5)
		return nil

	case "g", "home":
		m.keybindScrollOffset = 0
		return nil

	case "G", "end":
		cw := m.contentWidth()
		total := len(m.getKeybindContentLines(cw))
		maxH := max(4, m.height-4)
		maxVisible := max(3, maxH-5)
		m.keybindScrollOffset = max(0, total-maxVisible)
		return nil

	default:
		return nil
	}
}
