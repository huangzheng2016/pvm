// A small DOM builder. Text from VMs and the API only ever becomes text
// nodes and attribute values; nothing here parses HTML.

import { copyText } from "./api";

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

// button is a push button. key names it across re-renders, so the focus
// can return to it.
export function button(
  label: string,
  onclick: () => void,
  opts: { kind?: "primary" | "danger" | "plain"; disabled?: boolean; title?: string; key?: string } = {},
): HTMLButtonElement {
  return h(
    "button",
    {
      type: "button",
      class: "btn" + (opts.kind ? " " + opts.kind : ""),
      disabled: opts.disabled ?? false,
      title: opts.title,
      "data-key": opts.key,
      onclick,
    },
    label,
  );
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

export function badge(text: string, kind = ""): HTMLElement {
  return h("span", { class: "badge " + kind }, text);
}

// held counts open menus: while one is open the list under it is not
// re-rendered. onRelease runs when the last one closes.
export const held = { count: 0, onRelease: () => {} };

export interface MenuItem {
  label: string;
  action: () => void;
  disabled?: boolean;
  danger?: boolean;
}

export function menuButton(label: string, items: MenuItem[], opts: { disabled?: boolean; key?: string } = {}): HTMLElement {
  const trigger = h(
    "button",
    {
      type: "button",
      class: "btn menu-trigger",
      "aria-haspopup": "menu",
      "aria-expanded": "false",
      disabled: opts.disabled ?? false,
      "data-key": opts.key,
      onclick: () => (list ? close() : open()),
    },
    label,
  );
  let list: HTMLElement | null = null;

  const outside = (e: Event) => {
    if (list && !list.contains(e.target as Node) && e.target !== trigger) {
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
  // Closing lets a held redraw run, which replaces the trigger: the
  // focus goes to the control that has its key now.
  const backToTrigger = () => (opts.key === undefined ? trigger.focus() : focusKey(opts.key));
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
          class: item.danger ? "danger" : "",
          disabled: item.disabled ?? false,
          onclick: () => {
            close();
            // a dialog the action opens returns the focus here
            backToTrigger();
            item.action();
          },
        },
        item.label,
      ),
    );
    list = h(
      "div",
      {
        class: "menu",
        role: "menu",
        onkeydown: (e) => {
          const key = (e as KeyboardEvent).key;
          const enabled = entries.filter((b) => !b.disabled);
          const at = enabled.indexOf(document.activeElement as HTMLButtonElement);
          if (key === "Escape") {
            close();
            backToTrigger();
          } else if (key === "ArrowDown" || key === "ArrowUp") {
            e.preventDefault();
            const step = key === "ArrowDown" ? 1 : -1;
            enabled[(at + step + enabled.length) % enabled.length]?.focus();
          } else if (key === "Tab") {
            close();
          }
        },
      },
      ...entries,
    );
    document.body.append(list);
    const box = trigger.getBoundingClientRect();
    const below = box.bottom + list.offsetHeight + 8 <= window.innerHeight;
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
  return trigger;
}

export interface DialogOptions {
  title: string;
  content: Child[];
  submit: string;
  danger?: boolean;
  // validate returns what is wrong, or "" to go on.
  validate?: () => string;
  onSubmit: () => void;
}

// dialog shows a modal form. The native dialog element keeps the focus
// inside, closes on Escape and returns the focus to where it was; when
// that control was redrawn meanwhile, the focus goes to its successor.
// fail shows a problem in the dialog: a toast would sit under it.
export function dialog(opts: DialogOptions): { close: () => void; fail: (text: string) => void } {
  const opener = (document.activeElement as HTMLElement | null)?.dataset.key;
  const fail = (text: string) => {
    error.textContent = text;
    error.hidden = false;
  };
  const error = h("p", { class: "error", role: "alert", hidden: true });
  const cancel = button("Cancel", () => el.close(), { kind: "plain" });
  const submit = h("button", { type: "submit", class: "btn " + (opts.danger ? "danger" : "primary") }, opts.submit);
  // a destructive dialog starts on Cancel
  (opts.danger ? cancel : submit).toggleAttribute("autofocus", true);
  const heading = h("h2", { id: "dialog-title" }, opts.title);
  const form = h(
    "form",
    {
      method: "dialog",
      novalidate: true,
      // a complaint goes away once the form is touched again
      oninput: () => (error.hidden = true),
      onsubmit: (e) => {
        e.preventDefault();
        const problem = opts.validate?.() ?? "";
        if (problem !== "") {
          fail(problem);
          return;
        }
        el.close();
        opts.onSubmit();
      },
    },
    heading,
    h("div", { class: "dialog-body" }, ...opts.content),
    error,
    h("div", { class: "dialog-actions" }, cancel, submit),
  );
  const el = h(
    "dialog",
    {
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
  el.showModal();
  // a field comes before the buttons for typing dialogs
  const first = form.querySelector<HTMLElement>(".dialog-body input:not([type=checkbox]), .dialog-body select");
  if (first && !opts.danger) {
    first.focus();
  }
  return { close: () => el.close(), fail };
}

// field is a labelled control with room for a hint and an error.
export function field(label: string, control: HTMLElement, hint = ""): HTMLLabelElement {
  return h(
    "label",
    { class: "field" },
    h("span", { class: "field-label" }, label),
    control,
    hint !== "" && h("span", { class: "hint" }, hint),
    h("span", { class: "field-error", role: "alert" }),
  );
}

export function setFieldError(control: HTMLElement, text: string): void {
  const slot = control.closest(".field")?.querySelector(".field-error");
  if (slot) {
    slot.textContent = text;
  }
  control.toggleAttribute("aria-invalid", text !== "");
}

export function select(options: { value: string; label: string }[], value: string, props: Record<string, Value> = {}): HTMLSelectElement {
  const el = h("select", props, ...options.map((o) => h("option", { value: o.value }, o.label)));
  el.value = value;
  return el;
}

export function checkbox(
  label: string,
  checked: boolean,
  props: Record<string, Value> = {},
): { box: HTMLInputElement; text: HTMLElement; el: HTMLLabelElement } {
  const box = h("input", { type: "checkbox", ...props });
  box.checked = checked;
  const text = h("span", null, label);
  return { box, text, el: h("label", { class: "check" }, box, text) };
}

// command is a command line with a button that copies it.
export function command(line: string): HTMLElement {
  const copy = button("Copy", () => {
    copyText(line).then(
      (ok) => (copy.textContent = ok ? "Copied" : "Copy failed"),
      () => (copy.textContent = "Copy failed"),
    );
  });
  copy.classList.add("small");
  return h("div", { class: "command" }, h("code", null, line), copy);
}

export function table(headers: string[], rows: HTMLTableRowElement[]): HTMLElement {
  return h(
    "div",
    { class: "table-wrap" },
    h("table", null, h("thead", null, h("tr", null, ...headers.map((t) => h("th", { scope: "col" }, t)))), h("tbody", null, ...rows)),
  );
}

export function empty(title: string, text: string, ...actions: Child[]): HTMLElement {
  return h("div", { class: "empty" }, h("h2", null, title), h("p", null, text), ...actions);
}

export function pageHeader(title: string, ...actions: Child[]): HTMLElement {
  return h("header", { class: "page-header" }, h("h1", null, title), h("div", { class: "page-actions" }, ...actions));
}
