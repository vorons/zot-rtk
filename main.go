// zot-rtk — zot extension that routes bash commands through rtk.
//
// Uses `rtk rewrite` (the canonical rewrite engine) to decide
// which bash commands to intercept and how to rewrite them.
// Covers all 100+ commands rtk supports (git, ls, cat, grep, cargo,
// docker, kubectl, aws, pytest, etc.).
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

func readFrame() (Frame, error) {
	var f Frame
	dec := json.NewDecoder(os.Stdin)
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	return f, nil
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

	// Fetch latest release
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

	// Only tar.gz is supported; zip extraction for Windows is TODO.
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
		// cross-device link fallback
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

// selfDir returns the directory containing this extension's own executable.
func selfDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func ensureRTK(dataDir string) string {
	// 1. Same directory as this extension binary (bundled install)
	if dir, err := selfDir(); err == nil {
		p := filepath.Join(dir, rtkBin())
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}

	// 2. Already on PATH
	if p, err := exec.LookPath(rtkBin()); err == nil {
		return p
	}

	// 3. Already downloaded in data dir
	dest := filepath.Join(dataDir, rtkBin())
	if _, err := os.Stat(dest); err == nil {
		return dest
	}

	// 4. Download
	notify("info", "Downloading latest rtk binary...")
	p, err := downloadRTK(dataDir)
	if err != nil {
		notify("error", fmt.Sprintf("rtk download failed: %v", err))
		return ""
	}
	notify("success", "rtk ready: "+p)
	return p
}

// ---------------------------------------------------------------------------
// rtk rewrite integration
// ---------------------------------------------------------------------------

// rewriteCmd uses `rtk rewrite` as the canonical decision engine.
// rtk rewrite exits 3 when no hook is installed, but stdout still
// contains the correctly rewritten command — so we always read it.
func rewriteCmd(rtkPath, command string) string {
	if rtkPath == "" || command == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, rtkPath, "rewrite", command)
	out, _ := cmd.Output() // ignore exit code; stdout is valid even on status 3
	r := strings.TrimSpace(string(out))
	if r == "" {
		return ""
	}
	return r
}

// runRTK executes a command through rtk and returns output.
func runRTK(ctx context.Context, rtkPath, command string) (string, error) {
	args := strings.Fields(command) // ponytail: naive split, no shell syntax
	cmd := exec.CommandContext(ctx, rtkPath, args...)
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
		"version":      "1.0.3",
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

		// ---- bash interception via rtk rewrite ----
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

			rewritten := rewriteCmd(rtkPath, command)
			if rewritten != "" && (strings.HasPrefix(rewritten, "rtk ") || strings.HasPrefix(rewritten, "rtk\t")) {
				// Use the full path to rtk so bash doesn't need it on PATH
				modified := rtkPath + rewritten[3:]
				emit(Frame{
					"type":         "event_intercept_response",
					"id":           msg["id"],
					"modified_args": map[string]any{"command": modified},
				})
			} else {
				emit(Frame{"type": "event_intercept_response", "id": msg["id"]})
			}

		// ---- shutdown ----
		case "shutdown":
			emit(Frame{"type": "shutdown_ack"})
			return
		}
	}
}
