import { backend, type Exit } from "./api";
import {
  badge,
  banner,
  button,
  callout,
  checkbox,
  dialog,
  field,
  h,
  input,
  keyed,
  loading,
  menuButton,
  mono,
  moreButton,
  redraw,
  setFieldError,
  stagger,
  tag,
  type Child,
} from "./dom";
import { t } from "./i18n";
import { icon } from "./icons";
import { act, busy, data } from "./store";
import { exitDialog } from "./vms";

// The CLI's rule, exit.ValidName.
const nameRule = /^[a-zA-Z][a-zA-Z0-9_-]{0,31}$/;
// What the backend takes as host:port: a name or an IPv4 address, or an
// IPv6 address in brackets.
const serverRule = /^([^\s:[\]-][^\s:[\]]*|\[[0-9a-fA-F:.]+\]):\d+$/;

function checkName(name: string): string {
  if (!nameRule.test(name)) {
    return t.exits.nameBad;
  }
  if (name === "direct" || data.exits?.some((e) => e.name === name)) {
    return t.exits.nameTaken(name);
  }
  return "";
}

const nameInput = () => input({ class: "input input--mono", placeholder: "my-exit" });

function addDialog(): void {
  const name = nameInput();
  const server = input({ class: "input input--mono", placeholder: "vps.example.com:18443" });
  const token = input({ class: "input input--mono", type: "password" });
  dialog({
    title: [t.exits.agentTitle],
    text: t.exits.agentText(mono("pvm exit-agent")),
    content: [
      field(t.exits.name, name),
      field(t.exits.server, server, t.exits.serverHint),
      field(t.exits.token, token, t.exits.tokenHint),
      callout("info", "note", t.exits.probe),
    ],
    submit: t.exits.agentSubmit,
    validate: () => {
      setFieldError(name, checkName(name.value));
      setFieldError(server, serverRule.test(server.value) ? "" : t.exits.serverBad);
      setFieldError(token, /^\S+$/.test(token.value) ? "" : t.exits.tokenBad);
      return ![name, server, token].some((el) => el.hasAttribute("aria-invalid"));
    },
    onSubmit: () => void act(["exit:" + name.value], () => backend().AddExit({ name: name.value, server: server.value, token: token.value })),
  });
}

function addWarpDialog(): void {
  const name = nameInput();
  dialog({
    title: [t.exits.warpTitle],
    text: [t.exits.warpText],
    content: [field(t.exits.name, name)],
    submit: t.exits.warpSubmit,
    validate: () => {
      setFieldError(name, checkName(name.value));
      return !name.hasAttribute("aria-invalid");
    },
    onSubmit: () => void act(["exit:" + name.value], () => backend().AddWarpExit(name.value)),
  });
}

function testDialog(exit: Exit): void {
  const speed = checkbox([t.exits.testSpeed], false);
  dialog({
    title: t.exits.testTitle(mono(exit.name)),
    text: [t.exits.testText],
    content: [speed.el],
    submit: t.exits.test,
    submitIcon: "bolt",
    narrow: true,
    onSubmit: () => void act(["exit:" + exit.name], () => backend().TestExit(exit.name, speed.box.checked)),
  });
}

function removeDialog(exit: Exit): void {
  const users = [
    exit.default && t.exits.usedAsDefault,
    exit.vms.length > 0 && t.exits.usedByVms(exit.vms),
    exit.rules.length > 0 && t.exits.usedByRules(exit.rules),
  ].filter((u): u is string => u !== false);
  dialog({
    title: t.exits.removeTitle(mono(exit.name)),
    content: [
      h("p", null, ...t.exits.removeText(h("strong", { lang: "en" }, exit.name))),
      users.length > 0 &&
        callout(
          "warn",
          "alert",
          h("strong", null, t.exits.stillUsed(exit.name)),
          h("ul", null, ...users.map((u) => h("li", null, u))),
          h("p", null, t.exits.notChecked),
        ),
    ],
    submit: t.exits.removeSubmit(exit.name),
    submitIcon: "trash",
    danger: true,
    narrow: true,
    onSubmit: () => void act(["exit:" + exit.name], () => backend().RemoveExit(exit.name, true)),
  });
}

