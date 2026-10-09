# Driving the desktop well

Worked patterns for the common jobs, and what to do when an action does not land.
Tool names first, CLI in brackets.

## Open a page in the browser

```
windows                                   # is a browser already open? where?
focus_window window="firefox"             (pc focus firefox)
press_keys keys="ctrl+t"                  (pc key ctrl+t)
type_text text="github.com/notifications" enter=true
wait_for what=["idle"]                    (pc wait idle)
screenshot window="firefox"
```

No coordinates at all: shortcuts are the fastest path and the hardest to get wrong.

## Fill a form

```
screenshot window="Settings"
click x=412 y=230                         # the "Name" field, read off that image
type_text text="Nischal" 
press_keys keys="Tab"
type_text text="nischal@example.com"
click x=640 y=600 expect="Settings"       # Save; refuses if the layout moved
```

Or in one call once the coordinates are known:

```
act script="click 412 230; type Nischal; key Tab; type nischal@example.com; click 640 600"
```

## Click something small

A 16px checkbox in a 1280px-wide shot of a 1920px monitor is 10px on the image. Zoom:

```
screenshot region="1500,300 400x200"      # layout coordinates around the target
click x=37 y=112                          # read off the zoomed image
```

The zoomed shot is now the current shot, so its coordinates just work.

## Menus

- Context menu: `click x y button="right"`, then click the item in the after-shot.
- Menu bar: click the menu, then the item; the after-shot shows the open menu.
- Hover menus: `hover x y`, then click in the shot taken while hovering.
- `Escape` closes a menu you opened by mistake.

## Scroll until something appears

```
scroll x=640 y=500 down=5                 # after-shot shows the new position
```

Repeat while the target is not visible. Each after-shot is a fresh coordinate space,
so click on the newest one.

## Launch an app and use it

```
open_app command="alacritty"              # waits for the window, prints its address
type_text text="htop" enter=true window="alacritty"
```

## Find out what the user did

```
screen_watch action="timeline" seconds=600   # who had focus, for how long
screen_watch action="view" seconds=120        # the moments that changed, one image
```

## When it does not work

| symptom | cause | fix |
|---|---|---|
| click landed on the wrong thing | the layout moved since the shot | take a new shot; pass `expect` |
| "outside the WxH image of sN" | x,y from a different image | use coordinates from the current shot, or `shot=` |
| nothing happened after a click | app is slow, or a modal stole focus | `wait_for change`, then look again |
| text went to the wrong window | focus moved (user touched the mouse) | pass `window=` to type_text / press_keys |
| "a modifier key is held down" | the user is holding Ctrl/Super/Alt | ask them to release it |
| "input control is switched OFF" | the user pressed the kill switch | stop; only they can run `pc input on` |
| a window "is not on screen" | it lives on another workspace | `focus_window` first |
| typed text came out as shortcuts | a modifier was stuck before pc checked | `press_keys keys="Escape"`, then retry |
| screenshot failed | screen locked or monitor off | tell the user; nothing to click on |

## Speed

- One screenshot per step. Low detail (800px) is enough to orient; normal to click.
- `act` for known sequences: one round trip instead of five.
- `wait_for` returns the moment the condition holds; sleeping guesses too long or too short.
- `windows` and `system_state` are text: use them before spending an image.
