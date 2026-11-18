// A small DOM builder and the parts every screen shares. Text from VMs
// and the API only ever becomes text nodes and attribute values; nothing
// here parses HTML.

import { copyText } from "./api";
import { language, t } from "./i18n";
import { icon, type IconName } from "./icons";

export type Child = Node | string | null | undefined | false;

type Value = string | number | boolean | undefined | ((event: Event) => void);

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props?: Record<string, Value> | null,
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  for (const [name, value] of Object.entries(props ?? {})) {
    if (typeof value === "function") {
      el.addEventListener(name.replace(/^on/, ""), value);
    } else if (typeof value === "boolean") {
      el.toggleAttribute(name, value);
    } else if (value !== undefined) {
      el.setAttribute(name, String(value));
    }
  }
  el.append(...children.filter((c): c is Node | string => c !== null && c !== undefined && c !== false));
  return el;
}

const classes = (...names: (string | false | undefined)[]): string => names.filter(Boolean).join(" ");

// mono shows a name or a command as pvm writes it, in every language.
export const mono = (text: string): HTMLElement => h("span", { class: "mono", lang: "en" }, text);

// The words are part of what a piece of the page shows.
const signed = (shows: unknown): string => JSON.stringify([language(), shows]);

// redraw fills el again only when what it shows has changed, so a control
// that did not change keeps its focus and the pointer over it.
export function redraw(el: HTMLElement, shows: unknown, build: () => Child[]): void {
  const signature = signed(shows);
  if (el.dataset.shows === signature) {
    return;
  }
  el.dataset.shows = signature;
  el.replaceChildren(...build().filter((c): c is Node | string => c !== null && c !== undefined && c !== false));
}

// keyed makes list show one row per item, in order. A row whose item did
// not change stays as it is; a changed one is built again in its place,
// and only a row for a new key is marked to rise in.
export function keyed<T>(list: HTMLElement, items: T[], key: (item: T) => string, shows: (item: T) => unknown, build: (item: T) => HTMLElement): void {
  const old = new Map<string, HTMLElement>();
  for (const row of list.children) {
    if (row instanceof HTMLElement && row.dataset.row !== undefined) {
      old.set(row.dataset.row, row);
    }
  }
  const rows = items.map((item, index) => {
    const name = key(item);
    const signature = signed(shows(item));
    const had = old.get(name);
    old.delete(name);
    if (had && had.dataset.shows === signature) {
      return had;
    }
    const row = build(item);
    row.dataset.row = name;
    row.dataset.shows = signature;
    if (!had) {
      row.dataset.enter = "true";
      row.style.setProperty("--i", String(index));
    }
    had?.replaceWith(row);
    return row;
  });
  old.forEach((row) => row.remove());
  rows.forEach((row, index) => {
    if (list.children[index] !== row) {
      list.insertBefore(row, list.children[index] ?? null);
    }
  });
}

// stagger makes the rows of a list rise in one after another, as when its
// screen opens.
export function stagger(list: HTMLElement): void {
  [...list.children].forEach((row, index) => {
    if (row instanceof HTMLElement) {
      row.dataset.enter = "true";
      row.style.setProperty("--i", String(index));
    }
  });
}

export interface ButtonOptions {
  kind?: "primary" | "danger" | "ghost";
  size?: "sm" | "lg";
  icon?: IconName;
  disabled?: boolean;
  // label is the accessible name where the text alone does not say what
  // the button acts on.
  label?: string;
  // key names the button across redraws, so the focus can return to it.
  key?: string;
}

export function button(label: string, onclick: () => void, opts: ButtonOptions = {}): HTMLButtonElement {
  return h(
    "button",
    {
      type: "button",
      class: classes("btn", opts.kind && "btn--" + opts.kind, opts.size && "btn--" + opts.size),
      disabled: opts.disabled ?? false,
      "aria-label": opts.label,
      "data-key": opts.key,
      onclick,
    },
    opts.icon && icon(opts.icon, opts.size === "sm" ? "sm" : "md"),
    label,
  );
}

export interface IconButtonOptions {
  // tip is the short name shown on hover; label says it in full.
  tip?: string;
  disabled?: boolean;
  small?: boolean;
  key?: string;
}

