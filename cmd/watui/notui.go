package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"watui/internal/domain"
	"watui/internal/ui"
)

// normalizeJID formats a phone number or partial JID into a valid WhatsApp JID.
func normalizeJID(input string) string {
	target := strings.TrimSpace(input)
	if strings.Contains(target, "@") {
		return target
	}
	var sb strings.Builder
	for _, r := range target {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	cleaned := sb.String()
	if cleaned != "" {
		return cleaned + "@s.whatsapp.net"
	}
	return target
}

// readMessageBody extracts message text from command line arguments or stdin.
func readMessageBody(args []string, stdin io.Reader) (string, error) {
	if len(args) >= 2 {
		if args[1] == "-" {
			// Read from stdin
			bytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", fmt.Errorf("failed to read from stdin: %w", err)
			}
			return strings.TrimSpace(string(bytes)), nil
		}
		return strings.TrimSpace(strings.Join(args[1:], " ")), nil
	}

	// If only JID is provided, check if stdin has piped data
	if f, ok := stdin.(*os.File); ok {
		stat, err := f.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			bytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", fmt.Errorf("failed to read from stdin: %w", err)
			}
			return strings.TrimSpace(string(bytes)), nil
		}
	}

	return "", nil
}

// executeNoTuiSend connects and dispatches a message via CLI without launching the TUI.
func executeNoTuiSend(
	ctx context.Context,
	adapter domain.WhatsAppAdapter,
	isRemote bool,
	targetJID string,
	messageText string,
	jsonOutput bool,
	stdout io.Writer,
	stderr io.Writer,
) error {
	targetJID = normalizeJID(targetJID)
	if targetJID == "" {
		err := fmt.Errorf("target JID cannot be empty")
		outputError(stderr, err.Error(), jsonOutput)
		return err
	}
	if strings.TrimSpace(messageText) == "" {
		err := fmt.Errorf("message text cannot be empty")
		outputError(stderr, err.Error(), jsonOutput)
		return err
	}

	// If running standalone (not connected to daemon), ensure logged in and connected
	if !isRemote {
		if !adapter.IsLoggedIn() {
			err := fmt.Errorf("WhatsApp session not paired. Run 'watui' or 'watui -d start' first to pair via QR code")
			outputError(stderr, err.Error(), jsonOutput)
			return err
		}

		connectCtx, cancelConnect := context.WithTimeout(ctx, 20*time.Second)
		defer cancelConnect()

		connectedChan := make(chan struct{})
		adapter.OnStatus(func(status domain.ConnectionStatus) {
			if status == domain.StatusConnected {
				select {
				case <-connectedChan:
				default:
					close(connectedChan)
				}
			}
		})

		if err := adapter.Connect(connectCtx); err != nil {
			outputError(stderr, fmt.Sprintf("Failed to connect to WhatsApp: %v", err), jsonOutput)
			return err
		}

		select {
		case <-connectedChan:
		case <-connectCtx.Done():
			err := fmt.Errorf("connection timed out waiting for WhatsApp (20s)")
			outputError(stderr, err.Error(), jsonOutput)
			return err
		}
		defer adapter.Disconnect()
	}

	// Send message (attachment or text)
	var sentMsg domain.Message
	var sendErr error

	if strings.HasPrefix(messageText, "file://") {
		filePath, caption := ui.ParseFileURI(messageText)
		sentMsg, sendErr = adapter.SendFileMessage(ctx, targetJID, filePath, caption)
	} else {
		sentMsg, sendErr = adapter.SendTextMessage(ctx, targetJID, messageText)
	}

	if sendErr != nil {
		outputError(stderr, fmt.Sprintf("Failed to send message: %v", sendErr), jsonOutput)
		return sendErr
	}

	if jsonOutput {
		out := map[string]interface{}{
			"status":    "success",
			"id":        sentMsg.ID,
			"chat_id":   sentMsg.ChatID,
			"timestamp": sentMsg.Timestamp.Format(time.RFC3339),
			"type":      string(sentMsg.Type),
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(data))
	} else {
		fmt.Fprintf(stdout, "Message sent successfully (ID: %s, To: %s)\n", sentMsg.ID, targetJID)
	}

	return nil
}

func outputError(w io.Writer, msg string, jsonOutput bool) {
	if jsonOutput {
		out := map[string]interface{}{
			"status": "error",
			"error":  msg,
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(w, string(data))
	} else {
		fmt.Fprintf(w, "Error: %s\n", msg)
	}
}
