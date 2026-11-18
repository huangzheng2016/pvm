import { backend, errorText, type CatalogApp, type CreateRequest, type Image } from "./api";
import { badge, button, checkbox, field, h, pageHeader, select, setFieldError } from "./dom";
import { actStart, data, nav, refresh, toast } from "./store";
import * as tasks from "./tasks";
import { exitChoices } from "./vms";

// The CLI's rules: vm.ValidateName and the user rule of vm.ValidateSpec.
const nameRule = /^[a-zA-Z][a-zA-Z0-9_-]{0,31}$/;
const userRule = /^[a-z_][a-z0-9_-]{0,31}$/;

const text = (props: Record<string, string> = {}) => h("input", { type: "text", spellcheck: "false", autocomplete: "off", ...props });
const number = () => h("input", { type: "number", step: "1", inputmode: "numeric" });

const name = text({ placeholder: "dev1" });
const image = select([], "");
const user = text();
const cpus = number();
const mem = number();
const disk = number();
const exit = select([], "");
const keep = { value: "", label: "Default" };
const clipboard = select(
  [keep, { value: "off", label: "Off" }, { value: "to-guest", label: "Host to VM" }, { value: "to-host", label: "VM to host" }, { value: "both", label: "Both ways" }],
  "",
);
const drop = select([keep, { value: "on", label: "On" }, { value: "off", label: "Off" }], "");
const audio = select(
  [keep, { value: "off", label: "Off" }, { value: "out", label: "VM sound on the host speakers" }, { value: "both", label: "Speakers and host microphone" }],
  "",
);
const imageHint = h("span", { class: "hint" });
const exitWarning = h("p", { class: "warning", hidden: true });
const programs = h("div", { class: "checks" });
const problem = h("p", { class: "error banner", role: "alert", hidden: true });
const run = h("div", { class: "create-run", hidden: true });

let images: Image[] = [];
let programBoxes: HTMLInputElement[] = [];
// a value the user typed is not replaced by a default read later
const typed = new Set<HTMLElement>();
for (const el of [user, cpus, mem, disk, exit, image]) {
  el.addEventListener("input", () => typed.add(el));
}

function limit(el: HTMLInputElement, label: string, least: number, most: number): string {
  const n = Number(el.value);
  return el.value.trim() === "" || !Number.isInteger(n) || n < least || n > most ? label + " must be a whole number from " + least + " to " + most + "." : "";
}

// The number limits are the daemon's own.
const checks: [HTMLElement, () => string][] = [
  [
    name,
    () => {
      if (!nameRule.test(name.value)) {
        return "Use letters, digits, - and _, at most 32, starting with a letter.";
      }
      return data.vms?.some((vm) => vm.name === name.value) ? "A VM named " + name.value + " already exists." : "";
    },
  ],
  [image, () => (image.value === "" ? "Choose an image." : "")],
  [user, () => (userRule.test(user.value) ? "" : "Use lowercase letters, digits, - and _, at most 32, starting with a letter or _.")],
  [cpus, () => limit(cpus, "CPUs", 1, 64)],
  [mem, () => limit(mem, "Memory", 128, 1048576)],
  [disk, () => limit(disk, "Disk", 1, 4096)],
  [exit, () => (exit.value === "" ? "Choose an exit." : "")],
];

function validate(only?: HTMLElement): boolean {
  let ok = true;
  for (const [el, check] of checks) {
    if (only && el !== only) {
      continue;
    }
    const message = check();
    setFieldError(el, message);
    ok = ok && message === "";
  }
  return ok;
}

for (const [el] of checks) {
  el.addEventListener("change", () => validate(el));
  // an error goes away as it is fixed, not at the click that leaves the
  // field: the form would move under that click
  el.addEventListener("input", () => {
    if (el.hasAttribute("aria-invalid")) {
      validate(el);
    }
  });
}

function showImageHint(): void {
  const chosen = images.find((i) => i.spec === image.value);
  imageHint.textContent =
    chosen && !chosen.golden ? "No prepared image for " + chosen.spec + " yet: the first create downloads and bakes one, which takes minutes." : "";
}
image.addEventListener("change", showImageHint);

function setOptions(el: HTMLSelectElement, options: { value: string; label: string }[], fallback: string): void {
  const before = el.value;
  el.replaceChildren(...options.map((o) => h("option", { value: o.value }, o.label)));
  const wanted = typed.has(el) && options.some((o) => o.value === before) ? before : fallback;
  el.value = options.some((o) => o.value === wanted) ? wanted : (options[0]?.value ?? "");
}

function programRow(app: CatalogApp, checked: boolean): HTMLElement {
  const { box, text, el } = checkbox(app.name, checked, { value: app.id });
  if (app.source !== "official") {
    text.append(" ", badge(app.source, "warn"));
  }
  text.append(" ", badge(app.window ? "window" : "terminal"), " ", h("span", { class: "sub" }, app.description));
  programBoxes.push(box);
  return el;
}

function fail(what: string): (e: unknown) => void {
  return (e) => {
    problem.textContent = what + ": " + errorText(e);
    problem.hidden = false;
  };
}