// iconButton is a button that shows only an icon. A disabled one keeps the
// focus and its tooltip, which can say why, so it is marked aria-disabled
// and ignores the click.
export function iconButton(name: IconName, label: string, onclick: () => void, opts: IconButtonOptions = {}): HTMLButtonElement {
  const el = h(
    "button",
    {
      type: "button",
      class: classes("icon-btn", opts.small && "icon-btn--sm"),
      "aria-label": label,
      "aria-disabled": opts.disabled ? "true" : undefined,
      "data-tip": opts.tip ?? label,
      "data-key": opts.key,
      onclick: () => {
        if (el.getAttribute("aria-disabled") !== "true") {
          onclick();
        }
      },
    },
    icon(name, opts.small ? "sm" : "md"),
  );
  return el;
}

// wanted is the key of the control that should have the focus but is
// disabled or gone for now, as while its action runs.
let wanted: string | undefined;
document.addEventListener("focusin", () => (wanted = undefined));

// focusKey moves the focus to the control with that key, now or once
// refocus finds it enabled again.
export function focusKey(key: string | undefined): void {
  if (key === undefined) {
    return;
  }
  const el = [...document.querySelectorAll<HTMLElement>("[data-key]")].find((c) => c.dataset.key === key);
  if (el && !el.matches(":disabled")) {
    el.focus();
  } else {
    wanted = key;
  }
}

// refocus runs after a redraw. It never takes the focus from where the
// user has moved it meanwhile.
export function refocus(): void {
  if (document.activeElement === document.body) {
    focusKey(wanted);
  }
}

export const dot = (pulse = false): HTMLElement => h("span", { class: classes("dot", pulse && "dot--pulse"), "aria-hidden": "true" });

export function badge(text: string, kind: "" | "ok" | "info" | "warn" | "danger" | "accent" | "outline" = "", lead?: Child): HTMLElement {
  return h("span", { class: classes("badge", kind !== "" && "badge--" + kind) }, lead, text);
}

export const tag = (text: string, warn = false): HTMLElement => h("span", { class: classes("tag", warn && "tag--warn") }, text);

const calloutIcon: Record<string, IconName> = { warn: "warning", danger: "alert", info: "info" };

// callout is a block of advice: amber for a warning, red for what went
// wrong, grey for a note.
export function callout(kind: "warn" | "danger" | "info", role: "alert" | "note", ...content: Child[]): HTMLElement {
  return h("div", { class: classes("callout", kind !== "warn" && "callout--" + kind), role }, icon(calloutIcon[kind]), h("div", null, ...content));
}

// banner reports a list that could not be read; detail is the backend's
// own words.
export function banner(what: string, detail: string): HTMLElement {
  return h("div", { class: "banner", role: "alert" }, icon("alert"), h("span", null, what, h("span", { lang: "en" }, detail)));
}

export const spinner = (small = false): HTMLElement => h("span", { class: classes("spinner", small && "spinner--sm"), "aria-hidden": "true" });

export const loading = (): HTMLElement => h("p", { class: "loading", role: "status" }, spinner(true), t.common.loading);

export function empty(art: IconName, title: string, text: string, ...actions: Child[]): HTMLElement {
  return h(
    "div",
    { class: "empty" },
    h("div", { class: "empty-art", "aria-hidden": "true" }, icon(art, "xl")),
    h("h2", { class: "empty-title" }, title),
    h("p", { class: "empty-text" }, text),
    ...actions,
  );
}

// One tooltip for the whole page, placed next to whatever carries
// data-tip. It hangs under the body, so no scrolling box cuts it off.
const tip = h("div", { class: "tip", "aria-hidden": "true" });
let tipFor: HTMLElement | null = null;
let tipTimer = 0;
const tipDelay = 350;
// The text of the tooltip a click sent away. It stays away while the
// pointer is on that control, also when a redraw has replaced the control.
let tipQuiet: string | null = null;

function hideTip(forget = true): void {
  window.clearTimeout(tipTimer);
  tip.removeAttribute("data-show");
  if (forget) {
    tipFor = null;
  }
}

function showTip(el: HTMLElement): void {
  // the names of the rail show only while it is too narrow for them
  const rail = el.hasAttribute("data-tip-rail") ? el.closest<HTMLElement>(".nav") : null;
  const text = rail && rail.offsetWidth > 100 ? "" : (el.dataset.tip ?? "");
  if (text === "" || !el.isConnected) {
    return;
  }
  tip.textContent = text;
  // a modal dialog covers everything outside it
  (el.closest("dialog") ?? document.body).append(tip);
  const box = el.getBoundingClientRect();
  let left = box.left + (box.width - tip.offsetWidth) / 2;
  let top = box.bottom + 6;
  if (rail) {
    left = box.right + 10;
    top = box.top + (box.height - tip.offsetHeight) / 2;
  } else if (top + tip.offsetHeight > window.innerHeight - 4) {
    top = box.top - tip.offsetHeight - 6;
  }
  left = Math.max(4, Math.min(left, window.innerWidth - tip.offsetWidth - 4));
  tip.style.transform = "translate(" + Math.round(left) + "px, " + Math.round(top) + "px)";
  tip.dataset.show = "true";
}

