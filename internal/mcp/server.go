// Package mcp serves pc's commands as MCP tools over stdio. It is a thin
// front end: every tool call is one request to the daemon, and images come
// back inline. The tool list is generated from the command table in spec.
package mcp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"pc/internal/rpc"
	"pc/internal/spec"
)

const Instructions = `pc: see and drive this Linux desktop (Hyprland/Wayland), and know what the machine is doing.

Machine: system_state first (load, memory, disks, focus, PROBLEMS such as failed or flapping services, crashes, OOM kills). Then system_events (what changed recently), processes / process_info, services, logs, ports, system_info.

Desktop: windows lists monitors, windows and both cursors. screenshot captures the focused monitor by default and returns an image with an id (s1, s2, ...).

Acting: x,y for click/point/hover/scroll/drag are PIXELS OF THE MOST RECENT SCREENSHOT, read straight off the image. No scaling, no monitor offsets. Pass shot=<id> to use an older one, or abs=true for layout coordinates.

The user keeps working while the agent does. The agent has its own visible cursor (a tinted clone of the user's pointer with a status tag) and never takes the user's keyboard focus:
- click borrows the user's pointer for one burst and puts it back; the clicked window is not focused or raised, and becomes the agent's window.
- type_text, press_keys and paste_text go to the agent's window (window=<class/title>, else the one it last clicked or typed into), not to whatever has focus. Keys go to that app only: compositor keybinds (super+...) do not fire; use switch_workspace, focus_window, open_app for those.
- open_app windows do not take the user's focus. focus_window only changes the agent's window unless show=true.
- The agent waits while the user holds a mouse button or modifier; hover and drag wait for a still mouse and let go the moment the user moves.
- Screenshots draw the user's real pointer (labelled "you") and list the open windows, on screen and off.
- Look, act, look again. Pointer tools return an after-shot by default (look=false to skip); keyboard tools do not (look=true to get one).
- act runs a whole sequence in one call ("click 412 230; type hello --enter; wait idle").
- wait_for waits for a window, a port, the screen changing or settling. Do not sleep and guess.
- agent_cursor action=say value="..." shows a few words of status in the tag between actions.
- Never click through consent dialogs, payment confirmations, or destructive prompts on your own initiative: describe what is on screen and let the user decide.
- If the user says stop, call input_disable. Only the user can turn input back on.

Everything captured is the user's live screen: treat credentials, messages and private data as confidential.`

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var protocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// lookByDefault: pointer actions return an after-shot unless told not to.
var lookByDefault = map[string]bool{"click": true, "scroll": true, "drag": true}

// Serve runs the stdio loop until stdin closes.
func Serve(version string) error {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := os.Stdout
	var wmu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		out.Write(append(b, '\n'))
		wmu.Unlock()
	}
	// Tool calls run one at a time in arrival order: "type this, then press
	// Return" must not race. Everything else is answered straight away.
	calls := make(chan request, 64)
	done := make(chan struct{})
	go func() {
		for req := range calls {
			write(handle(req, version))
		}
		close(done)
	}()
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var req request
			if json.Unmarshal(line, &req) != nil {
				write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			} else if len(req.ID) == 0 || string(req.ID) == "null" {
				// notification: nothing to answer
			} else if req.Method == "tools/call" {
				calls <- req
			} else {
				write(handle(req, version))
			}
		}
		if err != nil {
			close(calls)
			<-done
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func handle(req request, version string) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := protocols[0]
		for _, v := range protocols {
			if v == p.ProtocolVersion {
				pv = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "pc", "version": version},
			"instructions":    Instructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools()}
	case "resources/list":
		resp.Result = map[string]any{"resources": []any{}}
	case "prompts/list":
		resp.Result = map[string]any{"prompts": []any{}}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "bad params"}
			return resp
		}
		resp.Result = call(p.Name, p.Arguments)
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp
}

func tools() []map[string]any {
	var out []map[string]any
	for _, c := range spec.Cmds {
		if c.Tool == "" {
			continue
		}
		desc := c.TDesc
		if desc == "" {
			desc = c.Help
		}
		if lookByDefault[c.Name] {
			desc += " Returns an after-shot by default (look=false to skip)."
		}
		t := map[string]any{"name": c.Tool, "description": desc, "inputSchema": c.Schema()}
		if !c.Input {
			t["annotations"] = map[string]any{"readOnlyHint": c.Name != "watch" && c.Name != "cursor" && c.Name != "clip"}
		}
		out = append(out, t)
	}
	out = append(out, map[string]any{
		"name":        "input_disable",
		"description": "Switch OFF all mouse and keyboard control at once. Use it when the user says stop, or when acting is not clearly safe. You cannot turn it back on; only the user can, with `pc input on`.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	})
	return out
}

func call(name string, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	cmd := ""
	if name == "input_disable" {
		cmd, args = "input", map[string]any{"action": "off"}
	} else if c, ok := spec.Find(name); ok && c.Tool == name {
		cmd = c.Name
		if lookByDefault[cmd] {
			if _, set := args["look"]; !set {
				args["look"] = true
			}
		}
	} else {
		return toolError("unknown tool " + name)
	}
	timeout := 45 * time.Second
	switch cmd {
	case "wait", "open", "watch", "do", "events", "logs":
		timeout = 150 * time.Second
	}
	r, err := rpc.Call(rpc.Request{Cmd: cmd, Args: args}, timeout)
	if err != nil {
		return toolError(err.Error())
	}
	var content []map[string]any
	txt := r.Text
	if !r.OK {
		if txt != "" {
			txt += "\n"
		}
		txt += "error: " + r.Error
	}
	if txt != "" {
		content = append(content, map[string]any{"type": "text", "text": txt})
	}
	for _, im := range r.Images {
		b, err := os.ReadFile(im.Path)
		if err != nil {
			content = append(content, map[string]any{"type": "text", "text": fmt.Sprintf("(image %s unreadable: %v)", im.Path, err)})
			continue
		}
		content = append(content, map[string]any{"type": "image", "mimeType": im.Mime, "data": base64.StdEncoding.EncodeToString(b)})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": "ok"})
	}
	return map[string]any{"content": content, "isError": !r.OK}
}

func toolError(msg string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + msg}}, "isError": true}
}
