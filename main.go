// zot-rtk — zot extension that routes bash commands through rtk.
//
// Whitelist-based rewrite: prefixes known commands with `rtk` when
// the model forgets. Splits command chains (&&, ||, ;, |) and rewrites
// each segment independently. No dependency on `rtk rewrite`.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// JSON wire protocol
// ---------------------------------------------------------------------------

type Frame map[string]any

func emit(f Frame) { json.NewEncoder(os.Stdout).Encode(f) }

func notify(level, msg string) {
	emit(Frame{"type": "notify", "level": level, "message": msg})
}

var (
	stdinDec  = json.NewDecoder(os.Stdin)
	rtkOnPATH bool // rtk found on PATH or symlinked there
)

func readFrame() (Frame, error) {
	var f Frame
	if err := stdinDec.Decode(&f); err != nil {
		return nil, err
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// rtk command whitelist — all commands rtk supports
// https://github.com/rtk-ai/rtk
// ---------------------------------------------------------------------------

// unsupportedFindFlags lists native find flags that rtk find cannot handle.
// When detected, the command is passed to native find instead of rtk find.
// Source: https://github.com/rtk-ai/rtk/blob/develop/src/cmds/system/find_cmd.rs
var unsupportedFindFlags = []string{
	// Logical operators
	"-not", "!", "-or", "-o", "-and", "-a",

	// Actions
	"-exec", "-execdir", "-ok", "-okdir",
	"-delete", "-print0", "-printf", "-fprint", "-fprint0", "-fprintf",
	"-ls", "-fls",

	// File size, permissions, links
	"-size",
	"-perm",
	"-links", "-lname", "-ilname", "-inum", "-samefile",

	// Time predicates
	"-newer", "-anewer", "-cnewer",
	"-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin",
	"-used",

	// Empty / exists
	"-empty",

	// Ownership
	"-gid", "-uid", "-group", "-nogroup", "-nouser",

	// Filesystem
	"-fstype",

	// Regex / path matching
	"-regex", "-iregex",

	// Prune
	"-prune",

	// Options that rtk silently ignores
	"-L", "-follow", "-depth", "-ignore_readdir_race", "-regextype",
	"-mindepth",
}

// hasUnsupportedFindFlags tokenises cmd (respecting quotes) and checks whether
// any unsupported find flag appears as a standalone token.
func hasUnsupportedFindFlags(cmd string) bool {
	// Quick bail-out: none of the flags appear as substrings at all.
	// We look for '-' prefix to avoid O(n*m) scan on every command.
	if !strings.Contains(cmd, "-") && !strings.Contains(cmd, "!") {
		return false
	}

	// Tokenise respecting single and double quotes.
	// This avoids false positives when a flag appears inside a value or a string.
	var quote byte
	tokens := strings.Fields(cmd)
	// Fields splits on whitespace, which already handles basic cases.
	// We only need to skip tokens that are inside quotes — but since Fields
	// splits on whitespace even inside quotes, we reconstruct quoted regions.
	// Simpler: just check each field after stripping surrounding quotes.
	for _, tok := range tokens {
		// Track whether we're inside a quoted region that spans multiple fields
		if quote != 0 {
			// End of quoted region
			if len(tok) > 0 && tok[len(tok)-1] == quote {
				quote = 0
			}
			continue
		}

		stripped := tok
		if len(stripped) > 0 && (stripped[0] == '"' || stripped[0] == '\'') {
			q := stripped[0]
			if len(stripped) > 1 && stripped[len(stripped)-1] == q {
				// Fully quoted token — skip it entirely
				continue
			}
			// Starts quote but doesn't end it — spanning multiple fields
			quote = q
			continue
		}

		for _, f := range unsupportedFindFlags {
			if stripped == f {
				return true
			}
		}
	}
	return false
}

var rtkCommands = map[string]bool{
	// Files
	"ls":    true,
	"tree":  true,
	"cat":   true,
	"head":  true,
	"tail":  true,
	"read":  true,
	"find":  true,
	"grep":  true,
	"rg":    true,
	"diff":  true,
	"wc":    true,
	"smart": true,

	// Git
	"git": true,

	// GitHub CLI
	"gh": true,

	// Test runners
	"jest":        true,
	"vitest":      true,
	"playwright":  true,
	"pytest":      true,
	"go":          true,
	"rake":        true,
	"rspec":       true,
	"cargo":       true,
	"ruff":        true,
	"rustc":       true,
	"test":        true,
	"golangci-lint": true,

	// Build & lint
	"lint":     true,
	"eslint":   true,
	"prettier": true,
	"tsc":      true,
	"next":     true,
	"biome":    true,
	"rubocop":  true,

	// Package managers
	"pnpm":   true,
	"npm":    true,
	"npx":    true,
	"yarn":   true,
	"bun":    true,
	"pip":    true,
	"uv":     true,
	"bundle": true,
	"prisma": true,
	"dotnet": true,

	// AWS
	"aws": true,

	// Containers
	"docker":   true,
	"kubectl":  true,
	"oc":       true,

	// Infrastructure as Code
	"pulumi": true,

	// Data & misc
	"psql":   true,
	"curl":   true,
	"wget":   true,
	"jq":     true,
	"tmux":   true,
	"screen": true,
	"ssh":    true,
	"tar":    true,
	"zip":    true,
	"unzip":  true,
	"make":   true,
	"cmake":  true,
	"meson":  true,
	"ninja":  true,
	"just":   true,
	"task":   true,
}

// ---------------------------------------------------------------------------
// Command chain splitting and rewriting
// ---------------------------------------------------------------------------

var chainOps = map[string]bool{
	"&&": true,
	"||": true,
	";":  true,
	"|":  true,
}

// splitChain splits a command line at top-level shell operators (&&, ||, ;, |),
// keeping operators as their own tokens. Operators inside quotes are ignored.
// Returns nil on unbalanced quotes (caller should skip rewriting).
func splitChain(command string) []string {
	var out []string
	var buf strings.Builder
	var quote byte // 0 = not in quotes

	for i := 0; i < len(command); i++ {
		c := command[i]

		if quote != 0 {
			buf.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}

		if c == '\'' || c == '"' {
			quote = c
			buf.WriteByte(c)
			continue
		}

		// Two-char operators: &&, ||
		if i+1 < len(command) {
			pair := command[i : i+2]
			if pair == "&&" || pair == "||" {
				out = append(out, buf.String(), pair)
				buf.Reset()
				i++
				continue
			}
		}

		// Single-char operators: ; |
		if c == ';' || c == '|' {
			out = append(out, buf.String(), string(c))
			buf.Reset()
			continue
		}

		buf.WriteByte(c)
	}

	if quote != 0 {
		return nil // unbalanced quote
	}
	out = append(out, buf.String())
	return out
}

// rewriteChain prefixes each command segment with `rtk` when its first word
// is a known RTK command and it is not already prefixed. Operators are preserved.
func rewriteChain(command string) string {
	parts := splitChain(command)
	if parts == nil {
		return command // unparseable — leave untouched
	}

	changed := false
	for i, part := range parts {
		if chainOps[strings.TrimSpace(part)] {
			continue
		}

		leading := strings.TrimLeft(part, " \t")
		indent := part[:len(part)-len(leading)]
		if leading == "" {
			continue
		}

		firstWord := strings.Fields(leading)[0]
		if firstWord == "rtk" || !rtkCommands[firstWord] {
			continue
		}

		// For find, check if any unsupported native flags are present.
		// If so, skip rtk and let the command pass through to native find.
		if firstWord == "find" && hasUnsupportedFindFlags(leading) {
			continue
		}

		parts[i] = indent + "rtk " + leading
		changed = true
	}

	if !changed {
		return command
	}
	return strings.Join(parts, "")
}

// ---------------------------------------------------------------------------
// rtk binary lifecycle
// ---------------------------------------------------------------------------

func rtkBin() string {
	if runtime.GOOS == "windows" {
		return "rtk.exe"
	}
	return "rtk"
}

func targetTriple() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	switch runtime.GOOS {
	case "linux":
		if arch == "aarch64" {
			return arch + "-unknown-linux-gnu"
		}
		return arch + "-unknown-linux-musl"
	case "darwin":
		return arch + "-apple-darwin"
	case "windows":
		return arch + "-pc-windows-msvc"
	}
	return ""
}

// httpDo performs an HTTP GET with status-code checking, User-Agent, and timeout.
func httpDo(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zot-rtk/1.0")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return resp, nil
}

func downloadRTK(dataDir string) (string, error) {
	triple := targetTriple()
	if triple == "" {
		return "", fmt.Errorf("unsupported: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	url := "https://api.github.com/repos/rtk-ai/rtk/releases/latest"
	resp, err := httpDo(url)
	if err != nil {
		return "", fmt.Errorf("fetch release info: %w", err)
	}
	defer resp.Body.Close()

	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("decode release: %w", err)
	}

	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	archiveName := fmt.Sprintf("rtk-%s%s", triple, ext)

	var dlURL string
	for _, a := range rel.Assets {
		if a.Name == archiveName {
			dlURL = a.URL
			break
		}
	}
	if dlURL == "" {
		return "", fmt.Errorf("asset %q not found in release %s", archiveName, rel.Tag)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	dest := filepath.Join(dataDir, rtkBin())

	tmp, err := os.MkdirTemp("", "rtk")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	arcPath := filepath.Join(tmp, archiveName)
	out, err := os.Create(arcPath)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	r2, err := httpDo(dlURL)
	if err != nil {
		out.Close()
		os.Remove(arcPath)
		return "", fmt.Errorf("download archive: %w", err)
	}
	_, err = io.Copy(out, r2.Body)
	r2.Body.Close()
	out.Close()
	if err != nil {
		os.Remove(arcPath)
		return "", fmt.Errorf("save archive: %w", err)
	}

	if strings.HasSuffix(archiveName, ".tar.gz") {
		if err := extractTarGz(arcPath, tmp); err != nil {
			return "", fmt.Errorf("extract archive: %w", err)
		}
	}

	bin := filepath.Join(tmp, rtkBin())
	if _, err := os.Stat(bin); os.IsNotExist(err) {
		return "", fmt.Errorf("binary not found in archive")
	}

	if err := os.Rename(bin, dest); err != nil {
		if e := copyFile(dest, bin); e != nil {
			return "", fmt.Errorf("rename: %v; copy fallback: %w", err, e)
		}
	}
	if err := os.Chmod(dest, 0755); err != nil {
		return "", fmt.Errorf("chmod: %w", err)
	}
	return dest, nil
}

func extractTarGz(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(hdr.Name)
		if name == rtkBin() || name == "rtk" || name == "rtk.exe" {
			out, err := os.Create(filepath.Join(dest, name))
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			return err
		}
	}
	return fmt.Errorf("binary not found in archive")
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func selfDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func ensureRTK(dataDir string) string {
	if p, err := exec.LookPath(rtkBin()); err == nil {
		rtkOnPATH = true
		return p
	}

	if dir, err := selfDir(); err == nil {
		p := filepath.Join(dir, rtkBin())
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			enableRTKSymlink(p)
			return p
		}
	}

	dest := filepath.Join(dataDir, rtkBin())
	if _, err := os.Stat(dest); err == nil {
		enableRTKSymlink(dest)
		return dest
	}

	notify("info", "Downloading latest rtk binary...")
	p, err := downloadRTK(dataDir)
	if err != nil {
		notify("error", fmt.Sprintf("rtk download failed: %v", err))
		return ""
	}
	notify("success", "rtk ready: "+p)
	enableRTKSymlink(p)
	return p
}

func enableRTKSymlink(exe string) {
	binDir := filepath.Join(os.Getenv("HOME"), ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		notify("warn", fmt.Sprintf("mkdir %s: %v", binDir, err))
		return
	}
	link := filepath.Join(binDir, rtkBin())
	os.Remove(link)
	if err := os.Symlink(exe, link); err != nil {
		notify("warn", fmt.Sprintf("symlink %s -> %s: %v", link, exe, err))
		return
	}
	rtkOnPATH = true
}

// ---------------------------------------------------------------------------
// rtk execution
// ---------------------------------------------------------------------------

func runRTK(ctx context.Context, rtkPath, command string) (string, error) {
	// ponytail: run through sh so &&, ||, |, ; are shell operators, not
	// literal argv entries.  strings.Fields split caused e.g. `cd /path && git
	// log` to pass "&&" directly to /usr/bin/cd → "too many arguments".
	cmd := exec.CommandContext(ctx, "sh", "-c", rtkPath+" "+command)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(ee.Stderr), err
		}
		return "", err
	}
	return string(out), nil
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	emit(Frame{
		"type":         "hello",
		"name":         "zot-rtk",
		"version":      "1.2.0",
		"capabilities": []string{"commands", "tools"},
	})

	var rtkPath string
	var dataDir string

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case <-sigCh:
			return
		default:
		}

		msg, err := readFrame()
		if err != nil {
			return
		}

		switch msg["type"] {
		// ---- handshake ----
		case "hello_ack":
			dataDir, _ = msg["data_dir"].(string)
			if dataDir == "" {
				dataDir = filepath.Join(os.Getenv("HOME"), ".local", "state", "zot-rtk")
			}
			rtkPath = ensureRTK(dataDir)

			emit(Frame{"type": "register_command", "name": "rtk",
				"description": "Run a command through rtk for compact token-efficient output"})

			emit(Frame{
				"type":        "register_tool",
				"name":        "rtk",
				"description": "Run a shell command through the rtk filter (Rust Token Killer). Reduces token consumption by 60-90%% on 100+ commands. Use instead of 'bash' for compact output.",
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "Shell command to run through rtk (e.g. 'git status', 'ls -la')",
						},
					},
					"required": []string{"command"},
				},
			})

			emit(Frame{
				"type":      "subscribe",
				"events":    []string{"tool_call"},
				"intercept": []string{"tool_call"},
			})

			emit(Frame{"type": "ready"})

		// ---- /rtk slash command ----
		case "command_invoked":
			id, _ := msg["id"].(string)
			args, _ := msg["args"].(string)
			args = strings.TrimSpace(args)

			if rtkPath == "" {
				emit(Frame{"type": "command_response", "id": id,
					"action":  "display",
					"display": "rtk not available. Install: brew install rtk | cargo install --git https://github.com/rtk-ai/rtk"})
				continue
			}
			if args == "" {
				emit(Frame{"type": "command_response", "id": id,
					"action":  "display",
					"display": "Usage: /rtk <command>\n  /rtk git status\n  /rtk ls -la\n  /rtk cargo test"})
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			text, err := runRTK(ctx, rtkPath, args)
			cancel()
			if err != nil {
				emit(Frame{"type": "command_response", "id": id,
					"action":  "display",
					"display": fmt.Sprintf("rtk: %v\n%s", err, text)})
				continue
			}
			if len(text) > 50000 {
				text = text[:50000]
			}
			if text == "" {
				text = "(empty output)"
			}
			emit(Frame{"type": "command_response", "id": id,
				"action": "prompt",
				"prompt": "```\n" + text + "\n```"})

		// ---- rtk LLM tool ----
		case "tool_call":
			if name, _ := msg["name"].(string); name != "rtk" {
				continue
			}
			id, _ := msg["id"].(string)
			argsMap, _ := msg["args"].(map[string]any)
			command, _ := argsMap["command"].(string)

			if rtkPath == "" {
				emit(Frame{"type": "tool_result", "id": id,
					"is_error": true,
					"content":  []any{Frame{"type": "text", "text": "rtk binary not available"}}})
				continue
			}
			if command == "" {
				emit(Frame{"type": "tool_result", "id": id,
					"is_error": true,
					"content":  []any{Frame{"type": "text", "text": "No command provided"}}})
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			text, err := runRTK(ctx, rtkPath, command)
			cancel()
			isErr := err != nil
			if text == "" && err != nil {
				text = fmt.Sprintf("Error: %v", err)
			}
			if len(text) > 50000 {
				text = text[:50000]
			}
			emit(Frame{"type": "tool_result", "id": id,
				"is_error": isErr,
				"content":  []any{Frame{"type": "text", "text": text}}})

		// ---- bash interception — whitelist-based rewrite ----
		case "event_intercept":
			event, _ := msg["event"].(string)
			if event != "tool_call" {
				emit(Frame{"type": "event_intercept_response", "id": msg["id"]})
				continue
			}
			toolName, _ := msg["tool_name"].(string)
			if toolName != "bash" {
				emit(Frame{"type": "event_intercept_response", "id": msg["id"]})
				continue
			}
			toolArgs, _ := msg["tool_args"].(map[string]any)
			command, _ := toolArgs["command"].(string)

			if rtkPath == "" || command == "" {
				emit(Frame{"type": "event_intercept_response", "id": msg["id"]})
				continue
			}

			rewritten := rewriteChain(command)
			if rewritten == command {
				// Nothing to rewrite — pass through as-is
				emit(Frame{"type": "event_intercept_response", "id": msg["id"]})
				continue
			}

			emit(Frame{
				"type":         "event_intercept_response",
				"id":           msg["id"],
				"modified_args": map[string]any{"command": rewritten},
			})

		// ---- shutdown ----
		case "shutdown":
			emit(Frame{"type": "shutdown_ack"})
			return
		}
	}
}
