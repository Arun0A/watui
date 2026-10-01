package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigLoadYAML(t *testing.T) {
	yamlContent := `
mute:
  - "Crypto Announcements"
  - "120363311130744191@g.us"
  - "+1-800-555-0199"

pin:
  - "Alice Smith"
  - "Weekend Hikers"
  - "919876543210@s.whatsapp.net"
`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "watui.yaml")
	if err := os.WriteFile(cfgFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if len(cfg.GetMuted()) != 3 {
		t.Errorf("Expected 3 muted items, got %d", len(cfg.GetMuted()))
	}
	if len(cfg.GetPinned()) != 3 {
		t.Errorf("Expected 3 pinned items, got %d", len(cfg.GetPinned()))
	}

	// Test Mute matching with JID (both with and without @g.us)
	if !cfg.IsMuted("120363311130744191@g.us", "Some Group") {
		t.Errorf("Expected JID with server to be muted")
	}
	if !cfg.IsMuted("120363311130744191", "Some Group") {
		t.Errorf("Expected JID without server to match rule with server")
	}
	if !cfg.IsMuted("random@s.whatsapp.net", "VIP Crypto Announcements Official") {
		t.Errorf("Expected chat name substring match to be muted")
	}
	if !cfg.IsMuted("18005550199@s.whatsapp.net", "Spam Caller") {
		t.Errorf("Expected phone number to be muted")
	}
	if cfg.IsMuted("999@s.whatsapp.net", "Family Member") {
		t.Errorf("Unexpected mute on regular contact")
	}

	// Test Pin matching with JID
	if !cfg.IsPinned("111@s.whatsapp.net", "Alice Smith") {
		t.Errorf("Expected Alice Smith to be pinned")
	}
	if !cfg.IsPinned("1203639999@g.us", "Weekend Hikers") {
		t.Errorf("Expected Weekend Hikers to be pinned")
	}
	if !cfg.IsPinned("919876543210@s.whatsapp.net") {
		t.Errorf("Expected JID pin to match exact JID")
	}
	if !cfg.IsPinned("919876543210:2@s.whatsapp.net") {
		t.Errorf("Expected JID pin to match device JID")
	}
	if !cfg.IsPinned("+91 98765 43210") {
		t.Errorf("Expected JID pin to match formatted phone number")
	}
	if cfg.IsPinned("222@s.whatsapp.net", "Bob Jones") {
		t.Errorf("Unexpected pin on Bob Jones")
	}
}

func TestConfigLoadJSON(t *testing.T) {
	jsonContent := `{
		"muted": ["Spam Bot", "9998887777"],
		"pinned": ["Work Group"]
	}`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "watui.json")
	if err := os.WriteFile(cfgFile, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("Failed to write temp json config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load json config: %v", err)
	}

	if !cfg.IsMuted("9998887777@s.whatsapp.net") {
		t.Errorf("Expected phone number to be muted from JSON")
	}
	if !cfg.IsPinned("some_jid@g.us", "Work Group") {
		t.Errorf("Expected Work Group to be pinned from JSON")
	}
}

func TestConfigLoadMissingDefault(t *testing.T) {
	// Loading with empty string when no default file exists should return empty config without error
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Expected nil error for empty default, got: %v", err)
	}
	if cfg == nil {
		t.Fatalf("Expected non-nil config")
	}
	if len(cfg.GetMuted()) != 0 || len(cfg.GetPinned()) != 0 {
		t.Errorf("Expected empty rules")
	}
}

