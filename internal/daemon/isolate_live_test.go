package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pc/internal/hypr"
	"pc/internal/rpc"
)

// TestFocusIsolate finds what moves keyboard focus off the user's window:
// the user typing alone, the agent typing alone, then both (PC_LIVE=1).
func TestFocusIsolate(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	dir := t.TempDir()
	kb, err := newUinput("pc-test-user-keyboard", false)
	if err != nil {
		t.Fatal(err)
	}
	defer kb.Close()
	events := watchHyprEvents()
	logger := filepath.Join(dir, "log.sh")
	_ = os.WriteFile(logger, []byte("#!/bin/sh\nstty raw -echo\nexec cat > \"$1\"\n"), 0o755)
	orig, _ := hypr.ActiveWindow()
	open := func(app, rules string) hypr.Client {
		_, _ = hypr.Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.exec_cmd(%s, {%s}))`, hypr.LuaString("foot --app-id "+app+" "+logger+" "+filepath.Join(dir, app)), rules))
		for i := 0; i < 100; i++ {
			time.Sleep(50 * time.Millisecond)
			if c, err := findWindow(app); err == nil {
				return c
			}
		}
		t.Fatalf("%s never appeared", app)
		return hypr.Client{}
	}
	user := open("pc-user-test", `float = true, pin = true, size = "520 220", move = "460 800", no_initial_focus = true, monitor = "HDMI-A-1"`)
	agent := open("pc-agent-test2", `workspace = "name:pctest silent", no_initial_focus = true`)
	defer func() {
		_ = hypr.CloseWindow(user.Address)
		_ = hypr.CloseWindow(agent.Address)
		if orig.Address != "" {
			_ = hypr.Focus(orig.Address)
		}
		t.Logf("events:\n%s", events())
	}()
	time.Sleep(600 * time.Millisecond)
	_, _ = hypr.Eval(`hl.dispatch(hl.dsp.cursor.move({x = 700, y = 900}))`)
	_ = hypr.Focus(user.Address)
	time.Sleep(300 * time.Millisecond)
	check := func(phase string) bool {
		a, _ := hypr.ActiveWindow()
		x, y, _ := hypr.CursorPos()
		t.Logf("%s: focus %s, pointer %.0f,%.0f", phase, describeWindow(a), x, y)
		return a.Address == user.Address
	}
	userType := func(n int) {
		for i := 0; i < n; i++ {
			if a, _ := hypr.ActiveWindow(); a.Address != user.Address {
				t.Logf("user typing stopped after %d keys: focus on %s", i, describeWindow(a))
				return
			}
			c, _ := usKey(rune('a' + i%26))
			kb.key(c, 1)
			time.Sleep(40 * time.Millisecond)
			kb.key(c, 0)
			time.Sleep(30 * time.Millisecond)
		}
	}
	agentType := func(n int) {
		for i := 0; i < n; i++ {
			r, err := rpc.Call(rpc.Request{Cmd: "type", Args: map[string]any{"text": "agent text ABC!\n", "window": agent.Address}}, 10*time.Second)
			if err != nil || !r.OK {
				t.Logf("agent type: %v %v", err, r)
			}
			time.Sleep(120 * time.Millisecond)
		}
	}
	t.Logf("phase A at %s", time.Now().Format("15:04:05.000"))
	userType(30)
	if !check("A user alone") {
		_ = hypr.Focus(user.Address)
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("phase B at %s", time.Now().Format("15:04:05.000"))
	agentType(12)
	if !check("B agent alone") {
		_ = hypr.Focus(user.Address)
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("phase C at %s", time.Now().Format("15:04:05.000"))
	done := make(chan struct{})
	go func() { agentType(12); close(done) }()
	userType(30)
	<-done
	check("C both")
	u, _ := os.ReadFile(filepath.Join(dir, "pc-user-test"))
	t.Logf("user window got %q", u)
}
