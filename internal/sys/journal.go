package sys

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Unit is one systemd unit.
type Unit struct {
	Name   string `json:"unit"`
	Load   string `json:"load"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
	Desc   string `json:"description"`
	User   bool   `json:"user"`
}

func run(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// Units lists systemd units for the system and user managers. state filters
// ("failed", "running", "" for all loaded).
func Units(state string) []Unit {
	var out []Unit
	for _, user := range []bool{false, true} {
		args := []string{"list-units", "--output=json", "--no-pager"}
		if user {
			args = append([]string{"--user"}, args...)
		}
		switch state {
		case "failed":
			args = append(args, "--failed")
		case "all":
			args = append(args, "--all")
		case "":
		default:
			args = append(args, "--state="+state)
		}
		b, err := run(4*time.Second, "systemctl", args...)
		if err != nil && len(b) == 0 {
			continue
		}
		var us []Unit
		if json.Unmarshal(b, &us) != nil {
			continue
		}
		for i := range us {
			us[i].User = user
		}
		out = append(out, us...)
	}
	return out
}

// Systemd message ids (systemd/sd-messages.h).
var messageKinds = map[string]string{
	"39f53479d3a045ac8e11786248231fbf": "started",
	"7d4958e842da4a758f6c1cdc7b36dcc5": "starting",
	"de5b426a63be47a7b6ac3eaac82e2f6f": "stopping",
	"9d1aaa27d60140bd96365438aad20286": "stopped",
	"7ad2d189f7e94e70a38c781354912448": "deactivated",
	"be02cf6855d2428ba40df7e9d022f03d": "failed",
	"d9b373ed55a64feb8242e02dbe79a49c": "failed",
	"98e322203f7a4ed290d09fe03c09fe15": "exited",
	"5eb03494b6584870a536b337290809b3": "restarting",
	"fc2e22bc6ee647b6b90729ab34a250b1": "crashed",
	"d989611b15e44c9dbf31e3c81256e4ed": "oom-killed",
	"6bbd95ee977941e497c48be27c254128": "suspend",
	"8811e6df2a8e40f58a94cea26f8ebf14": "resume",
	"8d45620c1a4348dbb17410da57c60c66": "login",
	"3354939424b4456d9802ca8333ed424a": "logout",
	"ae8f7b866b0347b9af31fe1c80b127c0": "", // resource accounting: noise
	"0e4284a0caca4bfc81c0bb6786972673": "", // skipped by condition
}

// Event is something that happened on the machine, from the journal or the
// daemon's process tracking.
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject"`
	Detail  string    `json:"detail,omitempty"`
	Pid     int       `json:"pid,omitempty"`
	Source  string    `json:"source"`
	Notable bool      `json:"notable"`
}