func TestPreviewConfig(t *testing.T) {
	yamlContent := `
preview:
  image: "feh -."
  video: "vlc"
  audio: "custom-audio-player"
  sticker: "custom-sticker-cmd"
  document: "zathura"
  extensions:
    pdf: "sioyek"
    txt: "nvim"
  log: "less"
`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "watui.yaml")
	if err := os.WriteFile(cfgFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.GetPreviewCommand("image") != "feh -." {
		t.Errorf("Expected feh -., got %s", cfg.GetPreviewCommand("image"))
	}
	if cfg.GetPreviewCommand("video") != "vlc" {
		t.Errorf("Expected vlc, got %s", cfg.GetPreviewCommand("video"))
	}
	if cfg.GetPreviewCommand("audio") != "custom-audio-player" {
		t.Errorf("Expected custom-audio-player, got %s", cfg.GetPreviewCommand("audio"))
	}
	if cfg.GetPreviewCommand("sticker") != "custom-sticker-cmd" {
		t.Errorf("Expected custom-sticker-cmd, got %s", cfg.GetPreviewCommand("sticker"))
	}
	if cfg.GetPreviewCommand("document") != "zathura" {
		t.Errorf("Expected zathura, got %s", cfg.GetPreviewCommand("document"))
	}

	// Test per-extension command resolution
	if cfg.GetPreviewCommandForFile("/tmp/report.pdf", "document") != "sioyek" {
		t.Errorf("Expected sioyek for .pdf, got %s", cfg.GetPreviewCommandForFile("/tmp/report.pdf", "document"))
	}
	if cfg.GetPreviewCommandForFile("notes.txt", "document") != "nvim" {
		t.Errorf("Expected nvim for .txt, got %s", cfg.GetPreviewCommandForFile("notes.txt", "document"))
	}
	if cfg.GetPreviewCommandForFile("server.log", "document") != "less" {
		t.Errorf("Expected less for .log, got %s", cfg.GetPreviewCommandForFile("server.log", "document"))
	}
	// Fallback to document command for unconfigured extensions
	if cfg.GetPreviewCommandForFile("presentation.pptx", "document") != "zathura" {
		t.Errorf("Expected zathura for .pptx fallback, got %s", cfg.GetPreviewCommandForFile("presentation.pptx", "document"))
	}

	// Test default commands on empty config (should return OS default opener)
	emptyCfg := &Config{}
	expectedDefault := DefaultOpenCommand()
	for _, msgType := range []string{"image", "video", "audio", "sticker", "document", "other"} {
		if emptyCfg.GetPreviewCommand(msgType) != expectedDefault {
			t.Errorf("Expected default %s for %s, got %s", expectedDefault, msgType, emptyCfg.GetPreviewCommand(msgType))
		}
	}
	if emptyCfg.GetFilePickerCommand() != "" {
		t.Errorf("Expected empty default file picker command, got %s", emptyCfg.GetFilePickerCommand())
	}
	if emptyCfg.GetDeviceName() != "WA-TUI" {
		t.Errorf("Expected default device name 'WA-TUI', got %q", emptyCfg.GetDeviceName())
	}

	// Test custom file_picker and device_name
	customCfgYAML := `
device_name: "MyCustomTUI"
file_picker: "ranger --choosefile=/tmp/watui_test && cat /tmp/watui_test"
`
	_ = os.WriteFile(filepath.Join(tmpDir, "custom.yaml"), []byte(customCfgYAML), 0644)
	customCfg, err := Load(filepath.Join(tmpDir, "custom.yaml"))
	if err != nil {
		t.Fatalf("Failed to load custom config: %v", err)
	}
	if customCfg.GetFilePickerCommand() != "ranger --choosefile=/tmp/watui_test && cat /tmp/watui_test" {
		t.Errorf("Expected ranger file picker command, got %q", customCfg.GetFilePickerCommand())
	}
	if customCfg.GetDeviceName() != "MyCustomTUI" {
		t.Errorf("Expected device name 'MyCustomTUI', got %q", customCfg.GetDeviceName())
	}
}

func TestDefaultDBPath(t *testing.T) {
	path := DefaultDBPath()
	if path == "" {
		t.Fatalf("expected non-empty default db path")
	}

	if runtime.GOOS != "windows" {
		// On Unix (Linux / macOS), the default db path must be in XDG or ~/.local/share
		if !strings.Contains(path, "watui") {
			t.Errorf("expected standard watui path on Unix, got %s", path)
		}
	}
}

func TestExampleConfigIsUnconfigured(t *testing.T) {
	examplePath := filepath.Join("..", "..", "watui.example.yaml")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Failed to load watui.example.yaml: %v", err)
	}
	if len(cfg.GetPinned()) != 0 {
		t.Errorf("watui.example.yaml must have 0 pinned items by default, got %v", cfg.GetPinned())
	}
	if len(cfg.GetMuted()) != 0 {
		t.Errorf("watui.example.yaml must have 0 muted items by default, got %v", cfg.GetMuted())
	}
	if cfg.GetDBPath() != "" {
		t.Errorf("watui.example.yaml must have empty db path by default, got %q", cfg.GetDBPath())
	}
}