const tipTarget = (e: Event): HTMLElement | null => (e.target instanceof Element ? e.target.closest<HTMLElement>("[data-tip]") : null);

document.addEventListener("pointerover", (e) => {
  const el = tipTarget(e);
  if (el === tipFor) {
    return;
  }
  hideTip();
  if (el && el.dataset.tip === tipQuiet) {
    tipFor = el;
    return;
  }
  tipQuiet = null;
  if (el) {
    tipFor = el;
    tipTimer = window.setTimeout(() => showTip(el), tipDelay);
  }
});
document.addEventListener("focusin", (e) => {
  const el = tipTarget(e);
  if (el && el.matches(":focus-visible")) {
    hideTip();
    tipFor = el;
    showTip(el);
  }
});
document.addEventListener("focusout", () => hideTip());
document.addEventListener(
  "pointerdown",
  (e) => {
    tipQuiet = tipTarget(e)?.dataset.tip ?? null;
    hideTip(false);
  },
  true,
);
document.addEventListener("keydown", (e) => e.key === "Escape" && hideTip(false));
document.addEventListener("scroll", () => hideTip(false), true);
document.documentElement.addEventListener("pointerleave", () => hideTip());
window.addEventListener("blur", () => hideTip());

// dropTip takes the tooltip away when a redraw removed what it was for.
export function dropTip(): void {
  if (tipFor && !tipFor.isConnected) {
    hideTip();
  }
}

// held counts open menus: while one is open the list under it is not
// re-rendered. onRelease runs when the last one closes.
export const held = { count: 0, onRelease: () => {} };

export interface MenuItem {
  label: string;
  action: () => void;
  icon?: IconName;
  disabled?: boolean;
  danger?: boolean;
  // divided draws a line above the item.
  divided?: boolean;
}

function attachMenu(trigger: HTMLButtonElement, name: string, items: MenuItem[], key: string | undefined): void {
  let list: HTMLElement | null = null;

  const outside = (e: Event) => {
    if (list && e.target instanceof Node && !list.contains(e.target) && !trigger.contains(e.target)) {
      close();
    }
  };
  // The menu does not follow its row, so it closes when the row moves:
  // not when something else scrolls, as a task's output does by itself.
  const scrolled = (e: Event) => {
    if (e.target instanceof Node && e.target.contains(trigger)) {
      close();
    }
  };
  // Closing lets a held redraw run, which may replace the trigger: the
  // focus goes to the control that has its key now.
  const backToTrigger = () => (key === undefined ? trigger.focus() : focusKey(key));
  function close(): void {
    if (!list) {
      return;
    }
    list.remove();
    list = null;
    trigger.setAttribute("aria-expanded", "false");
    document.removeEventListener("pointerdown", outside, true);
    window.removeEventListener("resize", close);
    window.removeEventListener("blur", close);
    document.removeEventListener("scroll", scrolled, true);
    if (--held.count === 0) {
      held.onRelease();
    }
  }
  function open(): void {
    const entries = items.map((item) =>
      h(
        "button",
        {
          type: "button",
          role: "menuitem",
          class: classes("menu-item", item.danger && "menu-item--danger"),
          disabled: item.disabled ?? false,
          onclick: () => {
            close();
            // a dialog the action opens returns the focus here
            backToTrigger();
            item.action();
          },
        },
        item.icon && icon(item.icon),
        item.label,
      ),
    );
    list = h(
      "div",
      {
        class: "menu",
        role: "menu",
        "aria-label": name,
        onkeydown: (e) => {
          const pressed = (e as KeyboardEvent).key;
          const enabled = entries.filter((b) => !b.disabled);
          const at = enabled.indexOf(document.activeElement as HTMLButtonElement);
          if (pressed === "Escape") {
            close();
            backToTrigger();
          } else if (pressed === "ArrowDown" || pressed === "ArrowUp") {
            e.preventDefault();
            const step = pressed === "ArrowDown" ? 1 : -1;
            enabled[(at + step + enabled.length) % enabled.length]?.focus();
          } else if (pressed === "Home" || pressed === "End") {
            e.preventDefault();
            enabled[pressed === "Home" ? 0 : enabled.length - 1]?.focus();
          } else if (pressed === "Tab") {
            close();
          }
        },
      },
      ...items.flatMap((item, i) => [item.divided && h("div", { class: "menu-sep", role: "separator" }), entries[i]]),
    );
    document.body.append(list);
    const box = trigger.getBoundingClientRect();
    const below = box.bottom + list.offsetHeight + 8 <= window.innerHeight;
    list.classList.toggle("menu--up", !below);
    list.style.top = (below ? box.bottom + 4 : Math.max(8, box.top - list.offsetHeight - 4)) + "px";
    list.style.left = Math.max(8, box.right - list.offsetWidth) + "px";
    trigger.setAttribute("aria-expanded", "true");
    held.count++;
    document.addEventListener("pointerdown", outside, true);
    window.addEventListener("resize", close);
    window.addEventListener("blur", close);
    // scroll events do not bubble
    document.addEventListener("scroll", scrolled, true);
    entries.find((b) => !b.disabled)?.focus();
  }

  trigger.setAttribute("aria-haspopup", "menu");
  trigger.setAttribute("aria-expanded", "false");
  trigger.addEventListener("click", () => {
    if (list) {
      close();
    } else if (trigger.getAttribute("aria-disabled") !== "true") {
      open();
    }
  });
}

