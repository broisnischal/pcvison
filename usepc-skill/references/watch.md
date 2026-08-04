# Why the screen watcher is built this way

Notes for anyone changing `scripts/watch.py`. The brief was "watch the user's
computer live, without breaking" — so most of the design is about the second half.

## What "live" can and cannot mean

An agent cannot be pushed frames; it reads when it is invoked. So continuous
watching is two separate problems, and conflating them is what makes naive
implementations useless:

1. **Nothing is missed while nobody is looking.** A detached recorder writes into
   a ring buffer whether or not anyone reads it.
2. **Looking is cheap.** If catching up on five minutes costs five screenshots,
   the agent stops doing it. So every tick also writes one line of metadata, and
   the default read (`watch timeline`) is text: who had focus, when, for how long.
   Pixels are spent only on what the text left ambiguous.

Rejected: streaming frames into the context on a timer. It burns the window in
minutes and answers questions nobody asked.

## The failure modes, and what each one is

Every one of these presents identically — `grim` exits non-zero — and every one of
them ends by itself when the user comes back. That is the whole reason the loop
backs off instead of exiting:

| what happened | how it looks | how long |
|---|---|---|
| screen locked / DPMS blanked | capture fails | minutes to hours |
| laptop lid closed, output unplugged | `unknown output` | until replugged |
| compositor restarted | fails forever, silently | until rebound |
| machine suspended | a jump in wall-clock time | until resumed |

The first version (in `pcvision.py`) gave up after 60 consecutive failures and
exited. In practice that meant "the recorder is always dead by the time you want
it", because a locked screen at 1 fps burns 60 failures in a minute. Both
implementations now back off exponentially to 30 s and never exit.

**The compositor restart is the one that needs real work.** Hyprland comes back on
a *new* `wayland-N` socket, so a process holding the old one fails forever with no
way to notice. After a run of failures the recorder re-scans `$XDG_RUNTIME_DIR`
and rebinds — explicitly skipping the agent seat's nested socket, which is also a
`wayland-N` in the same directory and would otherwise mean silently recording the
agent's own screen instead of the user's.

Suspend is survivable rather than special-cased: `time.monotonic()` does not
advance across it on Linux, so the tick schedule resumes normally, and because
frames are stamped and pruned by *wall* clock, waking up hours later simply ages
the whole ring out. The schedule guard exists for the other case — a capture that
took longer than its interval — where it resets instead of firing a catch-up
burst.

## Why a supervisor process

The loop catches exceptions, so the only way the recorder dies is the process
going away entirely — OOM kill, `kill -9`, a Python crash. That is exactly what a
parent process is for. It is ~25 lines, and it turns "the watch silently stopped
an hour ago" into "two seconds of history missing".

On top of that, any `watch` read self-heals: if the daemon is gone or its
heartbeat is stale, it is restarted from the stored config before the read runs.
That deliberately only ever revives a watch **the user already started** —
recording a screen because someone asked a question about it would be a much
worse bug than not recording one.

Health is a heartbeat file, not the pid. A wedged process still has a live pid;
what matters is whether a frame landed recently, so `beat.json` is rewritten every
tick and `status` reports `◐` plus the actual error when it goes stale.

## Why identical frames are not stored

`grim` is deterministic: an unchanged screen encodes to byte-identical JPEG (the
cursor is excluded by default, which is what makes this hold in practice). So a
hash comparison catches idle time exactly, with no image library and no
thresholds — the skill is stdlib-only and starts in ~30 ms, and pulling in Pillow
for a perceptual diff would have cost more than the feature is worth.

Two things fall out of it for free: an idle desktop costs no disk at all, and
`idle` in the timeline is a *fact* rather than a guess — the screen did not
change, full stop. Partial change (a blinking cursor, a clock) counts as activity,
which is the honest reading.

## Bounding the ring

Both time and bytes, because either alone fails: 4K at `--detail full` blows past
a sensible disk budget inside the default window, and a byte cap alone gives an
unpredictable amount of history. Age is trimmed first, then size, and the index is
rewritten to match so the timeline never references frames that are gone.

## Privacy is a design constraint, not a note at the end

This records the user's real screen — the thing the rest of the tool goes out of
its way *not* to touch. So: frames never leave the machine, they live under the
user's own cache dir, they age out on their own, `stop --purge` deletes them, and
starting a watch is always an explicit act. Nothing in the tool starts one
implicitly, including the self-healing path.
