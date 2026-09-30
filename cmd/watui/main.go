package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watui/internal/config"
	"watui/internal/daemon"
	"watui/internal/domain"
	"watui/internal/notify"
	"watui/internal/ui"
	"watui/internal/whatsapp"
)

func main() {
	defaultDB := config.DefaultDBPath()
	dbPath := flag.String("db", "", "Path to SQLite session and cache database (default: "+defaultDB+")")
	logFile := flag.String("log-file", "", "File to write protocol and network logs to (disabled by default)")
	logLevel := flag.String("log", "WARN", "Protocol log level (DEBUG, INFO, WARN, ERROR)")
	cliMode := flag.Bool("cli", false, "Run in headless CLI mode instead of interactive TUI")
	jsonOutput := flag.Bool("json", false, "Print extracted messages as raw JSON (used with -cli)")
	daemonMode := flag.Bool("daemon", false, "Run in background notification daemon mode (start, stop, status, restart)")
	flag.BoolVar(daemonMode, "d", false, "Run in background notification daemon mode (shorthand)")
	daemonWorker := flag.Bool("daemon-worker", false, "Internal background worker process")

	configHelp := "Path to optional YAML or JSON config file (default: ./watui.yaml"
	if runtime.GOOS != "windows" {
		configHelp += " or ~/.config/watui/config.yaml)"
	} else {
		configHelp += ")"
	}
	configFile := flag.String("config", "", configHelp)

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(out, "  -cli\n    \tRun in headless CLI mode instead of interactive TUI\n")
		fmt.Fprintf(out, "  -config string\n    \t%s\n", configHelp)
		fmt.Fprintf(out, "  -d -daemon\n    \tRun in background notification daemon mode (start, stop, status, restart)\n")
		fmt.Fprintf(out, "  -db string\n    \tPath to SQLite session and cache database (default: %s)\n", defaultDB)
		fmt.Fprintf(out, "  -json\n    \tPrint extracted messages as raw JSON (used with -cli)\n")
		fmt.Fprintf(out, "  -log string\n    \tProtocol log level (DEBUG, INFO, WARN, ERROR) (default \"WARN\")\n")
		fmt.Fprintf(out, "  -log-file string\n    \tFile to write protocol and network logs to (disabled by default)\n")
	}
	flag.Parse()

	// Load declarative configuration
	appCfg, err := config.Load(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	finalDBPath := *dbPath
	if finalDBPath == "" {
		if appCfg.GetDBPath() != "" {
			finalDBPath = appCfg.GetDBPath()
		} else {
			finalDBPath = defaultDB
		}
	}
	finalDBPath = config.ResolveDBPath(finalDBPath)

	// Determine daemon subcommand if specified
	subcommand := ""
	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "daemon":
			*daemonMode = true
			if len(args) > 1 {
				subcommand = args[1]
			} else {
				subcommand = "start"
			}
		case "start", "stop", "status", "restart":
			*daemonMode = true
			subcommand = args[0]
		}
	}
	if *daemonMode && subcommand == "" {
		if len(args) > 0 && (args[0] == "start" || args[0] == "stop" || args[0] == "status" || args[0] == "restart") {
			subcommand = args[0]
		} else {
			subcommand = "start"
		}
	}

	// 1. Worker Mode: Detached headless daemon background execution
	if *daemonWorker {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		runDaemonWorker(ctx, cancel, sigChan, finalDBPath, appCfg, *logFile, *logLevel)
		return
	}

	// 2. Daemon Management: start, stop, status, restart
	if *daemonMode {
		switch subcommand {
		case "start":
			if err := startDaemon(finalDBPath, appCfg, *configFile, *logFile); err != nil {
				fmt.Fprintf(os.Stderr, "Error starting daemon: %v\n", err)
				os.Exit(1)
			}
			return
		case "stop":
			stopDaemon(finalDBPath)
			return
		case "status":
			statusDaemon(finalDBPath, appCfg, *logFile)
			return
		case "restart":
			if err := restartDaemon(finalDBPath, appCfg, *configFile, *logFile); err != nil {
				fmt.Fprintf(os.Stderr, "Error restarting daemon: %v\n", err)
				os.Exit(1)
			}
			return
		default:
			fmt.Printf("Unknown daemon action: '%s'. Valid actions: start, stop, status, restart\n", subcommand)
			os.Exit(1)
		}
	}

	// 3. Set up context and termination signals for foreground execution
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// 4. Emacs-Style Client/Daemon Integration:
	// If the background daemon is running, attach via local IPC!
	// This makes startup instantaneous, keeps the daemon running, and silences notifications while TUI is open.
	var adapter domain.WhatsAppAdapter
	isRemote := false

	pidFile := daemon.PIDFilePath(finalDBPath)
	if running, _ := daemon.IsRunning(pidFile); running {
		remote, err := daemon.ConnectRemote(finalDBPath)
		if err == nil {
			adapter = remote
			isRemote = true
		}
	}

	if !isRemote {
		// Standalone mode: daemon is not running, initialize direct local adapter
		var err error
		adapter, err = whatsapp.NewAdapter(ctx, whatsapp.Config{
			DBPath:     finalDBPath,
			LogFile:    *logFile,
			LogLevel:   *logLevel,
			DeviceName: appCfg.GetDeviceName(),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error initializing adapter: %v\n", err)
			os.Exit(1)
		}

		// Pairing Check: if not logged in, display QR code first
		if !adapter.IsLoggedIn() {
			fmt.Println("───────────────────────────────────────────────────────")
			fmt.Println(" watui - Native WhatsApp TUI (First-time Pairing)")
			fmt.Println("───────────────────────────────────────────────────────")
			fmt.Println("[*] No existing session found in", finalDBPath)
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
				fmt.Println("[*] Pairing successful! Launching...")
				time.Sleep(500 * time.Millisecond)
			case <-sigChan:
				fmt.Println("\n[*] Pairing aborted.")
				adapter.Disconnect()
				return
			}
		}
	}

	if !*cliMode {
		// Connect asynchronously in background so TUI launches quickly and status is synchronized
		go func() {
			_ = adapter.Connect(ctx)
		}()
	}

	// 5. CLI Stream Mode
	if *cliMode {
		fmt.Println("───────────────────────────────────────────────────────")
		fmt.Println(" watui - Stream CLI Mode (Listening for messages)")
		if appCfg.SourcePath != "" {
			fmt.Printf(" Loaded configuration from %s\n", appCfg.SourcePath)
		}
		fmt.Println("───────────────────────────────────────────────────────")

		if !isRemote {
			err = adapter.Connect(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
				os.Exit(1)
			}
		}

		adapter.OnMessage(func(msg domain.Message) {
			if appCfg.IsMuted(msg.ChatID, msg.ChatName, msg.SenderName) || adapter.IsChatArchived(msg.ChatID) {
				return
			}
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
		cancel()
		adapter.Disconnect()
		return
	}

	uiModel := ui.NewModel(ctx, adapter, appCfg)
	p := tea.NewProgram(
		uiModel,
		tea.WithAltScreen(),
	)

	go func() {
		<-sigChan
		p.Quit()
	}()

	finalModel, err := p.Run()
	if uiModel != nil {
		uiModel.CleanupOnExit()
	}
	if finalM, ok := finalModel.(*ui.Model); ok && finalM != nil {
		finalM.CleanupOnExit()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}

	cancel()
	adapter.Disconnect()
}

func startDaemon(dbPath string, appCfg *config.Config, configFile, customLog string) error {
	pidFile := daemon.PIDFilePath(dbPath)
	if running, pid := daemon.IsRunning(pidFile); running {
		fmt.Printf("Status: RUNNING (PID %d)\n", pid)
		return nil
	}

	// Verify that user is paired before launching background daemon
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	adapter, err := whatsapp.NewAdapter(testCtx, whatsapp.Config{
		DBPath: dbPath,
	})
	if err != nil {
		testCancel()
		return fmt.Errorf("failed to open session: %w", err)
	}
	loggedIn := adapter.IsLoggedIn()
	adapter.Disconnect()
	testCancel()

	if !loggedIn {
		return fmt.Errorf("no active WhatsApp session found. Run 'watui' first to link your account via QR code before starting the daemon")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine executable path: %w", err)
	}

	args := []string{"--daemon-worker", "--db", dbPath}
	if configFile != "" {
		if abs, err := filepath.Abs(configFile); err == nil {
			args = append(args, "--config", abs)
		} else {
			args = append(args, "--config", configFile)
		}
	}

	cmd := exec.Command(exe, args...)

	loggingEnabled := customLog != "" || appCfg.IsDaemonLogEnabled()
	var logFileHandle *os.File
	if loggingEnabled {
		logPath := customLog
		if logPath == "" {
			logPath = appCfg.ResolveDaemonLogPath(dbPath)
		}
		logDir := filepath.Dir(logPath)
		_ = os.MkdirAll(logDir, 0755)

		var err error
		logFileHandle, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file %s: %w", logPath, err)
		}
		defer logFileHandle.Close()

		cmd.Stdout = logFileHandle
		cmd.Stderr = logFileHandle
		cmd.Args = append(cmd.Args, "--log-file", logPath)
	} else {
		devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err == nil {
			defer devNull.Close()
			cmd.Stdout = devNull
			cmd.Stderr = devNull
		}
	}
	cmd.Stdin = nil
	daemon.DetachProcess(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to spawn daemon process: %w", err)
	}

	// Record PID
	_ = daemon.WritePID(pidFile, cmd.Process.Pid)

	// Verify child process initialized
	time.Sleep(500 * time.Millisecond)
	if running, _ := daemon.IsRunning(pidFile); !running {
		if loggingEnabled {
			logPath := customLog
			if logPath == "" {
				logPath = appCfg.ResolveDaemonLogPath(dbPath)
			}
			return fmt.Errorf("daemon exited prematurely. Check logs at: %s", logPath)
		}
		return fmt.Errorf("daemon exited prematurely")
	}

	fmt.Printf("Status: RUNNING (PID %d)\n", cmd.Process.Pid)
	return nil
}

