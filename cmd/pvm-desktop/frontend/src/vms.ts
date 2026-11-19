import { backend, errorText, type CatalogApp, type VM } from "./api";
import {
  badge,
  banner,
  button,
  callout,
  dialog,
  dot,
  empty,
  field,
  focusKey,
  h,
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
  tag,
  type Child,
} from "./dom";
import { t } from "./i18n";
import { icon, type IconName } from "./icons";
import * as prefs from "./prefs";
import { act, actStart, busy, data, memory, nav, onlyFollowed } from "./store";

const stateKind = { stopped: "", starting: "info", ready: "ok", failed: "danger" } as const;

// The stages a starting VM goes through, as the daemon names them.
const stageNames = ["boot", "provision", "ssh", "agent"];

// stageBars draws where a starting VM is: one bar per stage, with the
// names under them where there is room. A stage the list does not know
// draws nothing.
export function stageBars(stage: string, named = false): HTMLElement | null {
  const at = stageNames.indexOf(stage);
  if (at < 0) {
    return null;
  }
  return h(
    "span",
    {
      class: named ? "stages stages--labeled grow" : "stages",
      role: "img",
      "aria-label": t.common.stage(at + 1, stageNames.length, stage),
      "data-tip": named ? undefined : stageNames.join(", "),
    },
    ...stageNames.map((name, i) => h("span", { class: "stage", "data-state": i < at ? "done" : i === at ? "active" : "todo" }, named && name)),
  );
}

// exitChoices are the values `pvm route --via` takes.
export function exitChoices(): { value: string; label: string }[] {
  return [{ value: "direct", label: t.common.direct }, ...(data.exits ?? []).map((e) => ({ value: e.name, label: e.name }))];
}

export function exitDialog(name: string, current: string): void {
  const choices = exitChoices();
  const gone = !choices.some((c) => c.value === current);
  const via = select(choices, gone ? choices[0].value : current);
  dialog({
    title: t.vms.exitTitle(mono(name)),
    text: t.vms.exitNow(gone ? h("span", null, t.vms.exitRemoved(current)) : mono(current)),
    content: [
      gone && callout("danger", "alert", t.vms.exitGone(current, name)),
      field(t.vms.exitLabel, via, t.vms.exitHint),
      callout("warn", "note", t.vms.exitRestart(name)),
    ],
    submit: t.vms.exitSubmit,
    onSubmit: () => void act(["vm:" + name], () => backend().SetVMExit(name, via.value, true)),
  });
}

// appConfigChoices are the values `pvm app config` takes; keep names the
// empty choice, which leaves a setting alone.
export function appConfigChoices(keep: string): Record<"clipboard" | "drop" | "audio", { value: string; label: string }[]> {
  const first = { value: "", label: keep };
  const c = t.appConfig;
  return {
    clipboard: [first, { value: "off", label: c.off }, { value: "to-guest", label: c.toGuest }, { value: "to-host", label: c.toHost }, { value: "both", label: c.both }],
    drop: [first, { value: "on", label: c.on }, { value: "off", label: c.off }],
    audio: [first, { value: "off", label: c.off }, { value: "out", label: c.audioOut }, { value: "both", label: c.audioBoth }],
  };
}

// programCard is a catalog program to tick. The name and the description
// come from the catalog as they are.
export function programCard(app: CatalogApp, checked: boolean): { box: HTMLInputElement; el: HTMLElement } {
  const box = h("input", { type: "checkbox", value: app.id });
  box.checked = checked;
  const source = app.source === "official" ? t.catalog.official : app.source === "community" ? t.catalog.community : app.source;
  const el = h(
    "label",
    { class: "pick" },
    box,
    h(
      "span",
      { class: "pick-head" },
      h("span", { class: "pick-title", lang: "en" }, app.name),
      tag(app.window ? t.catalog.window : t.catalog.terminal),
      tag(source, app.source !== "official"),
    ),
    h("span", { class: "pick-desc", lang: "en" }, app.description),
  );
  return { box, el };
}

