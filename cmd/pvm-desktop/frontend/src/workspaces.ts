import { backend, errorText, type Workspace } from "./api";
import {
  badge,
  banner,
  button,
  checkbox,
  dialog,
  dot,
  empty,
  field,
  h,
  held,
  iconButton,
  input,
  keyed,
  loading,
  mono,
  moreButton,
  redraw,
  select,
  setFieldError,
  stagger,
  type MenuItem,
} from "./dom";
import { language, t } from "./i18n";
import { icon } from "./icons";
import { act, busy, data, nav } from "./store";
import { appDialog } from "./vms";

// The CLI's rule, workspace.ValidID.
const idRule = /^[a-z0-9][a-z0-9-]{0,62}$/;
// An absolute Windows path, as the backend requires: a drive or a share.
const absolute = /^([a-zA-Z]:[\\/]|\\\\)/;

const modes = ["mount", "sync", "copy"] as const;

function createDialog(): void {
  const id = input({ class: "input input--mono", placeholder: "my-project" });
  const vm = select((data.vms ?? []).map((v) => ({ value: v.name, label: v.name })), data.settings?.default_vm ?? "");
  if (vm.value === "") {
    vm.selectedIndex = 0;
  }
  // the id and the VM share a row, and the line under it
  const idError = h("p", { class: "field-error", role: "alert" });
  const guest = mono("/workspace/<id>");
  id.addEventListener("input", () => (guest.textContent = "/workspace/" + (id.value === "" ? "<id>" : id.value)));

  const radios = modes.map((mode, i) => {
    const box = h("input", { type: "radio", name: "workspace-mode", value: mode });
    box.checked = i === 0;
    return { box, el: h("label", { class: "check" }, box, h("span", null, h("strong", { lang: "en" }, mode), t.workspaces[mode])) };
  });
  const mode = () => radios.find((r) => r.box.checked)?.box.value ?? "mount";

  const path = input({ class: "input input--mono" });
  const choose = button(
    t.workspaces.choose,
    () => {
      backend()
        .PickFolder()
        .then(
          (folder) => {
            if (folder !== "") {
              path.value = folder;
              path.dispatchEvent(new Event("input", { bubbles: true }));
            }
          },
          (e) => form.fail(t.workspaces.chooseError + errorText(e)),
        );
    },
    { icon: "folder" },
  );
  const readOnly = checkbox([t.workspaces.readOnlyCheck], false);
  const confirmed = checkbox([t.workspaces.confirmCheck], false);
  // read-only is a property of a mount
  const modeChanged = () => {
    readOnly.box.disabled = mode() !== "mount";
    readOnly.el.toggleAttribute("data-disabled", readOnly.box.disabled);
    if (readOnly.box.disabled) {
      readOnly.box.checked = false;
    }
  };
  radios.forEach((r) => r.box.addEventListener("change", modeChanged));

  const form = dialog({
    title: [t.workspaces.newTitle],
    content: [
      h(
        "div",
        { class: "form-grid form-grid--id" },
        field(t.workspaces.id, id),
        field(t.workspaces.vm, vm),
        h("div", { class: "field field--span field--tight" }, idError, h("p", { class: "hint" }, ...t.workspaces.idHint(guest))),
      ),
      h("div", { class: "field", role: "radiogroup", "aria-labelledby": "workspace-mode" }, h("span", { class: "label", id: "workspace-mode" }, t.workspaces.mode), ...radios.map((r) => r.el)),
      field(t.workspaces.hostFolder, h("div", { class: "input-row" }, path, choose)),
      h("div", { class: "field" }, readOnly.el, confirmed.el),
    ],
    submit: t.workspaces.newSubmit,
    validate: () => {
      const taken = data.workspaces?.some((w) => w.id === id.value);
      setFieldError(id, !idRule.test(id.value) ? t.workspaces.idBad : taken ? t.workspaces.idTaken(id.value) : "", idError);
      setFieldError(path, absolute.test(path.value.trim()) ? "" : t.workspaces.hostBad);
      if (id.hasAttribute("aria-invalid") || path.hasAttribute("aria-invalid")) {
        return false;
      }
      if (vm.value === "") {
        form.fail(t.workspaces.vmFirst);
        return false;
      }
      return true;
    },
    onSubmit: () =>
      void act(["workspace:" + id.value], () =>
        backend().CreateWorkspace({
          id: id.value,
          vm: vm.value,
          mode: mode(),
          host_path: path.value.trim(),
          read_only: readOnly.box.checked,
          confirmed: confirmed.box.checked,
        }),
      ),
  });
}