// menuButton is a button with a caret that opens a menu.
export function menuButton(label: string, items: MenuItem[], opts: ButtonOptions = {}): HTMLButtonElement {
  const trigger = button(label, () => {}, opts);
  const caret = icon("chevronDown");
  caret.classList.add("ico--caret");
  trigger.append(caret);
  attachMenu(trigger, label, items, opts.key);
  return trigger;
}

// moreButton is the three dots at the end of a row; label names the row.
export function moreButton(label: string, items: MenuItem[], opts: { disabled?: boolean; key?: string } = {}): HTMLButtonElement {
  const trigger = iconButton("more", label, () => {}, { tip: t.common.more, disabled: opts.disabled, key: opts.key });
  attachMenu(trigger, label, items, opts.key);
  return trigger;
}

export interface DialogOptions {
  title: Child[];
  // text is a line under the title.
  text?: Child[];
  content: Child[];
  submit: string;
  submitIcon?: IconName;
  // danger makes the dialog a confirmation: red, and starting on Cancel.
  danger?: boolean;
  narrow?: boolean;
  // submitState is asked when the dialog opens and at every change of
  // the form: whether there is something to submit, and under what name.
  submitState?: () => { disabled: boolean; label?: string };
  // validate marks what is wrong, with setFieldError or fail, and returns
  // whether to go on.
  validate?: () => boolean;
  onSubmit: () => void;
}

// dialog shows a modal form. The native dialog element keeps the focus
// inside, closes on Escape and returns the focus to where it was; when
// that control was redrawn meanwhile, the focus goes to its successor.
// fail shows a problem in the dialog: a toast would sit under it.
export function dialog(opts: DialogOptions): { close: () => void; fail: (text: string) => void } {
  const opener = (document.activeElement as HTMLElement | null)?.dataset.key;
  const problem = h("span", { lang: "en" });
  const error = h("div", { class: "callout callout--danger", role: "alert", hidden: true }, icon("alert"), problem);
  const fail = (text: string) => {
    problem.textContent = text;
    error.hidden = false;
    error.scrollIntoView({ block: "nearest" });
  };
  const cancel = button(t.common.cancel, () => el.close());
  const submitLabel = h("span", null, opts.submit);
  const submit = h(
    "button",
    { type: "submit", class: classes("btn", opts.danger ? "btn--danger" : "btn--primary") },
    opts.submitIcon && icon(opts.submitIcon),
    submitLabel,
  );
  const settle = () => {
    const state = opts.submitState?.();
    if (state) {
      submit.disabled = state.disabled;
      submitLabel.textContent = state.label ?? opts.submit;
    }
  };
  // a destructive dialog starts on Cancel
  (opts.danger ? cancel : submit).toggleAttribute("autofocus", true);
  const form = h(
    "form",
    {
      method: "dialog",
      novalidate: true,
      class: "dialog-form",
      // a complaint goes away once the form is touched again
      oninput: (e) => {
        error.hidden = true;
        if (e.target instanceof HTMLElement && e.target.hasAttribute("aria-invalid")) {
          setFieldError(e.target, "");
        }
        settle();
      },
      onsubmit: (e) => {
        e.preventDefault();
        if (opts.validate && !opts.validate()) {
          form.querySelector<HTMLElement>("[aria-invalid]")?.focus();
          return;
        }
        el.close();
        opts.onSubmit();
      },
    },
    h(
      "div",
      { class: "dialog-head" },
      h("h2", { class: "dialog-title", id: "dialog-title" }, ...opts.title),
      opts.text && h("p", { class: "dialog-desc" }, ...opts.text),
    ),
    h("div", { class: "dialog-body" }, ...opts.content, error),
    h("div", { class: "dialog-foot" }, cancel, submit),
  );
  const el = h(
    "dialog",
    {
      class: classes("dialog", opts.narrow && "dialog--narrow"),
      role: opts.danger ? "alertdialog" : undefined,
      "aria-labelledby": "dialog-title",
      onclose: () => {
        el.remove();
        if (document.activeElement === document.body) {
          focusKey(opener);
        }
      },
    },
    form,
  );
  document.body.append(el);
  settle();
  el.showModal();
  // a field comes before the buttons for typing dialogs
  const first = form.querySelector<HTMLElement>(".dialog-body input:not([type=checkbox]):not([type=radio]), .dialog-body select");
  if (first && !opts.danger) {
    first.focus();
  }
  return { close: () => el.close(), fail };
}