// appDialog asks for a command line or a URL. programs, when given,
// loads the one-click entries shown under the field.
export function appDialog(
  title: Child[],
  text: Child[] | undefined,
  run: (target: string) => void,
  programs?: (list: HTMLElement, close: () => void) => void,
): void {
  const target = input({ class: "input input--mono", placeholder: t.vms.runPlaceholder });
  const list = h("div");
  const d = dialog({
    title,
    text,
    content: [
      field(t.vms.runLabel, target, t.vms.runHint),
      programs && h("div", { class: "field" }, h("span", { class: "label", id: "installed-programs" }, t.vms.installed), list),
    ],
    submit: t.vms.open,
    validate: () => {
      if (target.value.trim() !== "") {
        return true;
      }
      setFieldError(target, t.vms.runEmpty);
      return false;
    },
    onSubmit: () => run(target.value),
  });
  programs?.(list, d.close);
}

function vmAppDialog(vm: VM): void {
  const key = ["vm:" + vm.name];
  appDialog(
    t.vms.runTitle(mono(vm.name)),
    undefined,
    (target) => void act(key, () => backend().RunApp(vm.name, target)),
    (list, close) => {
      list.replaceChildren(loading());
      backend()
        .InstalledApps(vm.name)
        .then(
          (apps) => {
            if (apps.length === 0) {
              list.replaceChildren(h("p", { class: "hint" }, t.vms.noneInstalled));
              return;
            }
            const entries = apps.map((app) =>
              h(
                "button",
                {
                  type: "button",
                  class: "menu-item",
                  "data-tip": app.description,
                  onclick: () => {
                    close();
                    void act(key, () => backend().RunCatalogApp(vm.name, app.id));
                  },
                },
                icon(app.window ? "window" : "terminal"),
                h("span", null, h("span", { lang: "en" }, app.name), !app.window && h("small", null, t.vms.inTerminal)),
              ),
            );
            list.replaceChildren(h("div", { class: "dialog-list", role: "group", "aria-labelledby": "installed-programs" }, ...entries));
          },
          (e) => list.replaceChildren(h("p", { class: "field-error", role: "alert" }, t.vms.installedError, h("span", { lang: "en" }, errorText(e)))),
        );
    },
  );
}

function installDialog(vm: VM): void {
  const list = h("div", { class: "pick-grid pick-grid--stack", role: "group", "aria-labelledby": "dialog-title" }, loading());
  let boxes: HTMLInputElement[] = [];
  const chosen = () => boxes.filter((b) => b.checked).map((b) => b.value);
  const d = dialog({
    title: t.vms.installTitle(mono(vm.name)),
    text: [t.vms.installText],
    content: [list],
    submit: t.vms.installSubmit(0),
    submitState: () => ({ disabled: chosen().length === 0, label: t.vms.installSubmit(chosen().length) }),
    onSubmit: () => {
      const ids = chosen();
      void act(["vm:" + vm.name], () => backend().InstallApps(vm.name, ids));
    },
  });
  backend()
    .Catalog()
    .then(
      (catalog) => {
        const cards = catalog.map((app) => programCard(app, false));
        boxes = cards.map((c) => c.box);
        list.replaceChildren(...cards.map((c) => c.el));
        if (catalog.length === 0) {
          list.replaceChildren(h("p", { class: "hint" }, t.vms.catalogEmpty));
        }
      },
      (e) => {
        list.replaceChildren();
        d.fail(t.vms.catalogError + errorText(e));
      },
    );
}

