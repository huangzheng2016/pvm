import { backend, type Settings } from "./api";
import { badge, banner, callout, h, loading, mono, select, tag, type Child } from "./dom";
import { choice, choose, t, type Choice } from "./i18n";
import { icon } from "./icons";
import { act, busy, data } from "./store";

const panel = (title: string, ...content: Child[]): HTMLElement =>
  h("section", { class: "panel" }, h("div", { class: "panel-head" }, h("h2", { class: "fieldset-title" }, title)), ...content);

// stacked is a setting whose control sits under its text.
function stacked(label: string, hint: string, control: HTMLElement, said?: Child): HTMLElement {
  return h(
    "div",
    { class: "setting setting--stack" },
    h(
      "div",
      { class: "setting-main" },
      h("div", { class: "setting-head" }, h("label", { class: "setting-title", for: control.id }, label), said),
      h("p", { class: "hint" }, hint),
    ),
    h("div", { class: "setting-control" }, control),
  );
}

function defaultVM(s: Settings): HTMLElement {
  const working = busy.has("settings");
  const names = (data.vms ?? []).map((vm) => vm.name);
  const options = [{ value: "", label: t.settings.none }, ...names.map((n) => ({ value: n, label: n }))];
  const missing = s.default_vm !== "" && !names.includes(s.default_vm);
  if (missing) {
    options.push({ value: s.default_vm, label: t.settings.deleted(s.default_vm) });
  }
  // The screen is not redrawn under a focused field, so the change is
  // acknowledged here, next to it.
  const saved = badge(t.settings.saved, "ok", icon("check"));
  saved.setAttribute("role", "status");
  saved.hidden = true;
  let chosen = s.default_vm;
  const vm = select(options, s.default_vm, {
    id: "settings-default-vm",
    disabled: working,
    "aria-invalid": missing ? "true" : undefined,
    "data-key": "settings:default-vm",
    onchange: () => {
      // The field itself refuses a second change while one runs.
      // Disabling it would drop the keyboard focus.
      if (busy.has("settings")) {
        vm.value = chosen;
        return;
      }
      chosen = vm.value;
      saved.hidden = true;
      void act(["settings"], () => (chosen === "" ? backend().ClearDefaultVM() : backend().SetDefaultVM(chosen))).then((o) => {
        // a failed change redraws nothing: the state is what it was
        if (o.error !== undefined || o.task?.ok === false) {
          vm.value = chosen = data.settings?.default_vm ?? "";
          return;
        }
        saved.hidden = false;
        window.setTimeout(() => (saved.hidden = true), 1600);
      });
    },
  });
  return panel(
    t.settings.defaultVm,
    stacked(t.settings.defaultVmLabel, t.settings.defaultVmHint, vm, saved),
    missing && h("div", { class: "panel-notes" }, callout("warn", "alert", t.settings.defaultVmGone(s.default_vm))),
  );
}

function toggle(id: string, label: string, on: boolean, change: (on: boolean) => void): HTMLElement {
  const box = h("input", {
    type: "checkbox",
    class: "switch",
    role: "switch",
    id,
    disabled: busy.has("settings"),
    "data-key": id,
    onchange: () => change(box.checked),
  });
  box.checked = on;
  return h("div", { class: "setting" }, h("label", { class: "setting-main", for: id }, h("span", { class: "setting-title" }, label)), box);
}

function signIn(s: Settings): HTMLElement {
  return panel(
    t.settings.signIn,
    toggle("settings-daemon-login", t.settings.daemonAtLogin, s.daemon_at_login, (on) => void act(["settings"], () => backend().SetDaemonAtLogin(on))),
    toggle("settings-app-login", t.settings.appAtLogin, s.app_at_login, (on) => void act(["settings"], () => backend().SetAppAtLogin(on))),
  );
}

// The language is the window's own setting: it is not in pvm's config,
// and choosing it runs no command.
function languagePanel(): HTMLElement {
  const lang = select(
    [
      { value: "system", label: t.settings.followSystem },
      { value: "en", label: "English" },
      { value: "zh-CN", label: "简体中文" },
    ],
    choice(),
    { id: "settings-language", "data-key": "settings:language", onchange: () => choose(lang.value as Choice) },
  );
  return panel(t.settings.language, stacked(t.settings.languageLabel, t.settings.languageHint, lang));
}

function about(): HTMLElement {
  const st = data.state!;
  const mismatch = st.daemon_version !== "" && st.app_version !== st.daemon_version;
  const value = (text: string): HTMLElement => (text === "" ? h("span", { class: "muted" }, t.settings.unknown) : mono(text));
  const accelerator = st.accel === "whpx" ? t.settings.whpx : st.accel === "tcg" ? t.settings.tcg : "";
  return panel(
    t.settings.about,
    h(
      "dl",
      { class: "kv" },
      h("dt", { lang: "en" }, "PVM Desktop"),
      h("dd", null, value(st.app_version)),
      h("dt", null, t.settings.daemon),
      h("dd", null, value(st.daemon_version), mismatch && badge(t.settings.differs, "warn")),
      h("dt", null, t.settings.accelerator),
      h("dd", null, st.accel === "" ? value("") : tag(st.accel), accelerator !== "" && h("span", { class: "muted" }, accelerator)),
      h("dt", null, t.settings.dataFolder),
      h("dd", null, value(st.data_dir)),
    ),
    (mismatch || st.tcg_fallback > 0) &&
      h(
        "div",
        { class: "panel-notes" },
        mismatch && callout("warn", "note", ...t.settings.versions(st.app_version, st.daemon_version, mono("pvm upgrade"))),
        st.tcg_fallback > 0 && callout("warn", "note", t.settings.fallback(st.tcg_fallback)),
      ),
  );
}

export function render(): HTMLElement {
  const s = data.settings;
  const error = data.errors.settings ?? "";
  return h(
    "div",
    { class: "page" },
    h("header", { class: "page-head" }, h("h1", { class: "page-title" }, t.settings.title), h("span", { class: "page-sub" }, t.settings.subtitle)),
    h(
      "div",
      { class: "page-body" },
      error !== "" && banner(t.settings.readError, error),
      h(
        "div",
        { class: "cols" },
        h("div", { class: "col" }, s ? defaultVM(s) : error === "" && loading(), s && signIn(s), languagePanel()),
        h("div", { class: "col" }, data.state && about()),
      ),
    ),
  );
}
