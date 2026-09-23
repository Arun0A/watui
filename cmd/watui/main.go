package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watui/internal/domain"
	"watui/internal/ui"
	"watui/internal/whatsapp"
)

func main() {
	dbPath := flag.String("db", "watui.db", "Path to SQLite session and cache database")
	logLevel := flag.String("log", "WARN", "Protocol log level (DEBUG, INFO, WARN, ERROR)")
	cliMode := flag.Bool("cli", false, "Run in headless CLI mode instead of interactive TUI")
	jsonOutput := flag.Bool("json", false, "Print extracted messages as raw JSON (used with -cli)")
	flag.Parse()

	// 1. Set up context and termination signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// 2. Initialize WhatsApp Adapter (Encapsulated Protocol Layer)
	adapter, err := whatsapp.NewAdapter(ctx, whatsapp.Config{
		DBPath:   *dbPath,
		LogLevel: *logLevel,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing adapter: %v\n", err)
		os.Exit(1)
	}

	// 3. Pairing Check: if not logged in, display QR code first
	if !adapter.IsLoggedIn() {
		fmt.Println("───────────────────────────────────────────────────────")
		fmt.Println(" watui - Native WhatsApp TUI (First-time Pairing)")
		fmt.Println("───────────────────────────────────────────────────────")
		fmt.Println("[*] No existing session found in", *dbPath)
		fmt.Println("[*] Generating pairing QR code...")

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

		err = adapter.Connect(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
			os.Exit(1)
		}

		select {
		case <-connectedChan:
			fmt.Println("[*] Pairing successful! Launching TUI...")
			time.Sleep(1 * time.Second)
		case <-sigChan:
			fmt.Println("\n[*] Pairing aborted.")
			adapter.Disconnect()
			return
		}
	} else {
		// Session exists, connect in background
		err = adapter.Connect(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
			os.Exit(1)
		}
	}

	// 4. Run Mode: CLI Stream vs Interactive TUI
	if *cliMode {
		fmt.Println("───────────────────────────────────────────────────────")
		fmt.Println(" watui - Stream CLI Mode (Listening for messages)")
		fmt.Println("───────────────────────────────────────────────────────")

		adapter.OnMessage(func(msg domain.Message) {
			fmt.Println("\n================ [ NEW MESSAGE RECEIVED ] ================")
			if *jsonOutput {
				data, _ := json.MarshalIndent(msg, "", "  ")
				fmt.Println(string(data))
			} else {
				fmt.Printf(" Message ID : %s\n", msg.ID)
				fmt.Printf(" Chat ID    : %s\n", msg.ChatID)
				fmt.Printf(" Sender     : %s (%s)\n", msg.SenderName, msg.Sender)
				fmt.Printf(" Timestamp  : %s\n", msg.Timestamp.Format("2006-01-02 15:04:05"))
				fmt.Printf(" Type       : %s\n", msg.Type)
				fmt.Printf(" From Me    : %t\n", msg.IsFromMe)
				fmt.Printf(" Content    : %s\n", msg.Body)
			}
			fmt.Println("==========================================================")
		})

		<-sigChan
		fmt.Println("\n[*] Shutting down...")
		adapter.Disconnect()
		return
	}

	// 5. Launch Minimal Unread TUI
	p := tea.NewProgram(
		ui.NewModel(ctx, adapter),
		tea.WithAltScreen(),
	)

	go func() {
		<-sigChan
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}

	adapter.Disconnect()
}