func TestConfigDBDirAndDBPath(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Test db_dir option
	cfgYAML1 := `
db_dir: "/var/watui/data"
`
	f1 := filepath.Join(tmpDir, "cfg1.yaml")
	_ = os.WriteFile(f1, []byte(cfgYAML1), 0644)
	c1, err := Load(f1)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	expected1 := filepath.Join("/var/watui/data", "watui.db")
	if c1.GetDBPath() != expected1 {
		t.Errorf("Expected %s, got %s", expected1, c1.GetDBPath())
	}

	// 2. Test db_path option
	cfgYAML2 := `
db_path: "/custom/path/my_session.db"
`
	f2 := filepath.Join(tmpDir, "cfg2.yaml")
	_ = os.WriteFile(f2, []byte(cfgYAML2), 0644)
	c2, err := Load(f2)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	expected2 := filepath.Clean("/custom/path/my_session.db")
	if c2.GetDBPath() != expected2 {
		t.Errorf("Expected %s, got %s", expected2, c2.GetDBPath())
	}

	// 3. Test db pointing to directory
	dataDir := filepath.Join(tmpDir, "mysessiondir")
	_ = os.MkdirAll(dataDir, 0700)
	cfgYAML3 := "db: " + dataDir + "\n"
	f3 := filepath.Join(tmpDir, "cfg3.yaml")
	_ = os.WriteFile(f3, []byte(cfgYAML3), 0644)
	c3, err := Load(f3)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	expected3 := filepath.Join(dataDir, "watui.db")
	if c3.GetDBPath() != expected3 {
		t.Errorf("Expected %s, got %s", expected3, c3.GetDBPath())
	}
}

func TestConfigFileChaining(t *testing.T) {
	tmpDir := t.TempDir()

	// Base config with common settings
	baseYAML := `
device_name: "BaseDevice"
pin:
  - "Alice"
db_dir: "/base/db"
`
	basePath := filepath.Join(tmpDir, "base.yaml")
	_ = os.WriteFile(basePath, []byte(baseYAML), 0644)

	// Child config that includes base.yaml and overrides device_name
	childYAML := "config_file: " + basePath + `
device_name: "ChildDevice"
mute:
  - "Bob"
`
	childPath := filepath.Join(tmpDir, "child.yaml")
	_ = os.WriteFile(childPath, []byte(childYAML), 0644)

	loaded, err := Load(childPath)
	if err != nil {
		t.Fatalf("Failed to load chained config: %v", err)
	}

	if loaded.GetDeviceName() != "ChildDevice" {
		t.Errorf("Expected ChildDevice, got %q", loaded.GetDeviceName())
	}
	if len(loaded.GetPinned()) != 1 || loaded.GetPinned()[0] != "Alice" {
		t.Errorf("Expected inherited pin 'Alice', got %v", loaded.GetPinned())
	}
	if len(loaded.GetMuted()) != 1 || loaded.GetMuted()[0] != "Bob" {
		t.Errorf("Expected child mute 'Bob', got %v", loaded.GetMuted())
	}
	if loaded.GetDBPath() != filepath.Join("/base/db", "watui.db") {
		t.Errorf("Expected inherited db_dir, got %s", loaded.GetDBPath())
	}
}

func TestResolveDBPath(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Directory with trailing slash
	p1 := ResolveDBPath("/tmp/some_dir/")
	if !strings.HasSuffix(p1, "watui.db") {
		t.Errorf("Expected trailing slash to resolve to watui.db, got %s", p1)
	}

	// 2. Existing directory
	p2 := ResolveDBPath(tmpDir)
	if p2 != filepath.Join(tmpDir, "watui.db") {
		t.Errorf("Expected existing dir to resolve to watui.db, got %s", p2)
	}

	// 3. File path
	p3 := ResolveDBPath("/tmp/custom.db")
	if p3 != filepath.Clean("/tmp/custom.db") {
		t.Errorf("Expected direct file path, got %s", p3)
	}
}

