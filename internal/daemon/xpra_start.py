# pvm starts the Xpra server through this file. The Xpra server only
# presses keycodes its keymap already has, and the keymap it builds from
# the client fills every free keycode, so text a host input method commits
# (keycode 0 with the character as its string, such as Chinese from
# Microsoft Pinyin) was dropped. Such presses are typed with xdotool, which
# needs one empty keycode to borrow for each character; 254 (XF86WWAN) is
# cleared for it, as the full keymap leaves none.
import queue
import re
import subprocess
import sys
import threading
import time

from xpra.platform import init, set_default_name

set_default_name("Xpra")
init()

from xpra.server.subsystem.keyboard import KeyboardServer  # noqa: E402

_text = queue.Queue()


def _typist():
    while True:
        s = _text.get()
        # an input method commits a word as one key event per character,
        # milliseconds apart: type it in one call
        while True:
            try:
                s += _text.get(timeout=0.03)
            except queue.Empty:
                break
        try:
            keymap = subprocess.run(["xmodmap", "-pke"], capture_output=True, text=True, timeout=10).stdout
            if not re.search(r"^keycode 254 =\s*$", keymap, re.M):
                subprocess.run(["xmodmap", "-e", "keycode 254 ="], timeout=10, check=False)
                # applications reload the changed keymap; typing at once
                # lost the first character
                time.sleep(0.2)
            # applications look the borrowed keycode up after the event; a
            # shorter delay lost characters
            subprocess.run(["xdotool", "type", "--clearmodifiers", "--delay", "40", "--", s], timeout=60, check=False)
            time.sleep(0.05)
        except Exception as e:  # keep typing later text
            print("pvm: xdotool type failed: %s" % e, file=sys.stderr)


threading.Thread(target=_typist, daemon=True).start()
_get_keycode = KeyboardServer.get_keycode


def get_keycode(self, ss, client_keycode, keyname, pressed, modifiers, keyval, keystr, group):
    keycode, rgroup = _get_keycode(self, ss, client_keycode, keyname, pressed, modifiers, keyval, keystr, group)
    if keycode < 0 and pressed and client_keycode == 0 and keystr and keystr.isprintable():
        _text.put(keystr)
    return keycode, rgroup


KeyboardServer.get_keycode = get_keycode

from xpra.scripts.main import main  # noqa: E402

sys.exit(main("xpra", ["xpra"] + sys.argv[1:]))
