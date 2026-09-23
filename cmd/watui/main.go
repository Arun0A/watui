package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"watui/internal/domain"
	"watui/internal/whatsapp"
)

func main() {
	dbPath := flag.String("db", "watui.db", "Path to SQLite session and cache database")
	logLevel := flag.String("log", "WARN", "Protocol log level (DEBUG, INFO, WARN, ERROR)")
	jsonOutput := flag.Bool("json", false, "Print extracted message as raw JSON")
	flag.Parse()

	fmt.Println("───────────────────────────────────────────────────────")
	fmt.Println(" watui - Native WhatsApp TUI / Engine (Milestone 1)")
	fmt.Println("───────────────────────────────────────────────────────")

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

	// 3. Register Domain Message Handler (Semantic Representation)
	adapter.OnMessage(func(msg domain.Message) {
		fmt.Println("\n================ [ NEW MESSAGE RECEIVED ] ================")
		if *jsonOutput {
			data, _ := json.MarshalIndent(msg, "", "  ")
			fmt.Println(string(data))
		} else {
			fmt.Printf(" Message ID : %s\n", msg.ID)
			fmt.Printf(" Chat ID    : %s\n", msg.ChatID)
			fmt.Printf(" Sender     : %s\n", msg.Sender)
			fmt.Printf(" Timestamp  : %s\n", msg.Timestamp.Format("2006-01-02 15:04:05"))
			fmt.Printf(" Type       : %s\n", msg.Type)
			fmt.Printf(" From Me    : %t\n", msg.IsFromMe)
			fmt.Printf(" Content    : %s\n", msg.Body)
		}
		fmt.Println("==========================================================")
	})

	// 4. Register Connection Status Handler
	adapter.OnStatus(func(status domain.ConnectionStatus) {
		switch status {
		case domain.StatusConnected:
			fmt.Println("\n[*] Status: Connected and listening for messages...")
			fmt.Println("[*] Send a WhatsApp message to this account or any group to test.")
			fmt.Println("[*] Press Ctrl+C to disconnect cleanly.")
		case domain.StatusWaitingQR:
			fmt.Println("[*] Status: Awaiting device pairing scan...")
		case domain.StatusConnecting:
			fmt.Println("[*] Status: Connecting to WhatsApp servers...")
		case domain.StatusDisconnected:
			fmt.Println("[*] Status: Disconnected.")
		case domain.StatusLoggedOut:
			fmt.Println("[!] Status: Session logged out from phone.")
		}
	})

	// 5. Connect and authenticate
	if !adapter.IsLoggedIn() {
		fmt.Println("[*] No existing session found in", *dbPath)
		fmt.Println("[*] Generating pairing code...")
	} else {
		fmt.Println("[*] Found existing session in", *dbPath)
		fmt.Println("[*] Resuming connection...")
	}

	err = adapter.Connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
		os.Exit(1)
	}

	// Wait for shutdown signal
	<-sigChan
	fmt.Println("\n[*] Shutting down...")
	adapter.Disconnect()
	fmt.Println("[*] Goodbye.")
}