func stopDaemon(dbPath string) {
	pidFile := daemon.PIDFilePath(dbPath)
	running, pid, err := daemon.Stop(pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error stopping daemon: %v\n", err)
		os.Exit(1)
	}
	daemon.CleanupIPC(dbPath)
	if !running {
		fmt.Println("Status: STOPPED")
		return
	}
	fmt.Printf("Status: STOPPED (PID %d)\n", pid)
}

func statusDaemon(dbPath string, appCfg *config.Config, customLog string) {
	pidFile := daemon.PIDFilePath(dbPath)
	running, pid := daemon.IsRunning(pidFile)
	if running {
		fmt.Printf("Status: RUNNING (PID %d)\n", pid)
		if customLog != "" {
			fmt.Printf("Log file: %s\n", customLog)
		} else if appCfg.IsDaemonLogEnabled() {
			fmt.Printf("Log file: %s\n", appCfg.ResolveDaemonLogPath(dbPath))
		}
	} else {
		fmt.Println("Status: STOPPED")
	}
}

func restartDaemon(dbPath string, appCfg *config.Config, configFile, customLog string) error {
	pidFile := daemon.PIDFilePath(dbPath)
	if running, pid, _ := daemon.Stop(pidFile); running {
		fmt.Printf("Status: STOPPED (PID %d)\n", pid)
		time.Sleep(300 * time.Millisecond)
	}
	daemon.CleanupIPC(dbPath)
	return startDaemon(dbPath, appCfg, configFile, customLog)
}

