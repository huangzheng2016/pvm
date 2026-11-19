import { backend, errorText, type CatalogApp, type CreateRequest, type Image, type Task } from "./api";
import { banner, button, callout, command, field, focusKey, h, input, mono, redraw, select, type Child } from "./dom";
import { onLanguage, t } from "./i18n";
import { icon } from "./icons";
import { actStart, data, nav, refresh, toast } from "./store";
import * as tasks from "./tasks";
import { appConfigChoices, exitChoices, programCard, stageBars } from "./vms";

// The CLI's rules: vm.ValidateName and the user rule of vm.ValidateSpec.
const nameRule = /^[a-zA-Z][a-zA-Z0-9_-]{0,31}$/;
const userRule = /^[a-z_][a-z0-9_-]{0,31}$/;

const number = () => input({ type: "number", step: "1", inputmode: "numeric" });

// The controls live as long as the page: the words around them are laid
// out again when the language changes, and what was typed stays.
const name = input({ class: "input input--mono", placeholder: "dev1" });
const image = select([], "");
const user = input({ class: "input input--mono" });
const cpus = number();
const mem = number();
const disk = number();
const exit = select([], "");
const clipboard = select([], "");
const drop = select([], "");
const audio = select([], "");
const imageNote = h("div", { class: "slot" });
const exitWarning = h("div", { class: "slot" });
// cpus, memory and disk sit in one row and share the line under it
const sizeError = h("p", { class: "field-error", role: "alert" });
const programs = h("div", { class: "pick-grid pick-grid--stack", role: "group" });
const problem = h("div", { class: "slot" });

let images: Image[] = [];
let catalog: CatalogApp[] | null = null;
let programBoxes: HTMLInputElement[] = [];
// the default exit when it names an exit that was removed
let goneExit = "";
// failure is what could not be read, or why the backend refused
let failure: { what: "imagesError" | "defaultsError" | "catalogError" | ""; detail: string } | null = null;
// a value the user typed is not replaced by a default read later
const typed = new Set<HTMLElement>();
for (const el of [user, cpus, mem, disk, exit, image]) {
  el.addEventListener("input", () => typed.add(el));
}

function outside(el: HTMLInputElement, least: number, most: number): boolean {
  const n = Number(el.value);
  return el.value.trim() === "" || !Number.isInteger(n) || n < least || n > most;
}

const checks: [HTMLElement, () => string][] = [
  [
    name,
    () => {
      if (!nameRule.test(name.value)) {
        return t.create.nameBad;
      }
      return data.vms?.some((vm) => vm.name === name.value) ? t.create.nameTaken(name.value) : "";
    },
  ],
  [image, () => (image.value === "" ? t.create.imageMissing : "")],
  [user, () => (userRule.test(user.value) ? "" : t.create.userBad)],
  [exit, () => (exit.value === "" ? t.create.exitMissing : "")],
];

// The number limits are the daemon's own.
const sizes: [HTMLInputElement, () => string][] = [
  [cpus, () => (outside(cpus, 1, 64) ? t.create.cpusRange(1, 64) : "")],
  [mem, () => (outside(mem, 128, 1048576) ? t.create.memoryRange(128, 1048576) : "")],
  [disk, () => (outside(disk, 1, 4096) ? t.create.diskRange(1, 4096) : "")],
];

function complain(el: HTMLElement, message: string, slot: Element | null | undefined): void {
  slot?.replaceChildren(...(message === "" ? [] : [icon("alert", "sm"), h("span", null, message)]));
  if (message === "") {
    el.removeAttribute("aria-invalid");
  } else {
    el.setAttribute("aria-invalid", "true");
  }
}

function validateSizes(): boolean {
  let first = "";
  for (const [el, check] of sizes) {
    const message = check();
    complain(el, message, null);
    first ||= message;
  }
  sizeError.replaceChildren(...(first === "" ? [] : [icon("alert", "sm"), h("span", null, first)]));
  return first === "";
}

function validate(only?: HTMLElement): boolean {
  let ok = true;
  for (const [el, check] of checks) {
    if (only && el !== only) {
      continue;
    }
    const message = check();
    complain(el, message, el.closest(".field")?.querySelector(".field-error"));
    ok = ok && message === "";
  }
  if (!only || sizes.some(([el]) => el === only)) {
    ok = validateSizes() && ok;
  }
  return ok;
}