// enter fills the choices each time the screen opens: exits and
// prepared images change while the app runs.
export function enter(): void {
  if (!run.hidden) {
    return;
  }
  problem.hidden = true;
  const b = backend();
  b.Images().then((list) => {
    images = list;
    setOptions(image, list.map((i) => ({ value: i.spec, label: i.golden ? i.spec : i.spec + " (not prepared yet)" })), list.find((i) => i.golden)?.spec ?? "");
    showImageHint();
  }, fail("Cannot read the images"));
  Promise.all([b.CreateDefaults(), refresh()]).then(([d]) => {
    for (const [el, value] of [[user, d.user], [cpus, d.cpus], [mem, d.mem_mb], [disk, d.disk_gb]] as const) {
      if (!typed.has(el)) {
        el.value = String(value);
      }
    }
    const choices = exitChoices();
    setOptions(exit, choices, d.exit);
    exitWarning.hidden = choices.some((c) => c.value === d.exit);
    exitWarning.textContent = "The default exit " + d.exit + " no longer exists. Choose the exit of this VM here, and set another default in Exits.";
  }, fail("Cannot read the defaults"));
  b.Catalog().then((catalog) => {
    const checked = programBoxes.filter((b) => b.checked).map((b) => b.value);
    programBoxes = [];
    programs.replaceChildren(...catalog.map((app) => programRow(app, checked.includes(app.id))));
    if (catalog.length === 0) {
      programs.textContent = "The catalog is empty.";
    }
  }, fail("Cannot read the app catalog"));
}

function request(): CreateRequest {
  return {
    name: name.value,
    image: image.value,
    user: user.value,
    cpus: Number(cpus.value),
    mem_mb: Number(mem.value),
    disk_gb: Number(disk.value),
    exit: exit.value,
    app_config: { clipboard: clipboard.value, drop: drop.value, audio: audio.value },
    apps: programBoxes.filter((b) => b.checked).map((b) => b.value),
  };
}

const form = h(
  "form",
  {
    class: "create-form",
    novalidate: true,
    onsubmit: (e) => {
      e.preventDefault();
      problem.hidden = true;
      if (!validate()) {
        form.querySelector<HTMLElement>("[aria-invalid]")?.focus();
        return;
      }
      void create(request());
    },
  },
  problem,
  h(
    "fieldset",
    null,
    h("legend", null, "Machine"),
    h("div", { class: "grid" }, field("Name", name), field("Image", image), field("User", user, "The account pvm creates in the VM.")),
    imageHint,
    h("div", { class: "grid" }, field("CPUs", cpus), field("Memory (MB)", mem), field("Disk (GB)", disk)),
  ),
  h(
    "fieldset",
    null,
    h("legend", null, "Network"),
    exitWarning,
    h("div", { class: "grid" }, field("Exit", exit, "Where the VM's traffic leaves, from its first boot on. direct uses the host's network.")),
  ),
  h(
    "fieldset",
    null,
    h("legend", null, "App windows"),
    h("div", { class: "grid" }, field("Clipboard", clipboard), field("Drop files", drop), field("Audio", audio)),
  ),
  h("fieldset", null, h("legend", null, "Programs to install"), programs),
  h("div", { class: "form-actions" }, h("button", { type: "submit", class: "btn primary" }, "Create VM")),
);

async function create(r: CreateRequest): Promise<void> {
  const status = h("p", { class: "muted" }, "Starting...");
  const steps = h("div", null, status);
  const after = h("div", { class: "form-actions" });
  run.replaceChildren(steps, after);
  run.hidden = false;
  form.hidden = true;
  const cancel = tasks.whenStarted("Create " + r.name, (id) => steps.replaceChildren(tasks.view(id)!));

  const { task, error } = await actStart(r.name, () => backend().CreateVM(r), true);
  cancel();
  const back = (label: string, reset: boolean) =>
    button(label, () => {
      if (reset) {
        name.value = "";
        programBoxes.forEach((b) => (b.checked = false));
      }
      run.hidden = true;
      form.hidden = false;
      enter();
      name.focus();
    });
  if (error !== undefined) {
    // refused before anything ran: back to the form with the reason
    run.hidden = true;
    form.hidden = false;
    problem.textContent = error;
    problem.hidden = false;
    return;
  }
  if (task?.ok) {
    after.append(h("p", { class: "ok" }, r.name + " is ready."), button("Go to the VMs", () => nav.go("vms"), { kind: "primary" }), back("Create another", true));
    return;
  }
  if (nav.current !== "create") {
    toast("Create " + r.name + " failed: " + (task?.error ?? ""));
  }
  after.append(
    h("p", { class: "muted" }, "Nothing was undone: if " + r.name + " was created, it is in the VM list."),
    button("Go to the VMs", () => nav.go("vms"), { kind: "primary" }),
    back("Back to the form", false),
  );
}

const page = h("div", { class: "page" }, pageHeader("Create a VM"), form, run);

export function render(): HTMLElement {
  return page;
}