func TestDaemonConfig(t *testing.T) {
	tmpDir := t.TempDir()

	// Default when empty: logging is false
	emptyCfg, _ := Load("")
	if emptyCfg.IsDaemonLogEnabled() {
		t.Errorf("Expected daemon logging false by default")
	}

	yamlContent := `
daemon:
  log: true
  log_path: "/var/log/watui/daemon.log"
`
	f := filepath.Join(tmpDir, "daemon_cfg.yaml")
	_ = os.WriteFile(f, []byte(yamlContent), 0644)

	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if !cfg.IsDaemonLogEnabled() {
		t.Errorf("Expected daemon logging true")
	}
	if cfg.ResolveDaemonLogPath("") != "/var/log/watui/daemon.log" {
		t.Errorf("Expected custom log path, got %s", cfg.ResolveDaemonLogPath(""))
	}
}

func TestResolveSoundPath(t *testing.T) {
	tmpDir := t.TempDir()
	assetsDir := filepath.Join(tmpDir, "assets")
	_ = os.MkdirAll(assetsDir, 0755)

	soundFile := filepath.Join(assetsDir, "default.mp3")
	_ = os.WriteFile(soundFile, []byte("fake mp3"), 0644)

	cfg := &Config{
		SourcePath: filepath.Join(tmpDir, "watui.yaml"),
	}
	resolved := cfg.ResolveSoundPath()
	if resolved != soundFile {
		t.Errorf("Expected %s, got %s", soundFile, resolved)
	}
}

func TestMatchTargetWildcardAndRegex(t *testing.T) {
	// 1. Wildcard / Glob matches
	if !MatchTarget("*@newsletter", "120363143242@newsletter") {
		t.Errorf("Expected '*@newsletter' to match channel JID")
	}
	if MatchTarget("*@newsletter", "919876543210@s.whatsapp.net") {
		t.Errorf("'*@newsletter' should NOT match normal user JID")
	}
	if !MatchTarget("*@broadcast", "status@broadcast") {
		t.Errorf("Expected '*@broadcast' to match status@broadcast")
	}
	if !MatchTarget("status@*", "status@broadcast") {
		t.Errorf("Expected 'status@*' to match status@broadcast")
	}
	if !MatchTarget("status@broadcast", "status@broadcast") {
		t.Errorf("Expected exact 'status@broadcast' to match")
	}

	// 2. Direct @suffix match
	if !MatchTarget("@newsletter", "120363143242@newsletter") {
		t.Errorf("Expected '@newsletter' suffix to match")
	}
	if !MatchTarget("@broadcast", "status@broadcast") {
		t.Errorf("Expected '@broadcast' suffix to match")
	}

	// 3. Regular Expression matches
	if !MatchTarget(".*@newsletter", "120363143242@newsletter") {
		t.Errorf("Expected '.*@newsletter' regex to match")
	}
	if !MatchTarget("^status@broadcast$", "status@broadcast") {
		t.Errorf("Expected '^status@broadcast$' regex to match")
	}
	if !MatchTarget("/^120363.*@newsletter$/", "120363143242@newsletter") {
		t.Errorf("Expected slashed regex to match")
	}

	// 4. Config IsMuted integration
	cfg := &Config{
		Mute: []string{"*@newsletter", "status@broadcast"},
	}
	if !cfg.IsMuted("120363143242@newsletter", "BBC News") {
		t.Errorf("Expected channel to be muted by config")
	}
	if !cfg.IsMuted("status@broadcast", "Status") {
		t.Errorf("Expected status@broadcast to be muted by config")
	}
	if cfg.IsMuted("919876543210@s.whatsapp.net", "Best Friend") {
		t.Errorf("Expected friend to NOT be muted by config")
	}
}