for (const el of [...checks, ...sizes].map(([control]) => control)) {
  el.addEventListener("change", () => validate(el));
  // an error goes away as it is fixed, not at the click that leaves the
  // field: the form would move under that click
  el.addEventListener("input", () => {
    if (el.hasAttribute("aria-invalid")) {
      validate(el);
    }
  });
}

function showImageNote(): void {
  const chosen = images.find((i) => i.spec === image.value);
  redraw(imageNote, chosen && !chosen.golden ? chosen.spec : "", () => [
    chosen && !chosen.golden && callout("info", "note", ...t.create.imageRaw(h("strong", { lang: "en" }, chosen.spec))),
  ]);
}
image.addEventListener("change", showImageNote);

function setOptions(el: HTMLSelectElement, options: { value: string; label: string }[], fallback: string): void {
  const before = el.value;
  el.replaceChildren(...options.map((o) => h("option", { value: o.value }, o.label)));
  const wanted = typed.has(el) && options.some((o) => o.value === before) ? before : fallback;
  el.value = options.some((o) => o.value === wanted) ? wanted : (options[0]?.value ?? "");
}

function fillImages(): void {
  setOptions(
    image,
    images.map((i) => ({ value: i.spec, label: i.golden ? i.spec : t.create.notPrepared(i.spec) })),
    image.value !== "" ? image.value : (images.find((i) => i.golden)?.spec ?? ""),
  );
  showImageNote();
}

function fillPrograms(): void {
  if (!catalog) {
    return;
  }
  const checked = programBoxes.filter((b) => b.checked).map((b) => b.value);
  const cards = catalog.map((app) => programCard(app, checked.includes(app.id)));
  programBoxes = cards.map((c) => c.box);
  programs.replaceChildren(...cards.map((c) => c.el));
  if (catalog.length === 0) {
    programs.replaceChildren(h("p", { class: "hint" }, t.create.catalogEmpty));
  }
}

function showProblem(): void {
  redraw(problem, failure, () => [failure && banner(failure.what === "" ? "" : t.create[failure.what], failure.detail)]);
}

function showExitWarning(): void {
  redraw(exitWarning, goneExit, () => [goneExit !== "" && callout("warn", "alert", t.create.exitGone(goneExit))]);
}

function failed(what: "imagesError" | "defaultsError" | "catalogError"): (e: unknown) => void {
  return (e) => {
    failure = { what, detail: errorText(e) };
    showProblem();
  };
}