function removeDialog(ws: Workspace): void {
  dialog({
    title: t.workspaces.removeTitle(mono(ws.id)),
    content: [h("p", null, ...t.workspaces.removeText(h("strong", { lang: "en" }, ws.id)))],
    submit: t.workspaces.removeSubmit(ws.id),
    submitIcon: "trash",
    danger: true,
    narrow: true,
    onSubmit: () => void act(["workspace:" + ws.id], () => backend().RemoveWorkspace(ws.id, true)),
  });
}

// ago says how long since a transfer; the exact time is in the tooltip.
function ago(stamp: string): string {
  const at = new Date(stamp).getTime();
  if (stamp === "" || Number.isNaN(at)) {
    return t.workspaces.never;
  }
  const minutes = Math.max(0, Math.round((Date.now() - at) / 60000));
  if (minutes < 1) {
    return t.workspaces.justNow;
  }
  if (minutes < 60) {
    return t.workspaces.minutesAgo(minutes);
  }
  const hours = Math.round(minutes / 60);
  return hours < 24 ? t.workspaces.hoursAgo(hours) : t.workspaces.daysAgo(Math.round(hours / 24));
}

function when(stamp: string): HTMLElement {
  const at = new Date(stamp);
  if (stamp === "" || Number.isNaN(at.getTime())) {
    return h("span", null, t.workspaces.never);
  }
  return h("time", { datetime: stamp, "data-tip": at.toLocaleString(language(), { dateStyle: "medium", timeStyle: "short" }) }, ago(stamp));
}

const pathLine = (name: string, path: string): HTMLElement =>
  h("span", { class: "path" }, h("span", { class: "path-key" }, name), h("span", { class: "mono", lang: "en", "data-tip": path }, path));

