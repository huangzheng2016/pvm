# pvm adds this file next to the client's xpra/client/gui/keyboard_helper.pyc;
# Python loads a source file before compiled code of the same name. It runs
# the original module, then fixes one bug in it.
#
# GTK on Windows turns a character outside the Basic Multilingual Plane,
# such as an emoji, into two key events, one per UTF-16 surrogate. The
# client then fails to encode the lone surrogate, drops the connection and
# loses every window. The two halves are joined into one key event that
# carries the whole character, which the server types like other text from
# an input method.
import marshal
import os

with open(os.path.join(os.path.dirname(__file__), "keyboard_helper.pyc"), "rb") as _f:
    exec(marshal.loads(_f.read()[16:]), globals())

_send_key_action = KeyboardHelper.send_key_action  # noqa: F821


def _pvm_send_key_action(self, wid, key_event):
    s = getattr(key_event, "string", "") or ""
    if len(s) == 1 and 0xD800 <= ord(s) <= 0xDFFF:
        pending = self.__dict__.setdefault("_pvm_high_surrogate", {})
        if ord(s) < 0xDC00:
            pending[key_event.pressed] = s
            return
        high = pending.pop(key_event.pressed, None)
        if high is None:
            return
        ch = (high + s).encode("utf-16-le", "surrogatepass").decode("utf-16-le")
        key_event.string = ch
        key_event.keyval = 0x1000000 + ord(ch)
        key_event.keyname = "U+%X" % ord(ch)
    return _send_key_action(self, wid, key_event)


KeyboardHelper.send_key_action = _pvm_send_key_action  # noqa: F821