func runDaemonWorker(ctx context.Context, cancel context.CancelFunc, sigChan chan os.Signal, finalDBPath string, appCfg *config.Config, logFile string, logLevel string) {
	pidFile := daemon.PIDFilePath(finalDBPath)
	_ = daemon.WritePID(pidFile, os.Getpid())
	defer func() {
		daemon.RemovePID(pidFile)
		daemon.CleanupIPC(finalDBPath)
	}()

	loggingEnabled := logFile != "" || appCfg.IsDaemonLogEnabled()
	if !loggingEnabled {
		log.SetOutput(io.Discard)
	} else {
		log.Printf("[watui daemon] Background worker started (PID %d)\n", os.Getpid())
		if appCfg.SourcePath != "" {
			log.Printf("[watui daemon] Config loaded from %s\n", appCfg.SourcePath)
		}
		notifCfg := appCfg.GetNotificationConfig()
		log.Printf("[watui daemon] Notifications: Enabled=%t, Banner=%t, Sound=%t\n", notifCfg.Enabled, notifCfg.Banner, notifCfg.Sound)
		if notifCfg.Sound {
			log.Printf("[watui daemon] Sound file: %s\n", appCfg.ResolveSoundPath())
		}
	}

	adapter, err := whatsapp.NewAdapter(ctx, whatsapp.Config{
		DBPath:     finalDBPath,
		LogFile:    logFile,
		LogLevel:   logLevel,
		DeviceName: appCfg.GetDeviceName(),
	})
	if err != nil {
		if loggingEnabled {
			log.Printf("[watui daemon] Failed to initialize adapter: %v\n", err)
		}
		return
	}
	defer adapter.Disconnect()

	if !adapter.IsLoggedIn() {
		if loggingEnabled {
			log.Printf("[watui daemon] No active WhatsApp session. Pair first using 'watui'.\n")
		}
		return
	}

	// Create and start IPC server so TUI clients can attach instantaneously
	ipcServer, err := daemon.NewServer(adapter, finalDBPath)
	if err != nil {
		if loggingEnabled {
			log.Printf("[watui daemon] Failed to start IPC server: %v\n", err)
		}
	} else {
		defer ipcServer.Close()
	}

	adapter.OnMessage(func(msg domain.Message) {
		if appCfg.IsMuted(msg.ChatID, msg.ChatName, msg.SenderName) || adapter.IsChatArchived(msg.ChatID) {
			return
		}
		if loggingEnabled {
			log.Printf("[watui daemon] Incoming message from %s (%s): %s\n", msg.SenderName, msg.ChatID, msg.Body)
		}
		// Silence desktop notifications and sounds while TUI client is connected!
		if ipcServer == nil || !ipcServer.HasActiveClients() {
			notify.Dispatch(appCfg, msg)
		}
	})

	err = adapter.Connect(ctx)
	if err != nil {
		if loggingEnabled {
			log.Printf("[watui daemon] Connection failed: %v\n", err)
		}
		return
	}

	if loggingEnabled {
		log.Printf("[watui daemon] Connected to WhatsApp. Listening for incoming messages.\n")
	}

	<-sigChan
	if loggingEnabled {
		log.Printf("[watui daemon] Termination signal received. Shutting down gracefully...\n")
	}
	cancel()
	if ipcServer != nil {
		ipcServer.Close()
	}
	adapter.Disconnect()
	daemon.RemovePID(pidFile)
	daemon.CleanupIPC(finalDBPath)
	if loggingEnabled {
		log.Printf("[watui daemon] Daemon worker stopped.\n")
	}
}
