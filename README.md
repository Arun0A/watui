# watui 

[![CI](https://github.com/Arun0A/watui/actions/workflows/ci.yaml/badge.svg)](https://github.com/Arun0A/watui/actions/workflows/ci.yaml)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev)
[![Platform](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-blue)](https://github.com/Arun0A/watui/releases)

> Are you tired of your friends texting you on WhatsApp, having to open a 600MB bloated browser or Electron app just to reply "ok"?
> 
> **Then WA-TUI is for you.**

Most of the time, you don't need your entire 5-year conversation history loaded into memory just to reply to someone. **watui** adopts an **Inbox Zero** philosophy: it shows only your unread messages, with message context persisting only for your active session. You can also start a new chat with any contact on demand, preview media (images, videos, audio, documents), and send file attachments.

---

## Features

- **Lightweight & Instant:** Starts in milliseconds, uses under 30MB RAM (compared to ~800MB for WhatsApp Web).
- **Inbox Zero Philosophy:** Displays unread conversations. Open a chat, reply, and keep your inbox clean.
- **On-Demand Media Preview:** Preview images, videos, audio, and documents using your OS default applications out of the box, or customize your preferred external viewers (`mpv`, `feh`, `sioyek`, etc.).
- **File Attachments:** Launch terminal file managers (`yazi`, `ranger`, `fzf`) or GUI dialogs (`zenity`, `kdialog`) to attach and send files with <kbd>Alt</kbd>+<kbd>F</kbd>.
- **Pin & Mute Support:** Pin VIP contacts/groups and mute noisy chats via a simple declarative `watui.yaml`.

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

### Inbox View (Main Screen)
| Key | Action |
| :--- | :--- |
| <kbd>j</kbd> / <kbd>k</kbd> or <kbd>↓</kbd> / <kbd>↑</kbd> | Navigate unread / pinned conversations |
| <kbd>Enter</kbd> | Open selected conversation |
| <kbd>n</kbd> | Start a new chat (search all contacts & groups) |
| <kbd>d</kbd> | Dismiss selected unread conversation |
| <kbd>q</kbd> / <kbd>Ctrl</kbd>+<kbd>c</kbd> | Quit watui |

### Chat View
| Key | Action |
| :--- | :--- |
| `Type text` + <kbd>Enter</kbd> | Send message |
| <kbd>Esc</kbd> | Back to inbox |
| <kbd>PgUp</kbd> / <kbd>PgDn</kbd> | Scroll conversation history up / down |
| <kbd>Alt</kbd> + <kbd>p</kbd> | Preview selected media attachment |
| <kbd>Alt</kbd> + <kbd>x</kbd> | Stop media / audio playback immediately |
| <kbd>Alt</kbd> + <kbd>↑</kbd> / <kbd>↓</kbd> | Cycle targeted media in the message thread |
| <kbd>Alt</kbd> + <kbd>f</kbd> | Open file picker to attach and send a file |
| <kbd>Ctrl</kbd> + <kbd>u</kbd> | Clear current input line |

> **Tip:** In every chat window, the contact or group's exact WhatsApp **JID** is displayed in the **top-right corner** in faint grey text, making it easy to copy for pinning/muting in your config!

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
# 1. Pinned chats (always appear at top of inbox with [PIN] badge)
pin:
  - "bleh bleh"
  - "91XXXXXX9-15XXXXXX2@g.us" # JID or name supported

# 2. Muted chats (hidden from unread inbox)
mute:
  - "Crazy Scammer"
  - "Crypto Man"

# 3. Media Preview Commands (defaults to OS / MIME default: xdg-open on Linux, open on macOS, default app on Windows)
preview:
  image: "feh -."        # Optional override (e.g. feh, mpv --loop=inf)
  video: "mpv"           # Optional override
  document: "xdg-open"   # General document fallback
  extensions:            # Per-file-extension overrides (GUI or terminal viewers like nvim/less)
    pdf: "zathura"
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
```

---

## Security & Privacy

- **Machine-Bound AES-256 Encryption:** The database (`watui.db`) is encrypted using **SQLCipher**. The key is derived automatically at startup via HKDF-SHA256 from your hardware/OS identity (`/etc/machine-id` on Linux, `MachineGuid` registry on Windows, `IOPlatformUUID` on macOS) plus an owner-only salt file (`.key`).
- **Theft Resistance:** If someone copies your `watui.db` to another computer, it is completely undecryptable ciphertext without your host machine.
- **Strict File Permissions:** Files are restricted to mode `0600` (read/write only by your user account).
- **Remote Revocation:** If you ever lose access to a device, you can instantly revoke `watui` from your phone at any time:
  - **Phone ➔ WhatsApp ➔ Settings ➔ Linked Devices ➔ Tap "WA-TUI" ➔ Log out**.

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

## License

This project is licensed under the [MIT License](LICENSE).
