# watui

[![CI](https://github.com/Arun0A/watui/actions/workflows/ci.yaml/badge.svg)](https://github.com/Arun0A/watui/actions/workflows/ci.yaml)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev)
[![Platform](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-blue)](https://github.com/Arun0A/watui/releases)

A minimal WhatsApp TUI, specifically designed to reduce dependency on WA web or desktop application.

**Philosophy**: Most of the time you do not require past conversation context visible for a reply. And you often have to keep whatsapp (either in web or desktop-app) open anticipating a message from someone, this eats up a lot of RAM of your system (which you were saving for absolutely nothing).

So WA-TUI, just shows you the unread messages with message context persisting only for your active session. Although, you can start a new chat with any contact on demand. It is however a stripped down version of WhatsApp.

### What you dont get:
- No read-receipts (be honest, do you really care?)
- Your 5 years chat history is not loaded
- You don't get to see their profile picture

idk, probably much more... but i dont really see the need for them.
However, if you really feel that you would not want to compromise on these, you should be using the official web or desktop version for such tasks.

I must repeat, WATUI is not a replacement to the official WhatsApp, it's just what you need most of the time.

### What you get:
- Media and document preview (and saving them ofc)
- View statuses of your contacts
- Read-receipts are updated realtime (if you wish to bypass, you must be smart enough)
- Attach files without leaving the terminal emulator
- Initiate a message to a WA number
- Ghost your nemesis by adding them in your config
- Desktop notification

If you are concerned about security, the db is only accessible in your machine, encrypted with a key and your machine-id (guid).

---

## Installation

Pre-built binaries are available on the [**Releases Page**](https://github.com/Arun0A/watui/releases).

### Linux

1. Download `watui-linux-amd64.tar.gz` from the latest release:
   ```bash
   tar -xzf watui-linux-amd64.tar.gz
   cd watui-linux-amd64
   ```
2. Move the binary into your `PATH`:
   ```bash
   install -m 755 watui ~/.local/bin/watui
   # (Ensure ~/.local/bin is in your $PATH)
   ```
3. *(Optional)* Set up your config:
   ```bash
   mkdir -p ~/.config/watui
   cp watui.example.yaml ~/.config/watui/config.yaml
   ```

#### Using Nix Flakes (NixOS / any distro with Nix)
```bash
# Run directly without installing:
nix run github:Arun0A/watui

# Or start a development shell:
nix develop github:Arun0A/watui
```

---

### Windows

1. Download `watui-windows-amd64-portable.zip` from the latest release.
2. Extract the zip file anywhere (e.g. `C:\Tools\watui` or your preferred location).
3. The folder contains `watui.exe` and `watui.yaml`.
4. Launch `watui.exe` from PowerShell, Command Prompt, or Windows Terminal:
   ```powershell
   .\watui.exe
   ```
   *(Optional)* To run `watui` from any terminal, add the folder containing `watui.exe` to your Windows `Path` environment variable.

---

### macOS

[I don't own a macOS machine, reporting any reviews/issues is appreciated]

1. Download `watui-darwin-arm64.tar.gz` (Apple Silicon) from the Releases page:
   ```bash
   tar -xzf watui-darwin-arm64.tar.gz
   cd watui-darwin-arm64
   ```
2. Move the binary into your `PATH`:
   ```bash
   sudo install -m 755 watui /usr/local/bin/watui
   # Or to user local bin:
   mkdir -p ~/.local/bin && cp watui ~/.local/bin/
   ```
3. *(Optional)* Setup your configuration:
   ```bash
   mkdir -p ~/.config/watui
   cp watui.example.yaml ~/.config/watui/config.yaml
   ```
4. If macOS Gatekeeper flags the binary on first launch, allow it via:
   ```bash
   xattr -d com.apple.quarantine ~/.local/bin/watui
   ```

---

## First-Time Setup & Pairing

1. Open your terminal and launch:
   ```bash
   watui
   ```
2. A QR code will appear in your terminal.
3. Open **WhatsApp** on your mobile phone:
   - Tap **Settings** (or ⋮ on Android) ➔ **Linked Devices** ➔ **Link a Device**.
4. Scan the QR code in your terminal.
5. You're in! Your session credentials are saved in your local encrypted database. Future launches connect immediately.

---

## Keybindings

[Most of them are provided as hint as you use watui.]

### Inbox View (Main Screen)
| Key | Action |
| :--- | :--- |
| <kbd>j</kbd> / <kbd>k</kbd> or <kbd>↓</kbd> / <kbd>↑</kbd> | Navigate unread / pinned conversations |
| <kbd>Enter</kbd> | Open selected conversation |
| <kbd>a</kbd> | Toggle Archived chats section (press again or <kbd>Esc</kbd> to return) |
| <kbd>Shift</kbd>+<kbd>a</kbd> / <kbd>A</kbd> | Archive / Unarchive selected conversation |
| <kbd>n</kbd> | Start a new chat (search all contacts & groups) |
| <kbd>d</kbd> / <kbd>r</kbd> | Dismiss selected unread conversation |
| <kbd>?</kbd> | Toggle keybind hints |
| <kbd>q</kbd> / <kbd>Ctrl</kbd>+<kbd>c</kbd> | Quit watui |

### Chat View
| Key | Action |
| :--- | :--- |
| `Type text` + <kbd>Enter</kbd> | Send message |
| <kbd>Esc</kbd> | Back to inbox |
| <kbd>PgUp</kbd> / <kbd>PgDn</kbd> | Scroll conversation history up / down |
| <kbd>Alt</kbd> + <kbd>Enter</kbd> / <kbd>C-j</kbd> | Multi-line message |
| <kbd>Alt</kbd> + <kbd>p</kbd> | Preview selected media attachment |
| <kbd>Alt</kbd> + <kbd>x</kbd> | Stop media / audio playback immediately |
| <kbd>Alt</kbd> + <kbd>j</kbd> / <kbd>k</kbd> | Cycle targeted media in the message thread |
| <kbd>Alt</kbd> + <kbd>f</kbd> | Open file picker to attach and send a file |
| <kbd>Ctrl</kbd> + <kbd>u</kbd> | Clear current input line |

---

## Background Notification Daemon


```bash
watui -d                  # Start daemon in background (or: watui daemon start)
watui daemon status       # Check daemon status (or: watui -d status)
watui daemon stop         # Stop the background daemon (or: watui -d stop)
watui daemon restart      # Restart daemon (or: watui -d restart)
```

- When the daemon is running, launching `watui` attaches via local IPC for an instantaneous startup.
- If the daemon is not running, `watui` automatically runs standalone as normal.
- Consumes ~15-20 MB of RAM (50x less than WhatsApp Web or Desktop).
- Notification are configurable in the config file.

### Contact Picker (New Chat)
| Key | Action |
| :--- | :--- |
| `Type query` | Real-time fuzzy filter contacts and groups |
| <kbd>↓</kbd> / <kbd>↑</kbd> | Select contact / group |
| <kbd>Enter</kbd> | Open chat window |
| <kbd>Esc</kbd> | Cancel and return to inbox |

---

## Configuration (`watui.yaml`)

`watui` automatically checks for a config file at:
1. `./watui.yaml` *(Current working directory)*
2. `~/.config/watui/config.yaml` *(Linux / macOS)*
3. `%APPDATA%\watui\config.yaml` *(Windows)*
4. Or pass explicitly: `watui -config /path/to/custom.yaml`

See [**`watui.example.yaml`**](watui.example.yaml) for a full documented template:

```yaml
# 1. Pinned chats
pin:
  - "91XXXXXX9-15XXXXXX2@g.us" # JID or name supported

# 2. Muted chats (hidden from unread inbox)
mute:
  - "*@newsletter"         # Mute all WhatsApp Channels
  - "status@broadcast"     # Mute WhatsApp Status updates

# 3. Media Preview Commands (defaults to OS / MIME default: xdg-open on Linux, open on macOS, default app on Windows)
preview:
  image: "feh -."        # Optional override (e.g. feh, mpv --loop=inf)
  video: "mpv"           # Optional override
  document: "xdg-open"   # General document fallback
  extensions:            
    pdf: "sioyek"
    txt: "nvim"
    log: "less"

# 4. File Picker Command (Alt+F)
# Supported: yazi, ranger, lf, nnn, fzf, zenity, kdialog
file_picker: "yazi"

# 5. Custom Companion Device Name
device_name: "WA-TUI"

# 6. Database Directory or Path (Optional)
# db_dir: "~/.local/share/watui"      # Stores watui.db inside this folder
# db_path: "~/.local/share/watui/watui.db"

# 7. Include / Alternate Config File (Optional)
# config_file: "~/.config/watui/config.yaml"

# 8. Notifications (Banners & Sound Effects)
# Disabled by default. Muted chats never trigger alerts.
notifications:
  enabled: true          # Master toggle: set to true to enable alerts
  banner: true            # Desktop notification banner (notify-send on Linux, toast on Windows, macOS)
  sound: true             # Audio chime on incoming message
  # sound_path: ""          # Custom audio file path (defaults to whatsapp_notification.mp3)
```

---

## Building from Source

### Prerequisites
- **Go 1.26+** (or Go 1.24+)
- **GCC / Clang** (CGO is required by the SQLCipher SQLite engine)

```bash
# 1. Clone repository
git clone https://github.com/Arun0A/watui.git
cd watui

# 2. Build executable
go build -o watui ./cmd/watui

# 3. Run
./watui
```

---

## Questions?

If you have any questions, go to the discussion panel.

If find any major issues, create an issue.

If you want to contribute, create an issue, and then create a PR (resolving the issue).

If you want to thank me... your welcome :) (consider giving me a star?)

---

## License

This project is licensed under the [MIT License](LICENSE).
