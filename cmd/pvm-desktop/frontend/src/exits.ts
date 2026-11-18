import { backend, type Exit } from "./api";
import { badge, button, checkbox, dialog, field, h, menuButton, pageHeader, table } from "./dom";
import { act, busy, data } from "./store";
import { exitDialog } from "./vms";

// The CLI's rule, exit.ValidName.
const nameRule = /^[a-zA-Z][a-zA-Z0-9_-]{0,31}$/;
// What the backend takes as host:port: a name or an IPv4 address, or an
// IPv6 address in brackets.
const serverRule = /^([^\s:[\]-][^\s:[\]]*|\[[0-9a-fA-F:.]+\]):\d+$/;

function checkName(name: string): string {
  if (!nameRule.test(name)) {
    return "The name takes letters, digits, - and _, at most 32, starting with a letter.";
  }
  if (name === "direct" || data.exits?.some((e) => e.name === name)) {
    return "An exit named " + name + " already exists.";
  }
  return "";
}

const input = (type: string, placeholder: string) => h("input", { type, placeholder, spellcheck: "false", autocomplete: "off" });

function addDialog(): void {
  const name = input("text", "my-exit");
  const server = input("text", "vps.example.com:18443");
  const token = input("password", "");
  dialog({
    title: "Add an exit",
    content: [
      field("Name", name),
      field("Server", server, "The address of a server running pvm exit-agent, as host:port."),
      field("Token", token, "The token of that exit-agent. It is handed to pvm in a private file and is not shown or logged."),
      h("p", { class: "hint" }, "Adding probes the connection, which can take up to two minutes."),
    ],
    submit: "Add exit",
    validate: () => {
      if (checkName(name.value) !== "") {
        return checkName(name.value);
      }
      if (!serverRule.test(server.value)) {
        return "The server must be host:port.";
      }
      return /^\S+$/.test(token.value) ? "" : "Enter the token, without spaces.";
    },
    onSubmit: () => void act(["exit:" + name.value], () => backend().AddExit({ name: name.value, server: server.value, token: token.value })),
  });
}

function addWarpDialog(): void {
  const name = input("text", "my-exit");
  dialog({
    title: "Add a WARP exit",
    content: [field("Name", name), h("p", { class: "hint" }, "This registers a new Cloudflare WARP device and uses it as the exit.")],
    submit: "Add WARP exit",
    validate: () => checkName(name.value),
    onSubmit: () => void act(["exit:" + name.value], () => backend().AddWarpExit(name.value)),
  });
}

function testDialog(exit: Exit): void {
  const speed = checkbox("Also run a speed test", false);
  dialog({
    title: "Test " + exit.name,
    content: [h("p", null, "The result shows in Activity."), speed.el],
    submit: "Test",
    onSubmit: () => void act(["exit:" + exit.name], () => backend().TestExit(exit.name, speed.box.checked)),
  });
}

function removeDialog(exit: Exit): void {
  const users = [
    exit.default && "the default exit",
    exit.vms.length > 0 && "VMs: " + exit.vms.join(", "),
    exit.rules.length > 0 && "route rules: " + exit.rules.join(", "),
  ].filter((u): u is string => u !== false);
  dialog({
    title: "Remove " + exit.name + "?",
    content: [
      h("p", null, "This removes the exit ", h("strong", null, exit.name), " from pvm."),
      users.length > 0 &&
        h(
          "div",
          { class: "warning" },
          h("p", null, exit.name + " is still used by:"),
          h("ul", null, ...users.map((u) => h("li", null, u))),
          h("p", null, "pvm does not check this. Point them at another exit first, or they keep naming an exit that no longer exists."),
        ),
    ],
    submit: "Remove " + exit.name,
    danger: true,
    onSubmit: () => void act(["exit:" + exit.name], () => backend().RemoveExit(exit.name, true)),
  });
}

function setDefault(name: string): void {
  void act(["exit:" + name], () => backend().SetDefaultExit(name));
}

function usedBy(vms: string[], rules: string[]): HTMLElement {
  const parts = [vms.join(", "), rules.length > 0 ? rules.length + (rules.length === 1 ? " rule" : " rules") : ""].filter((p) => p !== "");
  const all = [...vms, ...rules].join("\n");
  return h("td", { class: "grow" }, parts.length > 0 ? h("div", { class: "clamp", title: all }, parts.join("; ")) : h("span", { class: "muted" }, "nothing"));
}

