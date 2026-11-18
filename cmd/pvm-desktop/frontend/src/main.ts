import { backend, bound, errorText, windowIsMinimised } from "./api";
import * as create from "./create";
import { button, callout, command, dropTip, focusKey, h, held, iconButton, mono, refocus, spinner } from "./dom";
import * as exits from "./exits";
import { language, onLanguage, t } from "./i18n";
import { icon, type IconName } from "./icons";
import * as prefs from "./prefs";
import * as settings from "./settings";
import { act, busy, changed, data, following, nav, onChange, readVMs, ready, refresh, toastArea } from "./store";
import * as tasks from "./tasks";
import * as vms from "./vms";
import * as workspaces from "./workspaces";

const visibleInterval = 2000;
const minimisedInterval = 10000;

interface Screen {
  icon: IconName;
  label(): string;
  render(): HTMLElement;
  enter?(): void;
}

const screens: Record<string, Screen> = {
  vms: { icon: "server", label: () => t.nav.vms, render: vms.render, enter: vms.enter },
  create: { icon: "plusCircle", label: () => t.nav.create, render: create.render, enter: create.enter },
  exits: { icon: "route", label: () => t.nav.exits, render: exits.render, enter: exits.enter },
  workspaces: { icon: "folder", label: () => t.nav.workspaces, render: workspaces.render, enter: workspaces.enter },
  settings: { icon: "gear", label: () => t.nav.settings, render: settings.render },
};
const order = Object.keys(screens);

const view = h("main", { class: "view", tabindex: -1 });
const links = order.map((id) =>
  h(
    "button",
    { type: "button", class: "nav-item", "data-screen": id, "data-tip-rail": true, onclick: () => nav.go(id) },
    icon(screens[id].icon, "lg"),
    h("span", { class: "nav-label" }),
  ),
);
// The bar that marks the current screen; it slides when --i changes.
const indicator = h("span", { class: "nav-indicator", "aria-hidden": "true" });
const daemonText = h("span", { class: "daemon-text" });
const daemonStatus = h("div", { class: "daemon", role: "status", "data-tip-rail": true }, h("span", { class: "daemon-dot", "aria-hidden": "true" }), daemonText);
const version = h("span", { class: "nav-version" });
const collapse = iconButton("sidebar", "", () => setCollapsed(side.dataset.collapsed !== "true"), { small: true });
collapse.classList.add("nav-toggle");
const side = h(
  "nav",
  { class: "nav" },
  h("div", { class: "nav-top" }, h("span", { class: "nav-mark", "aria-hidden": "true" }, icon("box")), h("span", { class: "nav-name", lang: "en" }, "PVM Desktop"), collapse),
  h("div", { class: "nav-list" }, indicator, ...links),
  h("div", { class: "nav-foot" }, daemonStatus, version),
);

// The sidebar narrows to its icons; a window below 860px does that by
// itself, in style.css.
function setCollapsed(collapsed: boolean): void {
  side.dataset.collapsed = String(collapsed);
  prefs.save("sidebar", collapsed ? "collapsed" : null);
  labelShell();
}

// labelShell writes the words of the parts that are built once.
function labelShell(): void {
  side.setAttribute("aria-label", t.nav.label);
  for (const link of links) {
    const label = screens[link.dataset.screen!].label();
    link.lastElementChild!.textContent = label;
    // the name is hidden in the narrow rail
    link.setAttribute("aria-label", label);
    link.dataset.tip = label;
  }
  const toggle = side.dataset.collapsed === "true" ? t.nav.expand : t.nav.collapse;
  collapse.setAttribute("aria-label", toggle);
  collapse.dataset.tip = toggle;
}