func TestChatHistoryConfig(t *testing.T) {
	// Test defaults
	emptyCfg := &Config{}
	if emptyCfg.IsHistoryPersistEnabled() {
		t.Errorf("Expected history persist to be disabled by default")
	}
	if emptyCfg.GetCycleMsgCountPerChat() != 30 {
		t.Errorf("Expected default cycle msg count to be 30, got %d", emptyCfg.GetCycleMsgCountPerChat())
	}
	if emptyCfg.IsAlwaysLoadHistoryEnabled() {
		t.Errorf("Expected always load history to be false by default")
	}

	// Test kebab-case YAML
	kebabYAML := `
persist-chat-history: true
cycle-msg-count-per-chat: 25
always-load-history: true
`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "kebab.yaml")
	if err := os.WriteFile(cfgFile, []byte(kebabYAML), 0644); err != nil {
		t.Fatalf("Failed to write kebab config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load kebab config: %v", err)
	}
	if !cfg.IsHistoryPersistEnabled() {
		t.Errorf("Expected history persist enabled for kebab config")
	}
	if cfg.GetCycleMsgCountPerChat() != 25 {
		t.Errorf("Expected cycle count 25, got %d", cfg.GetCycleMsgCountPerChat())
	}
	if !cfg.IsAlwaysLoadHistoryEnabled() {
		t.Errorf("Expected always load history enabled for kebab config")
	}

	// Test snake_case YAML
	snakeYAML := `
persist_chat_history: true
cycle_msg_count_per_chat: 50
always_load_history: false
`
	cfgFileSnake := filepath.Join(tmpDir, "snake.yaml")
	if err := os.WriteFile(cfgFileSnake, []byte(snakeYAML), 0644); err != nil {
		t.Fatalf("Failed to write snake config: %v", err)
	}

	cfgSnake, err := Load(cfgFileSnake)
	if err != nil {
		t.Fatalf("Failed to load snake config: %v", err)
	}
	if !cfgSnake.IsHistoryPersistEnabled() {
		t.Errorf("Expected history persist enabled for snake config")
	}
	if cfgSnake.GetCycleMsgCountPerChat() != 50 {
		t.Errorf("Expected cycle count 50, got %d", cfgSnake.GetCycleMsgCountPerChat())
	}
	if cfgSnake.IsAlwaysLoadHistoryEnabled() {
		t.Errorf("Expected always load history disabled for snake config")
	}

	// Test nested history block
	nestedYAML := `
history:
  persist: true
  cycle_count: 15
  always_load: true
`
	cfgFileNested := filepath.Join(tmpDir, "nested.yaml")
	if err := os.WriteFile(cfgFileNested, []byte(nestedYAML), 0644); err != nil {
		t.Fatalf("Failed to write nested config: %v", err)
	}

	cfgNested, err := Load(cfgFileNested)
	if err != nil {
		t.Fatalf("Failed to load nested config: %v", err)
	}
	if !cfgNested.IsHistoryPersistEnabled() {
		t.Errorf("Expected history persist enabled for nested config")
	}
	if cfgNested.GetCycleMsgCountPerChat() != 15 {
		t.Errorf("Expected cycle count 15, got %d", cfgNested.GetCycleMsgCountPerChat())
	}
	if !cfgNested.IsAlwaysLoadHistoryEnabled() {
		t.Errorf("Expected always load history enabled for nested config")
	}
}

func TestDisableReactionsConfig(t *testing.T) {
	// Default: false
	cfgDefault := &Config{}
	if cfgDefault.IsReactionsDisabled() {
		t.Errorf("Expected reactions enabled by default, got disabled")
	}

	// Direct yaml
	tmpDir := t.TempDir()
	yamlContent := `disable_reactions: true`
	cfgFile := filepath.Join(tmpDir, "disable.yaml")
	if err := os.WriteFile(cfgFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if !cfg.IsReactionsDisabled() {
		t.Errorf("Expected reactions disabled, got enabled")
	}

	// Kebab-case alias
	kebabYAML := `disable-reactions: true`
	cfgFileKebab := filepath.Join(tmpDir, "kebab.yaml")
	if err := os.WriteFile(cfgFileKebab, []byte(kebabYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	cfgKebab, err := Load(cfgFileKebab)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if !cfgKebab.IsReactionsDisabled() {
		t.Errorf("Expected reactions disabled for kebab-case, got enabled")
	}

	// show_reactions: false alias
	showYAML := `show_reactions: false`
	cfgFileShow := filepath.Join(tmpDir, "show.yaml")
	if err := os.WriteFile(cfgFileShow, []byte(showYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	cfgShow, err := Load(cfgFileShow)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	if !cfgShow.IsReactionsDisabled() {
		t.Errorf("Expected reactions disabled for show_reactions: false, got enabled")
	}
}