function setDefault(name: string): void {
  void act(["exit:" + name], () => backend().SetDefaultExit(name));
}

const sep = (): HTMLElement => h("span", { class: "sep", "aria-hidden": "true" }, "·");

// between puts a dot between the parts that are there.
const between = (parts: Child[]): Child[] => parts.filter(Boolean).flatMap((part, i) => (i === 0 ? [part] : [sep(), part]));

function usedBy(vms: string[], rules: string[]): HTMLElement {
  const note = rules.length > 0 ? t.exits.rules(rules.length) : vms.length === 0 ? t.exits.nothing : "";
  return h(
    "div",
    { class: "meta meta--wide" },
    h("span", { class: "meta-label" }, t.exits.usedBy),
    h(
      "span",
      { class: "tag-row" },
      ...vms.map((name) => tag(name)),
      note !== "" && h("span", { class: "meta-note", "data-tip": rules.length > 0 ? rules.join(", ") : undefined }, note),
    ),
  );
}

const busyBar = (name: string): HTMLElement => h("div", { class: "busybar", role: "progressbar", "aria-label": t.common.workingOn(name) });

function row(exit: Exit): HTMLElement {
  const working = busy.has("exit:" + exit.name);
  const k = (action: string) => "exit:" + exit.name + ":" + action;
  const kind = tag(exit.kind === "warp" ? t.exits.warp : t.exits.agent);
  if (exit.kind !== "warp") {
    kind.dataset.tip = t.exits.agentHint;
  }
  return h(
    "article",
    { class: "item", "data-busy": working ? "true" : undefined },
    working && busyBar(exit.name),
    h(
      "div",
      { class: "item-row" },
      h(
        "div",
        { class: "item-id" },
        h("div", { class: "item-title" }, h("h2", { class: "item-name", lang: "en" }, exit.name), exit.default && badge(t.common.isDefault, "accent"), kind),
        h(
          "p",
          { class: "item-sub", lang: "en" },
          exit.endpoint !== "" && h("span", { class: "mono" }, exit.endpoint),
          h("span", { class: "item-specs" }, ...between([exit.tz !== "" && h("span", null, exit.tz), exit.locale !== "" && h("span", null, exit.locale)])),
        ),
      ),
      h("div", { class: "item-meta" }, usedBy(exit.vms, exit.rules)),
      h(
        "div",
        { class: "item-actions" },
        // `pvm exit test --speed` is for agent exits only
        button(t.exits.test, () => (exit.kind === "warp" ? void act(["exit:" + exit.name], () => backend().TestExit(exit.name, false)) : testDialog(exit)), {
          size: "sm",
          icon: "bolt",
          disabled: working,
          key: k("test"),
        }),
        moreButton(
          t.common.moreFor(exit.name),
          [
            { label: t.exits.setDefault, icon: "star", action: () => setDefault(exit.name), disabled: exit.default },
            { label: t.exits.remove, icon: "trash", action: () => removeDialog(exit), danger: true, divided: true },
          ],
          { disabled: working, key: k("more") },
        ),
      ),
    ),
  );
}

const directVMs = (): string[] => (data.vms ?? []).filter((vm) => vm.exit === "direct").map((vm) => vm.name);

// directRow stands for no exit at all, which can be the default too.
function directRow(): HTMLElement {
  const isDefault = data.defaultExit === "direct";
  const working = busy.has("exit:direct");
  return h(
    "article",
    { class: "item item--plain", "data-busy": working ? "true" : undefined },
    working && busyBar("direct"),
    h(
      "div",
      { class: "item-row" },
      h(
        "div",
        { class: "item-id" },
        h("div", { class: "item-title" }, h("h2", { class: "item-name", lang: "en" }, "direct"), isDefault && badge(t.common.isDefault, "accent"), tag(t.exits.noExit)),
        h("p", { class: "item-sub" }, t.exits.directText),
      ),
      h("div", { class: "item-meta" }, usedBy(directVMs(), [])),
      h(
        "div",
        { class: "item-actions" },
        button(t.exits.setDefault, () => setDefault("direct"), { size: "sm", disabled: working || isDefault, key: "exit:direct:default" }),
      ),
    ),
  );
}