type journalEntry struct {
	Message   json.RawMessage `json:"MESSAGE"`
	MessageID string          `json:"MESSAGE_ID"`
	Priority  string          `json:"PRIORITY"`
	Unit      string          `json:"UNIT"`
	UserUnit  string          `json:"USER_UNIT"`
	SysUnit   string          `json:"_SYSTEMD_UNIT"`
	Ident     string          `json:"SYSLOG_IDENTIFIER"`
	Comm      string          `json:"_COMM"`
	Pid       string          `json:"_PID"`
	Exe       string          `json:"COREDUMP_EXE"`
	Signal    string          `json:"COREDUMP_SIGNAL_NAME"`
	Realtime  string          `json:"__REALTIME_TIMESTAMP"`
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func (e journalEntry) text() string {
	var s string
	if json.Unmarshal(e.Message, &s) != nil {
		var raw []byte // binary messages arrive as byte arrays
		if json.Unmarshal(e.Message, &raw) == nil {
			s = string(raw)
		} else {
			s = string(e.Message)
		}
	}
	return ansi.ReplaceAllString(s, "")
}

func (e journalEntry) time() time.Time {
	us, _ := strconv.ParseInt(e.Realtime, 10, 64)
	return time.UnixMicro(us)
}

func readJournal(args ...string) []journalEntry {
	base := []string{"-o", "json", "--no-pager", "--output-fields=MESSAGE,MESSAGE_ID,PRIORITY,UNIT,USER_UNIT,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,_COMM,_PID,COREDUMP_EXE,COREDUMP_SIGNAL_NAME"}
	b, _ := run(8*time.Second, "journalctl", append(base, args...)...)
	var out []journalEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e journalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func since(d time.Duration) string {
	return "--since=-" + strconv.Itoa(int(d.Seconds())) + "s"
}

// JournalEvents pulls what changed from the journal: unit starts, stops and
// failures, restarts, crashes, OOM kills, suspend, logins, and anything logged
// at error or worse.
func JournalEvents(window time.Duration) []Event {
	entries := readJournal(since(window),
		"_COMM=systemd", "+", "SYSLOG_IDENTIFIER=systemd-coredump", "+", "SYSLOG_IDENTIFIER=systemd-oomd",
		"+", "SYSLOG_IDENTIFIER=systemd-logind", "+", "PRIORITY=0", "PRIORITY=1", "PRIORITY=2", "PRIORITY=3")
	var out []Event
	for _, e := range entries {
		msg := e.text()
		kind, known := messageKinds[e.MessageID]
		if known && kind == "" {
			continue
		}
		prio, _ := strconv.Atoi(e.Priority)
		unit := e.Unit
		if unit == "" {
			unit = e.UserUnit
		}
		ev := Event{Time: e.time(), Kind: kind, Subject: unit, Detail: msg, Source: "journal"}
		ev.Pid, _ = strconv.Atoi(e.Pid)
		switch {
		case kind == "crashed":
			ev.Subject = e.Exe
			ev.Detail = strings.TrimSpace(e.Signal + " " + firstLine(msg))
			ev.Notable = true
		case kind == "failed" || kind == "oom-killed" || kind == "restarting":
			ev.Notable = true
		case kind == "exited":
			ev.Notable = !strings.Contains(msg, "status=0/SUCCESS")
		case kind == "started" || kind == "stopped":
			ev.Notable = false // routine; failures and restarts are what matter
		case kind == "suspend" || kind == "resume" || kind == "login" || kind == "logout":
			ev.Notable = true
		case kind == "deactivated" || kind == "starting" || kind == "stopping":
			ev.Notable = false
		case !known && prio <= 3:
			ev.Kind = "error"
			ev.Subject = firstNonEmpty(e.SysUnit, e.Ident, e.Comm)
			ev.Notable = true
		case !known:
			if strings.Contains(msg, "Out of memory: Killed process") {
				ev.Kind = "oom-killed"
				ev.Notable = true
			} else {
				continue
			}
		}
		if ev.Subject == "" {
			ev.Subject = firstNonEmpty(e.Ident, e.Comm)
		}
		out = append(out, ev)
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// LogLine is one journal line.
type LogLine struct {
	Time     time.Time `json:"time"`
	Priority int       `json:"priority"`
	Source   string    `json:"source"`
	Pid      int       `json:"pid,omitempty"`
	Message  string    `json:"message"`
}

// Logs reads the journal. unit may be a unit name or empty; priority is the
// worst-to-include threshold (3 = errors, 4 = warnings, 6 = info).
func Logs(unit string, window time.Duration, priority int, grep string, n int) []LogLine {
	args := []string{since(window), "-p", strconv.Itoa(priority), "-n", strconv.Itoa(max(n, 1))}
	if unit != "" {
		if !strings.Contains(unit, ".") {
			unit += ".service"
		}
		// the same name can be a system or a user unit; ask for both
		args = append(args, "_SYSTEMD_UNIT="+unit, "+", "_SYSTEMD_USER_UNIT="+unit, "+", "UNIT="+unit, "+", "USER_UNIT="+unit)
	}
	if grep != "" {
		args = append(args, "--grep="+grep, "--case-sensitive=false")
	}
	var out []LogLine
	for _, e := range readJournal(args...) {
		prio, _ := strconv.Atoi(e.Priority)
		pid, _ := strconv.Atoi(e.Pid)
		out = append(out, LogLine{Time: e.time(), Priority: prio, Pid: pid,
			Source: firstNonEmpty(e.Ident, e.Comm, e.SysUnit), Message: e.text()})
	}
	return out
}

// SortEvents orders events oldest first.
func SortEvents(ev []Event) {
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].Time.Before(ev[j].Time) })
}
