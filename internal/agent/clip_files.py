# The pvm agent puts files on the clipboard of the pvm app display with
# this script, run as the VM user with the files as arguments. It owns the
# CLIPBOARD selection and offers the files the way file managers
# (x-special/gnome-copied-files), browsers and Electron apps (text/uri-list)
# and text editors (their paths) take them, until another program takes
# the selection. xclip offers a single target, and GTK's clipboard API
# for several is not available to Python.
import ctypes
import os
import sys
from urllib.parse import quote

SELECTION_CLEAR, SELECTION_REQUEST = 29, 30
XA_ATOM = 4
PROP_MODE_REPLACE = 0


class SelectionRequest(ctypes.Structure):
    _fields_ = [("type", ctypes.c_int), ("serial", ctypes.c_ulong), ("send_event", ctypes.c_int),
                ("display", ctypes.c_void_p), ("owner", ctypes.c_ulong), ("requestor", ctypes.c_ulong),
                ("selection", ctypes.c_ulong), ("target", ctypes.c_ulong), ("property", ctypes.c_ulong),
                ("time", ctypes.c_ulong)]


class SelectionNotify(ctypes.Structure):
    _fields_ = [("type", ctypes.c_int), ("serial", ctypes.c_ulong), ("send_event", ctypes.c_int),
                ("display", ctypes.c_void_p), ("requestor", ctypes.c_ulong), ("selection", ctypes.c_ulong),
                ("target", ctypes.c_ulong), ("property", ctypes.c_ulong), ("time", ctypes.c_ulong)]


class XEvent(ctypes.Union):
    _fields_ = [("type", ctypes.c_int), ("request", SelectionRequest), ("notify", SelectionNotify),
                ("pad", ctypes.c_long * 24)]


def main():
    paths = [os.path.abspath(p) for p in sys.argv[1:]]
    if not paths:
        sys.exit("no files")
    uris = ["file://" + quote(p) for p in paths]
    x = ctypes.CDLL("libX11.so.6")
    x.XOpenDisplay.restype = ctypes.c_void_p
    x.XOpenDisplay.argtypes = [ctypes.c_char_p]
    x.XInternAtom.restype = ctypes.c_ulong
    x.XInternAtom.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int]
    x.XDefaultRootWindow.restype = ctypes.c_ulong
    x.XDefaultRootWindow.argtypes = [ctypes.c_void_p]
    x.XCreateSimpleWindow.restype = ctypes.c_ulong
    x.XCreateSimpleWindow.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_int, ctypes.c_int, ctypes.c_uint,
                                      ctypes.c_uint, ctypes.c_uint, ctypes.c_ulong, ctypes.c_ulong]
    x.XSetSelectionOwner.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong]
    x.XGetSelectionOwner.restype = ctypes.c_ulong
    x.XGetSelectionOwner.argtypes = [ctypes.c_void_p, ctypes.c_ulong]
    x.XNextEvent.argtypes = [ctypes.c_void_p, ctypes.POINTER(XEvent)]
    x.XChangeProperty.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_int,
                                  ctypes.c_int, ctypes.c_void_p, ctypes.c_int]
    x.XSendEvent.argtypes = [ctypes.c_void_p, ctypes.c_ulong, ctypes.c_int, ctypes.c_long, ctypes.POINTER(XEvent)]
    x.XFlush.argtypes = [ctypes.c_void_p]

    dpy = x.XOpenDisplay(None)
    if not dpy:
        sys.exit("cannot open display %s" % os.environ.get("DISPLAY", ""))

    def atom(name):
        return x.XInternAtom(dpy, name.encode(), 0)

    clipboard, targets_atom = atom("CLIPBOARD"), atom("TARGETS")
    offers = {
        atom("x-special/gnome-copied-files"): ("copy\n" + "\n".join(uris)).encode(),
        atom("text/uri-list"): ("".join(u + "\r\n" for u in uris)).encode(),
        atom("UTF8_STRING"): "\n".join(paths).encode(),
        atom("text/plain;charset=utf-8"): "\n".join(paths).encode(),
    }
    win = x.XCreateSimpleWindow(dpy, x.XDefaultRootWindow(dpy), 0, 0, 1, 1, 0, 0, 0)
    x.XSetSelectionOwner(dpy, clipboard, win, 0)
    if x.XGetSelectionOwner(dpy, clipboard) != win:
        sys.exit("cannot own the clipboard")
    x.XFlush(dpy)
    # the caller waits for this process; serve the selection from a child
    if os.fork() > 0:
        os._exit(0)
    os.setsid()
    null = os.open(os.devnull, os.O_RDWR)
    for fd in (0, 1, 2):
        os.dup2(null, fd)
    ev = XEvent()
    while True:
        x.XNextEvent(dpy, ctypes.byref(ev))
        if ev.type == SELECTION_CLEAR:
            return
        if ev.type != SELECTION_REQUEST:
            continue
        req = ev.request
        prop = req.property or req.target
        if req.target == targets_atom:
            ids = [targets_atom] + list(offers)
            arr = (ctypes.c_ulong * len(ids))(*ids)
            x.XChangeProperty(dpy, req.requestor, prop, XA_ATOM, 32, PROP_MODE_REPLACE, arr, len(ids))
        elif req.target in offers:
            data = offers[req.target]
            x.XChangeProperty(dpy, req.requestor, prop, req.target, 8, PROP_MODE_REPLACE, data, len(data))
        else:
            prop = 0
        reply = XEvent()
        reply.notify = SelectionNotify(type=31, send_event=1, display=dpy, requestor=req.requestor,
                                       selection=req.selection, target=req.target, property=prop, time=req.time)
        x.XSendEvent(dpy, req.requestor, 0, 0, ctypes.byref(reply))
        x.XFlush(dpy)


main()