nav.go = (id) => {
  nav.current = id;
  screens[id].enter?.();
  render(true);
  view.querySelector(".page-body")?.scrollTo(0, 0);
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

const art = (name: IconName, kind = ""): HTMLElement =>
  h("div", { class: "state-art" + (kind === "" ? "" : " state-art--" + kind), "aria-hidden": "true" }, icon(name, "xl"));

// gate is the page shown in place of the screens when they cannot work.
function gate(): HTMLElement {
  const st = data.state;
  if (!st && data.stateError === "") {
    return h(
      "div",
      { class: "state", role: "status" },
      h("div", { class: "loader", "aria-hidden": "true" }, h("i"), h("i"), h("i")),
      h("p", { class: "state-text" }, t.common.loading),
    );
  }
  if (!st) {
    const reason = callout("danger", "alert", h("span", { class: "mono", lang: "en" }, data.stateError));
    reason.classList.add("state-block");
    return h(
      "div",
      { class: "state" },
      art("warning", "danger"),
      h("h1", { class: "state-title" }, t.gate.cannotStart),
      h("p", { class: "state-text" }, t.gate.reason),
      reason,
    );
  }
  if (st.backend !== "native") {
    return h(
      "div",
      { class: "state" },
      art("terminal"),
      h("h1", { class: "state-title" }, t.gate.wslTitle),
      h("p", { class: "state-text" }, t.gate.wslText),
      h(
        "div",
        { class: "state-block" },
        h("p", { class: "hint" }, t.gate.wslLead),
        h("div", { class: "cmd-list" }, command("pvm ls"), command("pvm ssh"), command("pvm --help")),
      ),
    );
  }
  const working = busy.has("daemon");
  return h(
    "div",
    { class: "state", role: "status" },
    working ? h("div", { class: "state-art state-art--accent", "aria-hidden": "true" }, spinner()) : art("power"),
    h("h1", { class: "state-title" }, working ? t.gate.startingTitle : st.daemon_stopped ? t.gate.stoppedTitle : t.gate.missingTitle),
    h("p", { class: "state-text" }, ...(st.daemon_stopped ? t.gate.stoppedText(mono("pvm daemon stop")) : [t.gate.missingText])),
    !working && button(t.gate.start, startDaemon, { kind: "primary", size: "lg", icon: "power", key: "daemon:start" }),
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
  const gated = !ready();
  side.hidden = tasks.panel.hidden = st !== null && st.backend !== "native";
  for (const link of links) {
    link.disabled = gated;
    if (!gated && link.dataset.screen === nav.current) {
      link.setAttribute("aria-current", "page");
    } else {
      link.removeAttribute("aria-current");
    }
  }
  indicator.hidden = gated;
  indicator.style.setProperty("--i", String(order.indexOf(nav.current)));
  const daemon = st === null ? "unknown" : ready() ? "running" : "stopped";
  daemonStatus.dataset.state = daemon;
  daemonStatus.dataset.tip = daemonText.textContent = daemon === "running" ? t.nav.daemonUp : daemon === "stopped" ? t.nav.daemonDown : t.nav.daemonUnknown;
  version.textContent = st?.app_version ?? "";

  const signature = JSON.stringify([language(), nav.current, data, busy.entries(), following.entries()]);
  if (!force && signature === shown) {
    return;
  }
  if (!force && (held.count > 0 || typing())) {
    waiting = true;
    return;
  }
  shown = signature;
  waiting = false;
  const focused = document.activeElement instanceof HTMLElement && view.contains(document.activeElement) ? document.activeElement.dataset.key : undefined;
  // a screen keeps its page and changes it in place; a gate is drawn anew
  const node = gated ? gate() : screens[nav.current].render();
  if (view.firstElementChild !== node) {
    view.replaceChildren(node);
  }
  dropTip();
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

async function boot(): Promise<void> {
  if (import.meta.env.DEV && !bound()) {
    // `npm run dev` in a browser: no Wails window, so a made-up backend
    (await import("./mock")).install();
  }
  side.dataset.collapsed = String(prefs.load("sidebar") === "collapsed");
  labelShell();
  document.getElementById("app")!.append(side, h("div", { class: "app-main" }, h("div", { class: "app-stage" }, view, toastArea), tasks.panel));
  onChange(() => render());
  onLanguage(() => {
    labelShell();
    render(true);
  });
  held.onRelease = renderWaiting;
  view.addEventListener("focusout", () => setTimeout(renderWaiting));
  window.addEventListener("focus", () => void refresh());
  tasks.listen();
  render();
  void loop();
}

void boot();
