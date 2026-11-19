# pvm runs this as the VM user each time the app display starts. Apps there,
# such as Claude Desktop, keep secrets in the Secret Service, which
# gnome-keyring locks until a desktop login unlocks it; the app display
# has no login, so the keyring asked for a password on every start. A
# "login" keyring with an empty password is made the default and unlocked
# here, as a passwordless desktop login would. Its contents are stored
# unencrypted, protected by the VM user's file permissions like the rest
# of the home directory. A keyring the user created with a password is
# left alone.
import os

from gi.repository import Gio, GLib

SECRETS = "org.freedesktop.secrets"
SERVICE = "/org/freedesktop/secrets"
LOGIN = SERVICE + "/collection/login"

d = os.path.expanduser("~/.local/share/keyrings")
os.makedirs(d, mode=0o700, exist_ok=True)
path = os.path.join(d, "login.keyring")
if not os.path.exists(path):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write("[keyring]\ndisplay-name=Login\nctime=0\nmtime=0\nlock-on-idle=false\nlock-after=false\n")
    default = os.path.join(d, "default")
    if not os.path.exists(default):
        with open(default, "w") as f:
            f.write("login")

with open(path, "rb") as f:
    # a keyring with a password is a binary file
    plain = f.read(9) == b"[keyring]"

if plain:
    bus = Gio.bus_get_sync(Gio.BusType.SESSION)

    def call(obj, iface, method, args, rtype):
        return bus.call_sync(SECRETS, obj, iface, method, args, GLib.VariantType(rtype), 0, 10000, None).unpack()

    _, session = call(SERVICE, "org.freedesktop.Secret.Service", "OpenSession",
                      GLib.Variant("(sv)", ("plain", GLib.Variant("s", ""))), "(vo)")
    secret = GLib.Variant("(oayays)", (session, b"", b"", "text/plain"))
    call(SERVICE, "org.gnome.keyring.InternalUnsupportedGuiltRiddenInterface", "UnlockWithMasterPassword",
         GLib.Variant.new_tuple(GLib.Variant("o", LOGIN), secret), "()")
