// Package rpc is the wire between the thin front ends (CLI, MCP server) and
// the daemon: one JSON request and one JSON response per line over a unix
// socket in the user's runtime dir.
package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type Request struct {
	Cmd  string         `json:"cmd"`
	Args map[string]any `json:"args,omitempty"`
}

type Image struct {
	Path   string `json:"path"`
	Mime   string `json:"mime"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	ShotID string `json:"shot,omitempty"`
	Note   string `json:"note,omitempty"`
}

type Response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Text   string  `json:"text,omitempty"`
	Data   any     `json:"data,omitempty"`
	Images []Image `json:"images,omitempty"`
}

// Dir is where the socket and pid file live.
func Dir() string {
	if d := os.Getenv("PC_RUNTIME"); d != "" {
		return d
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(rt, "pc")
}

func SocketPath() string { return filepath.Join(Dir(), "pc.sock") }

// Call sends one request, starting the daemon if it is not running.
func Call(req Request, timeout time.Duration) (*Response, error) {
	conn, err := dial()
	if err != nil {
		if err := Start(); err != nil {
			return nil, err
		}
		conn, err = dial()
		if err != nil {
			return nil, fmt.Errorf("daemon started but its socket is unreachable: %w", err)
		}
	}
	defer conn.Close()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, fmt.Errorf("pc daemon did not answer %q within %v", req.Cmd, timeout)
		}
		return nil, fmt.Errorf("pc daemon dropped the request: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func dial() (net.Conn, error) {
	return net.DialTimeout("unix", SocketPath(), time.Second)
}

// Running reports whether a daemon answers on the socket.
func Running() bool {
	c, err := dial()
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Start launches the daemon detached and waits for its socket.
func Start() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(Dir(), "daemon.log")
	log, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting pc daemon: %w", err)
	}
	go cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if Running() {
			return nil
		}
		time.Sleep(15 * time.Millisecond)
	}
	return fmt.Errorf("pc daemon did not come up; see %s", logPath)
}
