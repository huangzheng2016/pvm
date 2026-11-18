import { backend, errorText, type VM } from "./api";
import { badge, button, checkbox, dialog, empty, field, h, menuButton, pageHeader, select, table } from "./dom";
import { act, actStart, busy, data, memory, nav, onlyFollowed } from "./store";

const stateLabel = { stopped: "Stopped", starting: "Starting", ready: "Ready", failed: "Failed" };

function stateText(vm: VM): string {
  return vm.state === "starting" && vm.stage !== "" ? "Starting (" + vm.stage + ")" : stateLabel[vm.state];
}

// exitChoices are the values `pvm route --via` takes.
export function exitChoices(): { value: string; label: string }[] {
  return [{ value: "direct", label: "direct (no exit)" }, ...(data.exits ?? []).map((e) => ({ value: e.name, label: e.name }))];
}

export function exitDialog(name: string, current: string): void {
  const choices = exitChoices();
  const gone = !choices.some((c) => c.value === current);
  const via = select(choices, gone ? choices[0].value : current);
  dialog({
    title: "Change the exit of " + name,
    content: [
      field("Exit", via),
      gone && h("p", { class: "warning" }, "The current exit " + current + " no longer exists, so " + name + " has no network until another is chosen."),
      h("p", { class: "warning" }, "A running VM restarts to apply the new exit. Programs running in " + name + " are stopped."),
    ],
    submit: "Change exit",
    onSubmit: () => void act(["vm:" + name], () => backend().SetVMExit(name, via.value, true)),
  });
}

// appDialog asks for a command line or a URL. programs, when given,
// loads the one-click entries shown under the field.
export function appDialog(title: string, run: (target: string) => void, programs?: (list: HTMLElement, close: () => void) => void): void {
  const target = h("input", { type: "text", spellcheck: "false", autocomplete: "off", placeholder: "firefox  or  https://example.com" });
  const list = h("div", { class: "programs" });
  const d = dialog({
    title,
    content: [
      field("Command line or URL", target, "A command runs in the VM and shows its window here. A URL opens in the VM's browser."),
      programs && h("div", { class: "field-label" }, "Installed programs"),
      programs && list,
    ],
    submit: "Open",
    validate: () => (target.value.trim() === "" ? "Enter a command or a URL." : ""),
    onSubmit: () => run(target.value),
  });
  programs?.(list, d.close);
}

function vmAppDialog(vm: VM): void {
  const key = ["vm:" + vm.name];
  appDialog(
    "Open an app in " + vm.name,
    (target) => void act(key, () => backend().RunApp(vm.name, target)),
    (list, close) => {
      list.textContent = "Loading...";
      backend()
        .InstalledApps(vm.name)
        .then(
          (apps) => {
            list.replaceChildren(
              ...apps.map((app) =>
                button(app.window ? app.name : app.name + " (terminal)", () => {
                  close();
                  void act(key, () => backend().RunCatalogApp(vm.name, app.id));
                }, { title: app.description }),
              ),
            );
            if (apps.length === 0) {
              list.textContent = "None yet. Install some with More, Install programs.";
            }
          },
          (e) => (list.textContent = "Cannot list the installed programs: " + errorText(e)),
        );
    },
  );
}

function installDialog(vm: VM): void {
  const list = h("div", { class: "checks" }, "Loading...");
  let boxes: HTMLInputElement[] = [];
  dialog({
    title: "Install programs in " + vm.name,
    content: [list, h("p", { class: "hint" }, "Downloads run in the VM and leave through its exit.")],
    submit: "Install",
    validate: () => (boxes.some((b) => b.checked) ? "" : "Choose a program to install."),
    onSubmit: () => {
      const ids = boxes.filter((b) => b.checked).map((b) => b.value);
      void act(["vm:" + vm.name], () => backend().InstallApps(vm.name, ids));
    },
  });
  backend()
    .Catalog()
    .then(
      (catalog) => {
        const rows = catalog.map((app) => checkbox(app.name + " - " + app.description, false, { value: app.id }));
        boxes = rows.map((r) => r.box);
        list.replaceChildren(...rows.map((r) => r.el));
      },
      (e) => (list.textContent = "Cannot read the catalog: " + errorText(e)),
    );
}