function settingsDialog(vm: VM): void {
  const choices = appConfigChoices(t.vms.keep);
  const clipboard = select(choices.clipboard, "");
  const drop = select(choices.drop, "");
  const audio = select(choices.audio, "");
  dialog({
    title: t.vms.settingsTitle(mono(vm.name)),
    text: [t.vms.settingsText],
    content: [
      field(t.appConfig.clipboard, clipboard),
      field(t.appConfig.drop, drop, ...t.vms.dropHint(mono("~/Downloads"))),
      field(t.appConfig.audio, audio),
    ],
    submit: t.vms.apply,
    // at least one setting has to change
    submitState: () => ({ disabled: clipboard.value + drop.value + audio.value === "" }),
    onSubmit: () =>
      void act(["vm:" + vm.name], () => backend().ConfigureApps(vm.name, { clipboard: clipboard.value, drop: drop.value, audio: audio.value })),
  });
}

function deleteDialog(vm: VM): void {
  dialog({
    title: t.vms.deleteTitle(mono(vm.name)),
    content: [h("p", null, ...t.vms.deleteText(h("strong", { lang: "en" }, vm.name)))],
    submit: t.vms.deleteSubmit(vm.name),
    submitIcon: "trash",
    danger: true,
    narrow: true,
    onSubmit: () => void act(["vm:" + vm.name], () => backend().DeleteVM(vm.name, true)),
  });
}

const sep = (): HTMLElement => h("span", { class: "sep", "aria-hidden": "true" }, "·");

// status is what decides how a VM's row looks, besides the VM itself.
function status(vm: VM): { working: boolean; waiting: boolean } {
  const working = busy.has("vm:" + vm.name);
  // a VM that is only being waited for can be stopped, looked at and
  // deleted, which ends the wait
  return { working, waiting: working && onlyFollowed(vm.name) && vm.state === "starting" };
}

function row(vm: VM): HTMLElement {
  const key = ["vm:" + vm.name];
  const { working, waiting } = status(vm);
  const locked = working && !waiting;
  const up = vm.state === "ready" || vm.state === "failed";
  const starting = vm.state === "starting";
  const k = (action: string) => "vm:" + vm.name + ":" + action;
  const needsUp = (name: string) => (up ? name : t.vms.startFirst);
  return h(
    "article",
    { class: "item", "data-busy": locked ? "true" : undefined },
    locked && h("div", { class: "busybar", role: "progressbar", "aria-label": t.common.workingOn(vm.name) }),
    h(
      "div",
      { class: "item-row" },
      h(
        "div",
        { class: "item-id" },
        h(
          "div",
          { class: "item-title" },
          h("span", { class: "glyph", "data-state": vm.state, "aria-hidden": "true" }),
          h("h2", { class: "item-name", lang: "en" }, vm.name),
          vm.default && badge(t.common.isDefault, "accent"),
        ),
        h(
          "p",
          { class: "item-sub" },
          h("span", { class: "mono" }, vm.image),
          h(
            "span",
            { class: "item-specs" },
            h("span", null, t.vms.cpus(vm.cpus)),
            sep(),
            h("span", null, t.vms.memory(memory(vm.mem_mb))),
            sep(),
            h("span", null, t.vms.disk(vm.disk_gb)),
          ),
        ),
      ),
      h(
        "div",
        { class: "item-meta" },
        h(
          "div",
          { class: "meta meta--status" },
          h("span", { class: "meta-value" }, badge(starting && vm.stage !== "" ? t.vms.startingAt(vm.stage) : t.vms.state[vm.state], stateKind[vm.state], dot(starting))),
          starting && stageBars(vm.stage),
        ),
        h(
          "div",
          { class: "meta meta--exit" },
          h("span", { class: "meta-value" }, icon("route", "sm"), h("span", { lang: "en", "data-tip": vm.exit }, vm.exit)),
          !vm.exit_own && h("span", { class: "meta-note" }, t.common.defaultExit),
        ),
      ),
      h(
        "div",
        { class: "item-actions" },
        vm.state === "stopped"
          ? button(t.vms.start, () => void actStart(vm.name, () => backend().StartVM(vm.name)), {
              kind: "primary",
              size: "sm",
              icon: "play",
              disabled: locked,
              key: k("power"),
            })
          : button(t.vms.stop, () => void act(key, () => backend().StopVM(vm.name)), { size: "sm", icon: "stop", disabled: locked, key: k("power") }),
        iconButton("terminal", t.vms.terminalIn(vm.name), () => void act([], () => backend().OpenTerminal(vm.name)), {
          tip: t.common.terminal,
          disabled: locked,
          key: k("terminal"),
        }),
        iconButton("window", t.vms.appIn(vm.name), () => vmAppDialog(vm), { tip: needsUp(t.common.app), disabled: working || !up, key: k("app") }),
        iconButton("files", t.vms.filesOf(vm.name), () => void act(key, () => backend().OpenFiles(vm.name)), {
          tip: needsUp(t.common.files),
          disabled: working || !up,
          key: k("files"),
        }),
        moreButton(
          t.common.moreFor(vm.name),
          [
            { label: t.vms.appSettings, icon: "sliders", action: () => settingsDialog(vm), disabled: waiting },
            { label: t.vms.install, icon: "download", action: () => installDialog(vm), disabled: vm.state !== "ready" },
            { label: t.vms.changeExit, icon: "route", action: () => exitDialog(vm.name, vm.exit), disabled: waiting },
            { label: t.vms.setDefault, icon: "star", action: () => void act(key, () => backend().SetDefaultVM(vm.name)), disabled: waiting || vm.default },
            { label: t.vms.remove, icon: "trash", action: () => deleteDialog(vm), danger: true, divided: true },
          ],
          { disabled: locked, key: k("more") },
        ),
      ),
    ),
    vm.state === "failed" &&
      vm.hint !== "" &&
      h("div", { class: "item-note item-note--danger", role: "status" }, icon("alert", "sm"), h("span", { lang: "en" }, vm.hint)),
  );
}