let fields = 0;

// field is a labelled control with room for an error and a hint. control
// may be a row that holds the input next to a button.
export function field(label: string, control: HTMLElement, ...hint: Child[]): HTMLElement {
  const target = control.matches("input, select") ? control : control.querySelector<HTMLElement>("input, select");
  if (target && target.id === "") {
    target.id = "field-" + ++fields;
  }
  return h(
    "div",
    { class: "field" },
    h("label", { class: "label", for: target?.id }, label),
    control,
    h("p", { class: "field-error", role: "alert" }),
    hint.length > 0 && h("p", { class: "hint" }, ...hint),
  );
}

// Where a control's complaint goes when it is not the line of its own field.
const errorSlots = new WeakMap<HTMLElement, Element>();

// setFieldError shows what is wrong with a control under it; "" takes the
// complaint away. slot is another place for it, remembered for the
// control: a line that controls in one row share.
export function setFieldError(control: HTMLElement, text: string, slot?: Element): void {
  if (slot) {
    errorSlots.set(control, slot);
  }
  const place = errorSlots.get(control) ?? control.closest(".field")?.querySelector(".field-error");
  place?.replaceChildren(...(text === "" ? [] : [icon("alert", "sm"), h("span", null, text)]));
  if (text === "") {
    control.removeAttribute("aria-invalid");
  } else {
    control.setAttribute("aria-invalid", "true");
  }
}

export function input(props: Record<string, Value> = {}): HTMLInputElement {
  return h("input", { type: "text", class: "input", spellcheck: "false", autocomplete: "off", ...props });
}

export function select(options: { value: string; label: string }[], value: string, props: Record<string, Value> = {}): HTMLSelectElement {
  const el = h("select", { class: "select", ...props }, ...options.map((o) => h("option", { value: o.value }, o.label)));
  el.value = value;
  return el;
}

export function checkbox(
  label: Child[],
  checked: boolean,
  props: Record<string, Value> = {},
): { box: HTMLInputElement; el: HTMLLabelElement } {
  const box = h("input", { type: "checkbox", ...props });
  box.checked = checked;
  return { box, el: h("label", { class: "check" }, box, h("span", null, ...label)) };
}

// command is a command line with a button that copies it.
export function command(line: string): HTMLElement {
  const said = h("span", { class: "cmd-copied", role: "status", hidden: true });
  let timer = 0;
  const say = (ok: boolean) => {
    said.textContent = ok ? t.common.copied : t.common.copyFailed;
    said.toggleAttribute("data-failed", !ok);
    said.hidden = false;
    copy.replaceChildren(icon(ok ? "check" : "copy", "sm"));
    window.clearTimeout(timer);
    timer = window.setTimeout(() => {
      said.hidden = true;
      copy.replaceChildren(icon("copy", "sm"));
    }, 1600);
  };
  const copy = iconButton("copy", t.common.copyCommand(line), () => void copyText(line).then(say, () => say(false)), {
    tip: t.common.copy,
    small: true,
  });
  return h(
    "div",
    { class: "cmd" },
    h("code", { class: "cmd-text", lang: "en" }, h("span", { class: "cmd-prompt", "aria-hidden": "true" }, "> "), line),
    said,
    copy,
  );
}
