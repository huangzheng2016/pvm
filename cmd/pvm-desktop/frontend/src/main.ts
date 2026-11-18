import { backend, errorText, windowIsMinimised } from "./api";
import * as create from "./create";
import { button, command, focusKey, h, held, refocus } from "./dom";
import * as exits from "./exits";
import * as settings from "./settings";
import { act, busy, changed, data, following, nav, onChange, readVMs, ready, refresh, toastArea } from "./store";
import * as tasks from "./tasks";
import * as vms from "./vms";
import * as workspaces from "./workspaces";

const visibleInterval = 2000;
const minimisedInterval = 10000;

interface Screen {
  label: string;
  render(): HTMLElement;
  enter?(): void;
}

const screens: Record<string, Screen> = {
  vms: { label: "VMs", render: vms.render },
  create: { label: "Create", render: create.render, enter: create.enter },
  exits: { label: "Exits", render: exits.render },
  workspaces: { label: "Workspaces", render: workspaces.render },
  settings: { label: "Settings", render: settings.render },
};

const view = h("main", { class: "view", tabindex: -1 });
const daemonStatus = h("div", { class: "daemon-status" });
const links = Object.entries(screens).map(([id, s]) => h("button", { type: "button", "data-screen": id, onclick: () => nav.go(id) }, s.label));
const side = h("nav", { class: "side", "aria-label": "Screens" }, h("div", { class: "brand" }, "pvm Desktop"), ...links, daemonStatus);

nav.go = (id) => {
  nav.current = id;
  screens[id].enter?.();
  render(true);
  view.scrollTop = 0;
  void refresh();
};

async function readState(): Promise<void> {
  try {
    data.state = await backend().State();
    data.stateError = "";
  } catch (e) {
    data.state = null;
    data.stateError = errorText(e);
  }
}

// The state is read again before the button comes back, so it does not
// say "stopped" about a daemon that has just started.
function startDaemon(): void {
  void act(["daemon"], async () => {
    const task = await backend().StartDaemon();
    await readState();
    return task;
  });
}

// gate is the page shown in place of the screens when they cannot work.
function gate(): HTMLElement | null {
  const st = data.state;
  if (!st) {
    return data.stateError === ""
      ? h("div", { class: "gate" }, h("p", { class: "muted" }, "Loading..."))
      : h("div", { class: "gate" }, h("h1", null, "pvm Desktop cannot start"), h("p", { class: "error", role: "alert" }, data.stateError));
  }
  if (st.backend !== "native") {
    return h(
      "div",
      { class: "gate" },
      h("h1", null, "pvm is using the WSL2 backend"),
      h(
        "p",
        null,
        "With the WSL2 backend the pvm daemon runs inside WSL, and Windows has no address or token to reach it. pvm Desktop works with the native Windows backend only, so it has nothing to show here.",
      ),
      h("p", null, "Everything works from the pvm command line, in a terminal:"),
      command("pvm ls"),
      command("pvm ssh"),
      command("pvm --help"),
    );
  }
  if (st.daemon_healthy) {
    return null;
  }
  const working = busy.has("daemon");
  const start = button("Start the daemon", startDaemon, { kind: "primary", disabled: working, key: "daemon:start" });
  return h(
    "div",
    { class: "gate" },
    h("h1", null, working ? "Starting the pvm daemon..." : st.daemon_stopped ? "The pvm daemon is stopped" : "The pvm daemon is not running"),
    h(
      "p",
      null,
      st.daemon_stopped
        ? "It was stopped with `pvm daemon stop`, so pvm Desktop does not start it by itself. VMs and exits show once it runs."
        : "pvm Desktop reads VMs and exits from the daemon. The output of the start is in Activity below.",
    ),
    working ? h("span", { class: "spinner", role: "status", "aria-label": "Working" }) : start,
  );
}

let shown = "";
let waiting = false;

// typing is true while the user is in a field of the view: replacing
// the view then would take the field away under their hands.
function typing(): boolean {
  const at = document.activeElement;
  return at !== null && view.contains(at) && at.matches("input:not([type=checkbox]), select, textarea");
}

function render(force = false): void {
  const st = data.state;
  const page = gate();
  side.hidden = tasks.panel.hidden = st !== null && st.backend !== "native";
  for (const link of links) {
    link.disabled = page !== null;
    link.toggleAttribute("aria-current", page === null && link.dataset.screen === nav.current);
  }
  daemonStatus.className = "daemon-status " + (ready() ? "up" : "down");
  daemonStatus.textContent = ready() ? "Daemon running" : "Daemon not running";

  const signature = JSON.stringify([nav.current, data, busy.entries(), following.entries()]);
  if (!force && signature === shown) {
    return;
  }
  if (!force && (held.count > 0 || typing())) {
    waiting = true;
    return;
  }
  shown = signature;
  waiting = false;
  const node = page ?? screens[nav.current].render();
  if (view.firstElementChild === node) {
    return;
  }
  const focused = document.activeElement instanceof HTMLElement && view.contains(document.activeElement) ? document.activeElement.dataset.key : undefined;
  const scroll = view.scrollTop;
  view.replaceChildren(node);
  view.scrollTop = scroll;
  if (focused !== undefined) {
    focusKey(focused);
  } else {
    refocus();
  }
}

function renderWaiting(): void {
  if (waiting) {
    render();
  }
}

let startTried = false;
let wasReady = false;

async function poll(): Promise<void> {
  await readState();
  const st = data.state;
  if (st?.backend === "native" && !st.daemon_healthy && !st.daemon_stopped && !startTried) {
    // once per run of the app: a daemon that keeps dying is the user's to look at
    startTried = true;
    startDaemon();
  }
  if (ready() && !wasReady) {
    await refresh();
  } else if (ready()) {
    await readVMs();
  }
  wasReady = ready();
  changed();
}

// One poll at a time: the next is scheduled when this one has answered.
async function loop(): Promise<void> {
  await poll();
  const minimised = await windowIsMinimised().catch(() => false);
  setTimeout(() => void loop(), minimised ? minimisedInterval : visibleInterval);
}

document.getElementById("app")!.append(side, h("div", { class: "main" }, view, tasks.panel), toastArea);
onChange(() => render());
held.onRelease = renderWaiting;
view.addEventListener("focusout", () => setTimeout(renderWaiting));
window.addEventListener("focus", () => void refresh());
tasks.listen();
render();
void loop();