function settingsDialog(vm: VM): void {
  const keep = { value: "", label: "Leave as it is" };
  const clipboard = select(
    [
      keep,
      { value: "off", label: "Off" },
      { value: "to-guest", label: "Host to VM" },
      { value: "to-host", label: "VM to host" },
      { value: "both", label: "Both ways" },
    ],
    "",
  );
  const drop = select([keep, { value: "on", label: "On" }, { value: "off", label: "Off" }], "");
  const audio = select(
    [keep, { value: "off", label: "Off" }, { value: "out", label: "VM sound on the host speakers" }, { value: "both", label: "Speakers and host microphone" }],
    "",
  );
  dialog({
    title: "App settings of " + vm.name,
    content: [
      field("Clipboard", clipboard),
      field("Drop files", drop, "Files dropped on the VM's windows upload into ~/Downloads in the VM."),
      field("Audio", audio),
    ],
    submit: "Apply",
    validate: () => (clipboard.value + drop.value + audio.value === "" ? "Choose at least one setting to change." : ""),
    onSubmit: () =>
      void act(["vm:" + vm.name], () => backend().ConfigureApps(vm.name, { clipboard: clipboard.value, drop: drop.value, audio: audio.value })),
  });
}

function deleteDialog(vm: VM): void {
  dialog({
    title: "Delete " + vm.name + "?",
    content: [h("p", null, "This deletes the VM ", h("strong", null, vm.name), " and its disk. It cannot be undone.")],
    submit: "Delete " + vm.name,
    danger: true,
    onSubmit: () => void act(["vm:" + vm.name], () => backend().DeleteVM(vm.name, true)),
  });
}

function rows(vm: VM): HTMLTableRowElement[] {
  const key = ["vm:" + vm.name];
  const working = busy.has(key[0]);
  // a VM that is only being waited for can be stopped, looked at and
  // deleted, which ends the wait
  const waiting = working && onlyFollowed(vm.name) && vm.state === "starting";
  const locked = working && !waiting;
  const up = vm.state === "ready" || vm.state === "failed";
  const k = (action: string) => "vm:" + vm.name + ":" + action;
  const needsUp = up ? undefined : "Start the VM first";
  const main = h(
    "tr",
    { class: working ? "working" : "" },
    h(
      "td",
      { class: "grow" },
      h("div", { class: "name" }, h("strong", { title: vm.name }, vm.name), vm.default && badge("default", "accent")),
      h("div", { class: "sub" }, [vm.image, vm.cpus + " CPUs", memory(vm.mem_mb) + " memory", vm.disk_gb + " GB disk"].join(", ")),
    ),
    h("td", { class: "fit" }, badge(stateText(vm), "state-" + vm.state)),
    h("td", { class: "grow" }, h("div", { class: "name" }, h("span", { title: vm.exit }, vm.exit)), !vm.exit_own && h("div", { class: "sub" }, "default exit")),
    h(
      "td",
      { class: "actions" },
      working && h("span", { class: "spinner", role: "status", "aria-label": "Working" }),
      vm.state === "stopped"
        ? button("Start", () => void actStart(vm.name, () => backend().StartVM(vm.name)), { kind: "primary", disabled: locked, key: k("power") })
        : button("Stop", () => void act(key, () => backend().StopVM(vm.name)), { disabled: locked, key: k("power") }),
      button("Terminal", () => void act([], () => backend().OpenTerminal(vm.name)), { disabled: locked, key: k("terminal") }),
      button("App", () => vmAppDialog(vm), { disabled: working || !up, title: needsUp, key: k("app") }),
      button("Files", () => void act(key, () => backend().OpenFiles(vm.name)), { disabled: working || !up, title: needsUp, key: k("files") }),
      menuButton(
        "More",
        [
          { label: "App settings", action: () => settingsDialog(vm), disabled: waiting },
          { label: "Install programs", action: () => installDialog(vm), disabled: vm.state !== "ready" },
          { label: "Change exit", action: () => exitDialog(vm.name, vm.exit), disabled: waiting },
          { label: "Set as default", action: () => void act(key, () => backend().SetDefaultVM(vm.name)), disabled: waiting || vm.default },
          { label: "Delete", action: () => deleteDialog(vm), danger: true },
        ],
        { disabled: locked, key: k("more") },
      ),
    ),
  );
  if (vm.state !== "failed" || vm.hint === "") {
    return [main];
  }
  return [main, h("tr", { class: "detail" }, h("td", { colspan: 4, class: "hint-failed" }, vm.hint))];
}

export function render(): HTMLElement {
  const create = () => nav.go("create");
  const page = h("div", { class: "page" }, pageHeader("Virtual machines", button("New VM", create, { kind: "primary", key: "vms:new" })));
  if (data.errors.vms) {
    page.append(h("p", { class: "error banner", role: "alert" }, "Cannot read the VM list: " + data.errors.vms));
  }
  if (!data.vms) {
    page.append(h("p", { class: "muted" }, data.errors.vms ? "" : "Loading..."));
  } else if (data.vms.length === 0) {
    page.append(empty("No VMs yet", "Create one to get started.", button("Create your first VM", create, { kind: "primary", key: "vms:first" })));
  } else {
    page.append(table(["Name", "State", "Exit", "Actions"], data.vms.flatMap(rows)));
  }
  return page;
}
