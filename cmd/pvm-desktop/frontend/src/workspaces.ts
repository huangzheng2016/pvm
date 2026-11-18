import { backend, errorText, type Workspace } from "./api";
import { badge, button, checkbox, dialog, empty, field, h, menuButton, pageHeader, select, table } from "./dom";
import { act, busy, data, nav } from "./store";
import { appDialog } from "./vms";

// The CLI's rule, workspace.ValidID.
const idRule = /^[a-z0-9][a-z0-9-]{0,62}$/;
// An absolute Windows path, as the backend requires: a drive or a share.
const absolute = /^([a-zA-Z]:[\\/]|\\\\)/;

const modes = [
  { value: "mount", label: "mount - one shared copy: the VM works on the host folder" },
  { value: "sync", label: "sync - a copy on each side, pushed and pulled" },
  { value: "copy", label: "copy - uploaded once into the VM" },
];

function createDialog(): void {
  const id = h("input", { type: "text", spellcheck: "false", autocomplete: "off", placeholder: "my-project" });
  const vm = select((data.vms ?? []).map((v) => ({ value: v.name, label: v.name })), data.settings?.default_vm ?? "");
  if (vm.value === "") {
    vm.selectedIndex = 0;
  }
  const mode = select(modes, "mount");
  const path = h("input", { type: "text", spellcheck: "false", autocomplete: "off" });
  const choose = button("Choose...", () => {
    backend()
      .PickFolder()
      .then(
        (folder) => {
          if (folder !== "") {
            path.value = folder;
            path.dispatchEvent(new Event("input", { bubbles: true }));
          }
        },
        (e) => form.fail("Cannot choose a folder: " + errorText(e)),
      );
  });
  const readOnly = checkbox("Read-only: the VM cannot change the host folder", false);
  const confirmed = checkbox("Let pvm start the VM if it is stopped, and upload more than 1 GiB", false);
  const modeChanged = () => {
    readOnly.box.disabled = mode.value !== "mount";
    if (readOnly.box.disabled) {
      readOnly.box.checked = false;
    }
  };
  mode.addEventListener("change", modeChanged);
  const form = dialog({
    title: "Create a workspace",
    content: [
      field("Id", id, "The folder shows in the VM as /workspace/<id>."),
      field("VM", vm),
      field("Mode", mode),
      h("div", { class: "field" }, h("span", { class: "field-label" }, "Host folder"), h("div", { class: "row" }, path, choose)),
      readOnly.el,
      confirmed.el,
    ],
    submit: "Create workspace",
    validate: () => {
      if (!idRule.test(id.value)) {
        return "The id takes lowercase letters, digits and -, at most 63, starting with a letter or digit.";
      }
      if (data.workspaces?.some((w) => w.id === id.value)) {
        return "A workspace named " + id.value + " already exists.";
      }
      if (vm.value === "") {
        return "Create a VM first.";
      }
      return absolute.test(path.value.trim()) ? "" : "Choose the host folder, or type its full path, such as C:\\Users\\me\\project.";
    },
    onSubmit: () =>
      void act(["workspace:" + id.value], () =>
        backend().CreateWorkspace({
          id: id.value,
          vm: vm.value,
          mode: mode.value,
          host_path: path.value.trim(),
          read_only: readOnly.box.checked,
          confirmed: confirmed.box.checked,
        }),
      ),
  });
  path.setAttribute("aria-label", "Host folder");
}

function removeDialog(ws: Workspace): void {
  dialog({
    title: "Remove workspace " + ws.id + "?",
    content: [
      h("p", null, "This removes the record of ", h("strong", null, ws.id), " only. The files in the host folder and in the VM are not touched."),
    ],
    submit: "Remove " + ws.id,
    danger: true,
    onSubmit: () => void act(["workspace:" + ws.id], () => backend().RemoveWorkspace(ws.id, true)),
  });
}

function when(stamp: string): string {
  const t = new Date(stamp);
  return stamp === "" || Number.isNaN(t.getTime()) ? "never" : t.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function row(ws: Workspace): HTMLTableRowElement {
  const key = ["workspace:" + ws.id];
  const working = busy.has(key[0]);
  const k = (action: string) => "workspace:" + ws.id + ":" + action;
  const transfers = ws.mode !== "mount";
  const sync = (direction: "push" | "pull") => () => void act(key, () => backend().SyncWorkspace(ws.id, direction));
  return h(
    "tr",
    { class: working ? "working" : "" },
    h(
      "td",
      { class: "grow" },
      h("div", { class: "name" }, h("strong", { title: ws.id }, ws.id), badge(ws.mode), ws.read_only && badge("read-only")),
      h("div", { class: "sub name" }, h("span", { title: ws.vm }, ws.vm === "" ? "no VM" : "VM " + ws.vm)),
    ),
    h(
      "td",
      { class: "grow" },
      h("div", { class: "name" }, h("span", { title: ws.host_path }, ws.host_path)),
      h("div", { class: "sub name" }, h("span", { title: "In the VM: " + ws.guest_path }, ws.guest_path)),
    ),
    h("td", { class: "fit" }, transfers ? when(ws.last_transfer) : h("span", { class: "muted" }, "live")),
    h(
      "td",
      { class: "actions" },
      working && h("span", { class: "spinner", role: "status", "aria-label": "Working" }),
      button("Folder", () => void act([], () => backend().OpenWorkspaceFolder(ws.id)), { disabled: working, title: "Open the host folder", key: k("folder") }),
      button("Terminal", () => void act([], () => backend().OpenWorkspaceTerminal(ws.id)), { disabled: working, key: k("terminal") }),
      button("App", () => appDialog("Open an app in workspace " + ws.id, (target) => void act(key, () => backend().RunWorkspaceApp(ws.id, target))), {
        disabled: working,
        key: k("app"),
      }),
      button("Files", () => void act(key, () => backend().OpenWorkspaceFiles(ws.id)), { disabled: working, key: k("files") }),
      menuButton(
        "More",
        [
          ...(transfers ? [{ label: "Push to the VM", action: sync("push") }, { label: "Pull from the VM", action: sync("pull") }] : []),
          { label: "Remove", action: () => removeDialog(ws), danger: true },
        ],
        { disabled: working, key: k("more") },
      ),
    ),
  );
}

export function render(): HTMLElement {
  const page = h("div", { class: "page" }, pageHeader("Workspaces", button("New workspace", createDialog, { kind: "primary", key: "workspaces:new" })));
  if (data.errors.workspaces) {
    page.append(h("p", { class: "error banner", role: "alert" }, "Cannot read the workspaces: " + data.errors.workspaces));
  }
  if (!data.workspaces) {
    page.append(h("p", { class: "muted" }, data.errors.workspaces ? "" : "Loading..."));
  } else if (data.workspaces.length === 0) {
    const first =
      data.vms?.length === 0
        ? button("Create a VM first", () => nav.go("create"), { kind: "primary", key: "workspaces:vm" })
        : button("Create a workspace", createDialog, { kind: "primary", key: "workspaces:first" });
    page.append(empty("No workspaces yet", "A workspace binds a host folder to a folder in a VM, so terminals and apps start there.", first));
  } else {
    page.append(table(["Workspace", "Folder", "Last transfer", "Actions"], data.workspaces.map(row)));
  }
  return page;
}