function row(ws: Workspace): HTMLElement {
  const key = ["workspace:" + ws.id];
  const working = busy.has(key[0]);
  const k = (action: string) => "workspace:" + ws.id + ":" + action;
  const transfers = ws.mode !== "mount";
  const sync = (direction: "push" | "pull") => () => void act(key, () => backend().SyncWorkspace(ws.id, direction));
  const mode = badge(ws.mode);
  mode.lang = "en";
  const menu: MenuItem[] = [
    ...(transfers
      ? [
          { label: t.workspaces.push, icon: "upload" as const, action: sync("push") },
          { label: t.workspaces.pull, icon: "download" as const, action: sync("pull") },
        ]
      : []),
    { label: t.workspaces.remove, icon: "trash", action: () => removeDialog(ws), danger: true, divided: transfers },
  ];
  return h(
    "article",
    { class: "item item--workspace", "data-busy": working ? "true" : undefined },
    working && h("div", { class: "busybar", role: "progressbar", "aria-label": t.common.workingOn(ws.id) }),
    h(
      "div",
      { class: "item-row" },
      h(
        "div",
        { class: "item-id" },
        h(
          "div",
          { class: "item-title" },
          h("h2", { class: "item-name", lang: "en" }, ws.id),
          mode,
          ws.read_only && badge(t.workspaces.readOnly, "outline", icon("lock")),
        ),
        h("p", { class: "item-sub" }, icon("server", "sm"), ws.vm === "" ? h("span", null, t.workspaces.noVm) : h("span", { lang: "en" }, ws.vm)),
      ),
      h(
        "div",
        { class: "item-meta" },
        h("div", { class: "meta meta--paths" }, pathLine(t.workspaces.host, ws.host_path), pathLine(t.workspaces.vm, ws.guest_path)),
        h(
          "div",
          { class: "meta meta--time" },
          h("span", { class: "meta-label" }, t.workspaces.lastTransfer),
          h("span", { class: "meta-value" }, transfers ? when(ws.last_transfer) : badge(t.workspaces.live, "ok", dot())),
        ),
      ),
      h(
        "div",
        { class: "item-actions" },
        iconButton("folder", t.workspaces.folderOf(ws.id), () => void act([], () => backend().OpenWorkspaceFolder(ws.id)), {
          tip: t.workspaces.folder,
          disabled: working,
          key: k("folder"),
        }),
        iconButton("terminal", t.workspaces.terminalIn(ws.id), () => void act([], () => backend().OpenWorkspaceTerminal(ws.id)), {
          tip: t.common.terminal,
          disabled: working,
          key: k("terminal"),
        }),
        iconButton(
          "window",
          t.workspaces.appIn(ws.id),
          () =>
            appDialog(t.workspaces.runTitle(mono(ws.id)), t.workspaces.runText(mono(ws.guest_path)), (target) =>
              void act(key, () => backend().RunWorkspaceApp(ws.id, target)),
            ),
          { tip: t.common.app, disabled: working, key: k("app") },
        ),
        iconButton("files", t.workspaces.filesOf(ws.id), () => void act(key, () => backend().OpenWorkspaceFiles(ws.id)), {
          tip: t.common.files,
          disabled: working,
          key: k("files"),
        }),
        moreButton(t.common.moreFor(ws.id), menu, { disabled: working, key: k("more") }),
      ),
    ),
  );
}

const head = h("header", { class: "page-head" });
const notice = h("div", { class: "slot" });
const rest = h("div", { class: "slot" });
const list = h("div", { class: "list" });
const page = h("div", { class: "page" }, head, h("div", { class: "page-body" }, notice, rest, list));

export function enter(): void {
  stagger(list);
}

export function render(): HTMLElement {
  const all = data.workspaces;
  const error = data.errors.workspaces ?? "";
  const noVMs = data.vms?.length === 0;
  redraw(head, all?.length, () => [
    h("h1", { class: "page-title" }, t.workspaces.title),
    all && all.length > 0 && h("span", { class: "page-sub" }, t.workspaces.summary(all.length)),
    all &&
      all.length > 0 &&
      h("div", { class: "page-actions" }, button(t.workspaces.create, createDialog, { kind: "primary", icon: "plus", key: "workspaces:new" })),
  ]);
  redraw(notice, error, () => [error !== "" && banner(t.workspaces.readError, error)]);
  redraw(rest, [all === null, all?.length === 0, error !== "", noVMs], () => {
    if (!all) {
      return [error === "" && loading()];
    }
    const first = noVMs
      ? button(t.workspaces.vmFirst, () => nav.go("create"), { kind: "primary", key: "workspaces:vm" })
      : button(t.workspaces.create, createDialog, { kind: "primary", icon: "plus", key: "workspaces:first" });
    return [all.length === 0 && empty("folder", t.workspaces.emptyTitle, t.workspaces.emptyText, first)];
  });
  list.hidden = !all || all.length === 0;
  keyed(
    list,
    all ?? [],
    (ws) => ws.id,
    // the age of the last transfer is part of what the row shows
    (ws) => [ws, busy.has("workspace:" + ws.id), ws.mode === "mount" ? "" : ago(ws.last_transfer)],
    row,
  );
  return page;
}

// The ages grow while nothing else changes.
window.setInterval(() => {
  if (page.isConnected && held.count === 0) {
    render();
  }
}, 60000);