function row(exit: Exit): HTMLTableRowElement {
  const working = busy.has("exit:" + exit.name);
  const k = (action: string) => "exit:" + exit.name + ":" + action;
  const region = [exit.tz, exit.locale].filter((p) => p !== "").join(" - ");
  return h(
    "tr",
    { class: working ? "working" : "" },
    h(
      "td",
      { class: "grow" },
      h("div", { class: "name" }, h("strong", { title: exit.name }, exit.name), exit.default && badge("default", "accent")),
      h("div", { class: "sub" }, h("code", null, exit.endpoint)),
      region !== "" && h("div", { class: "sub" }, region),
    ),
    h("td", { class: "fit" }, exit.kind === "warp" ? "WARP" : h("span", { title: "pvm exit-agent over WireGuard" }, "Agent")),
    usedBy(exit.vms, exit.rules),
    h(
      "td",
      { class: "actions" },
      working && h("span", { class: "spinner", role: "status", "aria-label": "Working" }),
      // `pvm exit test --speed` is for agent exits only
      button("Test", () => (exit.kind === "warp" ? void act(["exit:" + exit.name], () => backend().TestExit(exit.name, false)) : testDialog(exit)), {
        disabled: working,
        key: k("test"),
      }),
      button("Set default", () => setDefault(exit.name), { disabled: working || exit.default, key: k("default") }),
      button("Remove", () => removeDialog(exit), { kind: "danger", disabled: working, key: k("remove") }),
    ),
  );
}

// directRow stands for no exit at all, which can be the default too.
function directRow(): HTMLTableRowElement {
  const isDefault = data.defaultExit === "direct";
  const working = busy.has("exit:direct");
  const vms = (data.vms ?? []).filter((vm) => vm.exit === "direct").map((vm) => vm.name);
  return h(
    "tr",
    { class: working ? "working" : "" },
    h(
      "td",
      { class: "grow" },
      h("div", { class: "name" }, h("strong", null, "direct"), isDefault && badge("default", "accent")),
      h("div", { class: "sub" }, "the host's network"),
    ),
    h("td", { class: "fit" }, "No exit"),
    usedBy(vms, []),
    h(
      "td",
      { class: "actions" },
      working && h("span", { class: "spinner", role: "status", "aria-label": "Working" }),
      button("Set default", () => setDefault("direct"), { disabled: working || isDefault, key: "exit:direct:default" }),
    ),
  );
}

export function render(): HTMLElement {
  const page = h(
    "div",
    { class: "page" },
    pageHeader(
      "Exits",
      menuButton(
        "Add exit",
        [
          { label: "Agent exit (your own server)", action: addDialog },
          { label: "Cloudflare WARP", action: addWarpDialog },
        ],
        { key: "exits:add" },
      ),
    ),
  );
  if (data.errors.exits) {
    page.append(h("p", { class: "error banner", role: "alert" }, "Cannot read the exits: " + data.errors.exits));
  }
  if (!data.exits) {
    page.append(h("p", { class: "muted" }, data.errors.exits ? "" : "Loading..."));
    return page;
  }
  const def = data.defaultExit;
  if (def !== null && def !== "direct" && !data.exits.some((e) => e.name === def)) {
    page.append(
      h(
        "p",
        { class: "warning", role: "alert" },
        "The default exit " + def + " no longer exists. VMs that follow the default exit have no network until another default is set, here or with `pvm route default --via <exit>`.",
      ),
    );
  }
  const list = table(["Name", "Kind", "Used by", "Actions"], [...data.exits.map(row), directRow()]);
  list.classList.add("exits");
  page.append(list);
  if (data.exits.length === 0) {
    page.append(h("p", { class: "muted" }, "No exits yet. An exit is a server the traffic of a VM leaves through; without one, VMs use the host's network."));
  }
  const vms = data.vms ?? [];
  if (vms.length > 0) {
    page.append(
      h("h2", null, "Exit of each VM"),
      table(
        ["VM", "Exit", "Actions"],
        vms.map((vm) =>
          h(
            "tr",
            null,
            h("td", { class: "grow" }, h("div", { class: "name" }, h("strong", { title: vm.name }, vm.name))),
            h("td", { class: "grow" }, h("div", { class: "name" }, h("span", { title: vm.exit }, vm.exit), !vm.exit_own && badge("default exit"))),
            h("td", { class: "actions" }, button("Change exit", () => exitDialog(vm.name, vm.exit), { disabled: busy.has("vm:" + vm.name), key: "route:" + vm.name })),
          ),
        ),
      ),
    );
  }
  return page;
}