// The rows of the list: the exits, then direct.
type Entry = { exit: Exit } | { exit: null };

const head = h("header", { class: "page-head" });
const notice = h("div", { class: "slot" });
const rest = h("div", { class: "slot" });
const list = h("div", { class: "list" });
const after = h("div", { class: "slot" });
const page = h("div", { class: "page" }, head, h("div", { class: "page-body" }, notice, rest, list, after));

export function enter(): void {
  stagger(list);
}

export function render(): HTMLElement {
  const all = data.exits;
  const def = data.defaultExit;
  const error = data.errors.exits ?? "";
  const known = (name: string) => name === "direct" || (all ?? []).some((e) => e.name === name);
  const defaultGone = all !== null && def !== null && !known(def);

  redraw(head, [all?.length, def], () => [
    h("h1", { class: "page-title" }, t.exits.title),
    all && h("span", { class: "page-sub" }, all.length === 0 ? t.exits.none : def === null ? "" : t.exits.summary(all.length, known(def) ? def : t.exits.removed(def))),
    h(
      "div",
      { class: "page-actions" },
      menuButton(
        t.exits.add,
        [
          { label: t.exits.addAgent, icon: "server", action: addDialog },
          { label: t.exits.addWarp, icon: "cloud", action: addWarpDialog },
        ],
        { kind: "primary", icon: "plus", key: "exits:add" },
      ),
    ),
  ]);
  redraw(notice, [error, defaultGone, def], () => [
    error !== "" && banner(t.exits.readError, error),
    defaultGone && callout("warn", "alert", ...t.exits.defaultGone(def ?? "", mono("pvm route default --via <exit>"))),
  ]);
  redraw(rest, [all === null, error !== ""], () => [!all && error === "" && loading()]);

  list.hidden = !all;
  const entries: Entry[] = all ? [...all.map((exit) => ({ exit })), { exit: null }] : [];
  keyed(
    list,
    entries,
    (e) => (e.exit ? "exit:" + e.exit.name : "direct"),
    (e) => (e.exit ? [e.exit, busy.has("exit:" + e.exit.name)] : [def === "direct", busy.has("exit:direct"), directVMs()]),
    (e) => (e.exit ? row(e.exit) : directRow()),
  );

  const vms = all ? (data.vms ?? []) : [];
  redraw(after, [all?.length === 0, vms.map((vm) => [vm.name, vm.exit, vm.exit_own, known(vm.exit), busy.has("vm:" + vm.name)])], () => [
    all?.length === 0 && h("p", { class: "hint" }, t.exits.emptyHint),
    vms.length > 0 &&
      h(
        "section",
        { class: "section", "aria-labelledby": "exits-per-vm" },
        h("div", { class: "section-head" }, h("h2", { class: "section-title", id: "exits-per-vm" }, t.exits.perVm)),
        h(
          "div",
          { class: "panel panel--wide" },
          ...vms.map((vm) =>
            h(
              "div",
              { class: "setting setting--tight" },
              h("div", { class: "setting-main" }, h("span", { class: "setting-title", lang: "en" }, vm.name)),
              h(
                "div",
                { class: "setting-value" },
                icon("route", "sm"),
                known(vm.exit) ? h("span", { lang: "en" }, vm.exit) : h("span", null, t.exits.removed(vm.exit)),
                !vm.exit_own && badge(t.common.defaultExit),
                !known(vm.exit) && badge(t.exits.noNetwork, "danger"),
              ),
              button(t.exits.changeExit, () => exitDialog(vm.name, vm.exit), {
                kind: "ghost",
                size: "sm",
                label: t.exits.changeExitOf(vm.name),
                disabled: busy.has("vm:" + vm.name),
                key: "route:" + vm.name,
              }),
            ),
          ),
        ),
      ),
  ]);
  return page;
}