// The list shows as rows or as cards; the choice is the window's own.
type Layout = "rows" | "cards";
let layout: Layout = prefs.load("vm-layout") === "cards" ? "cards" : "rows";

function layoutSwitch(): HTMLElement {
  const option = (value: Layout, name: IconName, label: string) =>
    h(
      "button",
      {
        type: "button",
        class: "seg-btn",
        "aria-pressed": String(layout === value),
        "aria-label": label,
        "data-tip": label,
        "data-key": "vms:layout:" + value,
        onclick: () => {
          layout = value;
          prefs.save("vm-layout", value === "rows" ? null : value);
          render();
          focusKey("vms:layout:" + value);
        },
      },
      icon(name),
    );
  return h("div", { class: "seg", role: "group", "aria-label": t.vms.layout }, option("rows", "rows", t.vms.rows), option("cards", "cards", t.vms.cards));
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
  const create = () => nav.go("create");
  const all = data.vms;
  const error = data.errors.vms ?? "";
  redraw(head, [all?.length, all?.filter((vm) => vm.state === "ready").length, layout], () => [
    h("h1", { class: "page-title" }, t.vms.title),
    all && all.length > 0 && h("span", { class: "page-sub" }, t.vms.summary(all.length, all.filter((vm) => vm.state === "ready").length)),
    h(
      "div",
      { class: "page-actions" },
      all && all.length > 0 && layoutSwitch(),
      button(t.vms.create, create, { kind: "primary", icon: "plus", key: "vms:new" }),
    ),
  ]);
  redraw(notice, error, () => [error !== "" && banner(t.vms.readError, error)]);
  redraw(rest, [all === null, all?.length === 0, error !== ""], () => {
    if (!all) {
      return [error === "" && loading()];
    }
    return [
      all.length === 0 &&
        empty("server", t.vms.emptyTitle, t.vms.emptyText, button(t.vms.emptyAction, create, { kind: "primary", icon: "plus", key: "vms:first" })),
    ];
  });
  list.hidden = !all || all.length === 0;
  list.dataset.layout = layout === "cards" ? "grid" : "rows";
  keyed(
    list,
    all ?? [],
    (vm) => vm.name,
    (vm) => [vm, status(vm)],
    row,
  );
  return page;
}