// enter fills the choices each time the screen opens: exits and
// prepared images change while the app runs.
export function enter(): void {
  if (current) {
    current.toEnd();
    return;
  }
  failure = null;
  showProblem();
  const b = backend();
  b.Images().then((list) => {
    images = list;
    fillImages();
  }, failed("imagesError"));
  Promise.all([b.CreateDefaults(), refresh()]).then(([d]) => {
    for (const [el, value] of [[user, d.user], [cpus, d.cpus], [mem, d.mem_mb], [disk, d.disk_gb]] as const) {
      if (!typed.has(el)) {
        el.value = String(value);
      }
    }
    const choices = exitChoices();
    setOptions(exit, choices, d.exit);
    goneExit = choices.some((c) => c.value === d.exit) ? "" : d.exit;
    showExitWarning();
  }, failed("defaultsError"));
  b.Catalog().then((list) => {
    catalog = list;
    fillPrograms();
  }, failed("catalogError"));
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

const head = h("header", { class: "page-head" });
const form = h("form", {
  class: "form form--split",
  id: "create-form",
  novalidate: true,
  onsubmit: (e) => {
    e.preventDefault();
    failure = null;
    showProblem();
    if (!validate()) {
      form.querySelector<HTMLElement>("[aria-invalid]")?.focus();
      return;
    }
    void create(request());
  },
});
const formBody = h("div", { class: "page-body" }, problem, form);
const runBody = h("div", { class: "page-body page-body--run", hidden: true });
const page = h("div", { class: "page" }, head, formBody, runBody);

const group = (title: string, ...content: Child[]): HTMLElement =>
  h("section", { class: "fieldset" }, h("h2", { class: "fieldset-title" }, title), ...content);

// label lays the form out in the current language around the controls.
function label(): void {
  const c = t.create;
  head.replaceChildren(
    h("h1", { class: "page-title" }, c.title),
    // outside the form, so it names the form it submits
    h("div", { class: "page-actions" }, h("button", { type: "submit", form: "create-form", class: "btn btn--primary" }, icon("plus"), c.submit)),
  );
  form.replaceChildren(
    h(
      "div",
      { class: "form-col" },
      group(
        c.machine,
        field(c.name, name, c.nameHint),
        field(c.image, image),
        imageNote,
        field(c.user, user, c.userHint),
        h("div", { class: "form-grid form-grid--3" }, field(c.cpus, cpus), field(c.memory, mem), field(c.disk, disk)),
        sizeError,
      ),
      group(c.network, exitWarning, field(c.exit, exit, c.exitHint)),
    ),
    h(
      "div",
      { class: "form-col" },
      group(
        c.windows,
        h(
          "div",
          { class: "form-grid form-grid--3" },
          field(t.appConfig.clipboard, clipboard),
          field(t.appConfig.drop, drop),
          field(t.appConfig.audio, audio),
        ),
      ),
      group(c.programs, programs),
    ),
  );
  const config = appConfigChoices(c.keep);
  for (const [el, options] of [[clipboard, config.clipboard], [drop, config.drop], [audio, config.audio]] as const) {
    const value = el.value;
    el.replaceChildren(...options.map((o) => h("option", { value: o.value }, o.label)));
    el.value = value;
  }
  fillImages();
  const chosen = exit.value;
  exit.replaceChildren(...exitChoices().map((o) => h("option", { value: o.value }, o.label)));
  exit.value = chosen;
  fillPrograms();
  showProblem();
  showExitWarning();
  // the complaints were written in the other language
  for (const el of [...checks, ...sizes].map(([control]) => control)) {
    if (el.hasAttribute("aria-invalid")) {
      validate(el);
    }
  }
}

const two = (n: number): string => String(n).padStart(2, "0");

interface Run {
  observer: tasks.Observer;
  // unwatch ends the watch of the task, once there is one.
  unwatch: () => void;
  // finish takes the task as the action returned it.
  finish(task: Task | undefined): void;
  // draw brings every part up to date; sync only what follows the VM list.
  draw(): void;
  sync(): void;
  toEnd(): void;
  stop(): void;
}

// newRun is the view of one create: the steps, the output of the one in
// view, and how it ended. It follows the task through its observer.
function newRun(r: CreateRequest, firstUse: boolean): Run {
  const started = Date.now();
  let ended = 0;
  let task: Task | null = null;
  // the step whose output shows; null follows the one at work
  let selected: number | null = null;
  const outputs: string[][] = [];
  let lines: HTMLElement | null = null;
  let scroller: HTMLElement | null = null;
  let placeholder: HTMLElement | null = null;
  let shownStep = -1;

  const clock = h("span");
  const headSlot = h("div", { class: "slot" });
  const bar = h("div", { class: "run-bar", role: "progressbar", "aria-valuemin": 0 });
  const card = h("section", { class: "run-card" });
  const termHead = h("div", { class: "term-head" });
  const stageSlot = h("div", { class: "slot" });
  const termBody = h("div", { class: "slot" });
  const term = h("section", { class: "term", lang: "en" }, termHead, stageSlot, termBody);
  const footSlot = h("div", { class: "slot" });
  runBody.replaceChildren(h("div", { class: "run" }, headSlot, bar, h("div", { class: "run-grid" }, card, term), footSlot));

  const seconds = () => Math.floor(((ended || Date.now()) - started) / 1000);
  const tick = () => (clock.textContent = two(Math.floor(seconds() / 60)) + ":" + two(seconds() % 60));
  const timer = window.setInterval(tick, 1000);
  tick();

  const steps = () => task?.steps ?? [];
  // the step at work, else the one that failed, else the last
  function current(): number {
    const all = steps();
    const running = all.findIndex((s) => s.state === "running");
    const failed = all.findIndex((s) => s.state === "failed");
    return running >= 0 ? running : failed >= 0 ? failed : all.length - 1;
  }
  const viewed = () => Math.min(selected ?? current(), steps().length - 1);
  const phase = () => (ended === 0 ? (task ? "running" : "starting") : task?.ok ? "success" : "failed");

  const toVms = (opts: { kind?: "primary"; size?: "sm" } = {}) => button(t.create.toVms, () => nav.go("vms"), opts);

  function drawHead(): void {
    const c = t.create;
    const all = steps();
    redraw(headSlot, [phase(), current(), all.length, task?.error], () => {
      if (phase() === "success") {
        const via = r.exit === "direct" ? c.viaHost : c.viaExit(r.exit);
        return [
          h(
            "div",
            { class: "result", role: "status" },
            h("span", { class: "seal", "aria-hidden": "true" }, icon("check")),
            h(
              "div",
              { class: "result-body" },
              h("h1", { class: "run-title" }, ...c.ready(mono(r.name))),
              h("p", { class: "muted" }, c.took(Math.floor(seconds() / 60), seconds() % 60, via)),
              h("div", { class: "result-actions" }, toVms({ kind: "primary" }), button(c.another, () => back(true))),
            ),
          ),
        ];
      }
      if (phase() === "failed") {
        return [
          h(
            "div",
            { class: "result", role: "alert" },
            h("span", { class: "seal seal--danger", "aria-hidden": "true" }, icon("cross")),
            h(
              "div",
              { class: "result-body" },
              h("h1", { class: "run-title" }, ...c.failed(mono(r.name))),
              callout("danger", "note", h("span", { class: "mono", lang: "en" }, task?.error ?? "")),
              h("div", { class: "result-actions" }, toVms({ kind: "primary" }), button(c.back, () => back(false))),
            ),
          ),
        ];
      }
      return [
        h(
          "div",
          { class: "run-head" },
          h(
            "div",
            { class: "grow" },
            h("h1", { class: "run-title" }, ...c.creating(mono(r.name))),
            h("p", { class: "run-sub" }, all.length === 0 ? c.starting : c.step(current() + 1, all.length) + (firstUse ? c.firstUse(r.image) : "")),
          ),
          h("span", { class: "run-clock", role: "timer" }, icon("clock", "sm"), h("span", { class: "sr-only" }, c.elapsed), clock),
        ),
      ];
    });
  }

  function drawSteps(): void {
    const c = t.create;
    const all = steps();
    const done = all.filter((s) => s.state === "done").length;
    bar.setAttribute("aria-label", c.progress(r.name, done, all.length));
    bar.setAttribute("aria-valuemax", String(all.length));
    bar.setAttribute("aria-valuenow", String(done));
    redraw(bar, all.map((s) => s.state), () => all.map((s) => h("span", { class: "run-seg", "data-state": s.state })));
    card.setAttribute("aria-label", c.steps);
    // the steps are drawn again around the one that has the focus
    const focused = document.activeElement instanceof HTMLElement && card.contains(document.activeElement) ? document.activeElement.dataset.key : undefined;
    redraw(card, [all, viewed(), phase(), task?.remaining], () => [
      h(
        "ol",
        { class: "steps" },
        ...all.map((s, i) =>
          h(
            "li",
            { class: "step", "data-state": s.state },
            tasks.stepMark(s.state),
            h(
              "button",
              {
                type: "button",
                class: "step-pick",
                "aria-pressed": String(i === viewed()),
                "aria-label": c.showOutput(s.command),
                "data-key": "create:step:" + i,
                onclick: () => {
                  // the step at work is followed again when it is picked
                  selected = i === current() && phase() === "running" ? null : i;
                  draw();
                },
              },
              h("span", { class: "step-line" }, h("code", { class: "step-cmd", lang: "en" }, s.command), h("span", { class: "step-state" }, t.common.stepState[s.state])),
            ),
          ),
        ),
      ),
      phase() === "failed" &&
        h(
          "div",
          { class: "run-note" },
          task && task.remaining.length > 0 && h("p", { class: "hint" }, t.common.notRunSteps),
          task && task.remaining.length > 0 && h("div", { class: "cmd-list" }, ...task.remaining.map(command)),
          h("p", { class: "hint" }, ...c.notUndone(h("strong", { lang: "en" }, r.name))),
        ),
    ]);
    if (document.activeElement === document.body) {
      focusKey(focused);
    }
  }

  function drawTerminal(): void {
    const c = t.create;
    const at = viewed();
    const step = steps()[at];
    term.setAttribute("aria-label", c.output);
    const kept = outputs[at] ?? [];
    const live = step?.state === "running";
    redraw(termHead, [at, step, live ? 0 : kept.length], () => [
      h("span", { "aria-hidden": "true" }, ">"),
      h("strong", null, step?.command ?? ""),
      live && h("span", { class: "term-live", lang: document.documentElement.lang }, h("span", { class: "dot dot--pulse", "aria-hidden": "true" }), c.live),
      step &&
        !live &&
        h(
          "span",
          { class: "term-live term-live--idle", lang: document.documentElement.lang },
          step.state === "pending" ? c.notRun : c.lines(t.common.stepState[step.state], kept.length),
        ),
    ]);
    redraw(termBody, [at, step?.state], () => {
      shownStep = at;
      lines = h("div");
      lines.append(...kept.map(tasks.logLine));
      placeholder =
        kept.length > 0 || live || !step
          ? null
          : h("span", { class: "log-line", "data-kind": "dim", lang: document.documentElement.lang }, step.state === "pending" ? c.noOutput : c.silent);
      scroller = h(
        "div",
        { class: "term-body", role: "log", tabindex: 0, "aria-label": step ? c.outputOf(step.command) : c.output },
        lines,
        placeholder,
        live && h("span", { class: "caret", "aria-hidden": "true" }),
      );
      return [scroller];
    });
  }

  function drawFoot(): void {
    redraw(footSlot, ended === 0, () => [
      ended === 0 && h("div", { class: "run-foot" }, icon("info"), h("span", null, t.create.leave), toVms({ size: "sm" })),
    ]);
  }

  // The stages come from the VM list, which says where the VM is; the
  // output only tells it in words.
  function sync(): void {
    const vm = data.vms?.find((v) => v.name === r.name);
    const following = ended === 0 && task !== null && viewed() === current();
    redraw(stageSlot, [following, vm?.state, vm?.stage], () => {
      const bars = following && vm?.state === "starting" ? stageBars(vm.stage, true) : null;
      return [bars && h("div", { class: "term-head" }, bars)];
    });
  }

  function toEnd(): void {
    // the page is put back after this call
    requestAnimationFrame(() => {
      if (scroller && selected === null) {
        scroller.scrollTop = scroller.scrollHeight;
      }
    });
  }

  function draw(): void {
    drawHead();
    drawSteps();
    const before = scroller;
    drawTerminal();
    drawFoot();
    sync();
    if (scroller && scroller !== before) {
      scroller.scrollTop = scroller.scrollHeight;
    }
  }

  const observer: tasks.Observer = {
    update(next) {
      task = next;
      next.steps.forEach((_, i) => (outputs[i] ??= []));
      draw();
    },
    append(step, line) {
      const kept = (outputs[step] ??= []);
      kept.push(line);
      if (kept.length > tasks.keptLines) {
        kept.shift();
      }
      if (step === shownStep && lines && scroller) {
        placeholder?.remove();
        placeholder = null;
        tasks.appendLine(lines, line, scroller);
      }
    },
  };

  draw();
  const run: Run = {
    observer,
    unwatch: () => {},
    finish(result) {
      ended = Date.now();
      window.clearInterval(timer);
      if (result) {
        task = result;
      }
      draw();
    },
    draw,
    sync,
    toEnd,
    stop() {
      window.clearInterval(timer);
      run.unwatch();
    },
  };
  return run;
}

let current: Run | null = null;

function showRun(on: boolean): void {
  head.hidden = formBody.hidden = on;
  runBody.hidden = !on;
}

// back returns from the run to the form; reset clears it for another VM.
function back(reset: boolean): void {
  current?.stop();
  current = null;
  runBody.replaceChildren();
  if (reset) {
    name.value = "";
    programBoxes.forEach((b) => (b.checked = false));
  }
  showRun(false);
  enter();
  name.focus();
}

async function create(r: CreateRequest): Promise<void> {
  const view = newRun(r, images.some((i) => i.spec === r.image && !i.golden));
  current = view;
  showRun(true);
  // An action learns its task only when it ends; the steps show while
  // they run, from the first snapshot on.
  const cancel = tasks.whenStarted("Create " + r.name, (id) => (view.unwatch = tasks.watch(id, view.observer)));

  const { task, error } = await actStart(r.name, () => backend().CreateVM(r), true);
  cancel();
  if (current !== view) {
    return;
  }
  if (error !== undefined) {
    // refused before anything ran: back to the form with the reason
    back(false);
    failure = { what: "", detail: error };
    showProblem();
    return;
  }
  view.finish(task);
  if (!task?.ok && nav.current !== "create") {
    toast(t.common.failed("Create " + r.name), task?.error ?? "");
  }
}

onLanguage(() => {
  label();
  current?.draw();
});
label();

export function render(): HTMLElement {
  current?.sync();
  return page;
}
