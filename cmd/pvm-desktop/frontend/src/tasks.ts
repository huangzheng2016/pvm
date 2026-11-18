// Tasks are the commands the app runs. The backend sends a snapshot on
// every step change and each output line as it is printed; this module
// keeps them and draws them in the Activity panel. Create watches its own
// task through watch.

import { onTask, onTaskOutput, type Step, type Task } from "./api";
import { badge, button, callout, command, dot, h, spinner } from "./dom";
import { onLanguage, t } from "./i18n";
import { icon } from "./icons";

const keptTasks = 30;
export const keptLines = 2000;

// Observer follows one task: every snapshot, and every line as it comes.
export interface Observer {
  update(task: Task): void;
  append(step: number, line: string): void;
}

interface Entry {
  task: Task;
  output: string[][];
  // when the page first heard of the task, and when it ended
  started: number;
  ended: number;
  observers: Set<Observer>;
}

const entries = new Map<number, Entry>();
const starts: { title: string; found: (id: number) => void }[] = [];

export function statusBadge(task: Task): HTMLElement {
  if (!task.done) {
    return badge(t.tasks.running, "accent", dot(true));
  }
  return task.ok ? badge(t.tasks.done, "ok", dot()) : badge(t.tasks.failed, "danger", dot());
}

// stepMark is the round sign in front of a step.
export function stepMark(state: Step["state"]): HTMLElement {
  const sign = state === "running" ? spinner() : state === "done" ? icon("check") : state === "failed" ? icon("cross") : null;
  return h("span", { class: "step-mark", "aria-hidden": "true" }, sign);
}

// logLine is one line of output: always a text node.
export const logLine = (line: string): HTMLElement => h("span", { class: "log-line" }, line);

// appendLine adds a line to a log and stays at the end of scroller, the
// box that scrolls it, when it was there.
export function appendLine(log: HTMLElement, line: string, scroller = log): void {
  const atEnd = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 8;
  log.append(logLine(line));
  while (log.childElementCount > keptLines) {
    log.firstElementChild?.remove();
  }
  if (atEnd) {
    scroller.scrollTop = scroller.scrollHeight;
  }
}

function since(from: number, now: string, minutes: (n: number) => string, hours: (n: number) => string): string {
  const passed = Math.floor((Date.now() - from) / 60000);
  if (passed < 1) {
    return now;
  }
  return passed < 60 ? minutes(passed) : hours(Math.floor(passed / 60));
}

interface PanelView extends Observer {
  el: HTMLElement;
  tick(): void;
}

interface Row {
  item: HTMLElement;
  mark: HTMLElement;
  state: HTMLElement;
  toggle: HTMLButtonElement;
  log: HTMLElement;
  is: Step["state"] | "";
  lines: number;
  // chosen is what the user asked for with the toggle, when they did.
  chosen: boolean | undefined;
}

function panelView(entry: Entry): PanelView {
  const status = h("span");
  const time = h("span", { class: "task-time" });
  const steps = h("ol", { class: "steps" });
  const result = h("div", { class: "task-result" });
  const el = h(
    "article",
    { class: "task" },
    h("header", { class: "task-head" }, h("h3", { class: "task-title", lang: "en" }, entry.task.title), status, time),
    steps,
    result,
  );
  const rows: Row[] = [];

  // the step at work and a failed one show their output
  function draw(row: Row): void {
    const open = row.lines > 0 && (row.chosen ?? (row.is === "running" || row.is === "failed"));
    const wasHidden = row.log.hidden === true;
    row.log.hidden = !open;
    if (open && wasHidden) {
      // shown again: start at the newest line
      row.log.scrollTop = row.log.scrollHeight;
    }
    row.toggle.hidden = row.lines === 0;
    row.toggle.textContent = open ? t.tasks.hideOutput : t.tasks.showOutput(row.lines);
    row.toggle.setAttribute("aria-expanded", String(open));
  }

  function newRow(step: Step): Row {
    const log = h("div", { class: "log", role: "log", lang: "en", tabindex: 0, hidden: true, "aria-label": t.tasks.outputOf(step.command) });
    const toggle = h("button", {
      type: "button",
      class: "step-toggle",
      hidden: true,
      onclick: () => {
        row.chosen = row.log.hidden === true;
        draw(row);
      },
    });
    const state = h("span", { class: "step-state" });
    const mark = h("span");
    const item = h(
      "li",
      { class: "step" },
      mark,
      h("div", { class: "step-body" }, h("div", { class: "step-line" }, h("code", { class: "step-cmd", lang: "en" }, step.command), state), toggle, log),
    );
    const row: Row = { item, mark, state, toggle, log, is: "", lines: 0, chosen: undefined };
    return row;
  }

  function tick(): void {
    time.textContent = entry.task.done
      ? since(entry.ended, t.tasks.justNow, t.tasks.minutesAgo, t.tasks.hoursAgo)
      : since(entry.started, t.tasks.startedNow, t.tasks.startedMinutes, t.tasks.startedHours);
  }

  function append(step: number, line: string): void {
    const row = rows[step];
    if (!row) {
      return;
    }
    appendLine(row.log, line);
    row.lines = Math.min(row.lines + 1, keptLines);
    draw(row);
  }

  function update(task: Task): void {
    task.steps.forEach((step, i) => {
      if (!rows[i]) {
        rows[i] = newRow(step);
        steps.append(rows[i].item);
      }
      const row = rows[i];
      if (row.is !== step.state) {
        row.is = step.state;
        row.item.dataset.state = step.state;
        row.state.textContent = t.common.stepState[step.state];
        const mark = stepMark(step.state);
        row.mark.replaceWith(mark);
        row.mark = mark;
        draw(row);
      }
    });
    status.replaceChildren(statusBadge(task));
    tick();
    result.replaceChildren();
    if (task.done && !task.ok) {
      result.append(callout("danger", "alert", h("span", { lang: "en" }, task.error)));
      if (task.remaining.length > 0) {
        result.append(h("p", { class: "hint" }, t.common.notRunSteps), h("div", { class: "cmd-list" }, ...task.remaining.map(command)));
      }
    }
  }

  return { el, update, append, tick };
}

// replay brings an observer up to what a task has shown so far.
function replay(entry: Entry, observer: Observer): void {
  observer.update(entry.task);
  entry.output.forEach((lines, step) => lines.forEach((line) => observer.append(step, line)));
}

// watch follows a task from its start, including the output it printed
// before now. The result ends the watch.
export function watch(id: number, observer: Observer): () => void {
  const entry = entries.get(id);
  if (!entry) {
    return () => {};
  }
  entry.observers.add(observer);
  replay(entry, observer);
  return () => void entry.observers.delete(observer);
}

// whenStarted calls found once, with the id of the next new task of
// that title. An action learns its task only when it ends, and Create
// shows the steps while they run. The result cancels the wait.
export function whenStarted(title: string, found: (id: number) => void): () => void {
  const wait = { title, found };
  starts.push(wait);
  return () => {
    const at = starts.indexOf(wait);
    if (at >= 0) {
      starts.splice(at, 1);
    }
  };
}

const views = new Map<number, PanelView>();

const title = h("span", { class: "activity-title" });
const latest = h("span", { class: "activity-latest" });
const count = h("span", { class: "muted" });
const bar = h(
  "button",
  { type: "button", class: "activity-bar", "aria-expanded": "false", "aria-controls": "activity-panel", onclick: () => setOpen(panel.dataset.open !== "true") },
  icon("chevronUp"),
  title,
  latest,
  count,
);
bar.firstElementChild?.classList.add("activity-chev");
const intro = h("span");
const clear = button(
  "",
  () => {
    for (const [id, entry] of entries) {
      if (entry.task.done) {
        drop(id);
      }
    }
    summarise();
  },
  { size: "sm" },
);
const nothing = h("p", { class: "empty-text" });
const list = h("div", { class: "activity-tasks" });
const runningLine = h("span", { class: "activity-running", "aria-hidden": "true", hidden: true });

export const panel = h(
  "section",
  { class: "activity" },
  runningLine,
  bar,
  h("div", { class: "activity-panel", id: "activity-panel" }, h("div", { class: "activity-tools" }, intro, clear), nothing, list),
);

function setOpen(open: boolean): void {
  panel.dataset.open = String(open);
  bar.setAttribute("aria-expanded", String(open));
}

function drop(id: number): void {
  entries.delete(id);
  views.get(id)?.el.remove();
  views.delete(id);
}

function summarise(): void {
  const all = [...entries.values()].map((e) => e.task);
  const running = all.filter((task) => !task.done);
  const last = all[all.length - 1];
  const shown = running.length === 1 ? running[0] : last;
  if (running.length > 1) {
    latest.replaceChildren(h("span", null, t.tasks.manyRunning(running.length)), badge(t.tasks.running, "accent", dot(true)));
  } else if (shown) {
    latest.replaceChildren(h("span", { lang: "en" }, shown.title), statusBadge(shown));
  } else {
    latest.replaceChildren(h("span", null, t.tasks.none));
  }
  count.textContent = all.length === 0 ? "" : t.tasks.count(all.length, running.length);
  runningLine.hidden = running.length === 0;
  clear.disabled = all.every((task) => !task.done);
  nothing.hidden = all.length > 0;
}

function label(): void {
  panel.setAttribute("aria-label", t.tasks.title);
  title.textContent = t.tasks.title;
  intro.textContent = t.tasks.intro;
  clear.textContent = t.tasks.clear;
  nothing.textContent = t.tasks.none;
  summarise();
}

// record keeps a snapshot. A task without steps ran nothing in the
// background, such as opening a terminal, and is not shown.
export function record(task: Task): void {
  if (task.steps.length === 0) {
    return;
  }
  let entry = entries.get(task.id);
  if (!entry) {
    entry = { task, output: task.steps.map(() => []), started: Date.now(), ended: 0, observers: new Set() };
    entries.set(task.id, entry);
    const view = panelView(entry);
    entry.observers.add(view);
    views.set(task.id, view);
    list.prepend(view.el);
    const done = [...entries].filter(([, e]) => e.task.done).map(([id]) => id);
    for (const id of done.slice(0, Math.max(0, entries.size - keptTasks))) {
      drop(id);
    }
    const at = starts.findIndex((s) => s.title === task.title);
    if (at >= 0) {
      starts.splice(at, 1)[0].found(task.id);
    }
  }
  entry.task = task;
  if (task.done && entry.ended === 0) {
    entry.ended = Date.now();
  }
  entry.observers.forEach((o) => o.update(task));
  summarise();
}

// reveal opens the panel at a task, as when it failed.
export function reveal(id: number): void {
  setOpen(true);
  views.get(id)?.el.scrollIntoView({ block: "nearest" });
}

export function listen(): void {
  onTask(record);
  onTaskOutput((o) => {
    const entry = entries.get(o.id);
    const lines = entry?.output[o.step];
    if (!entry || !lines) {
      return;
    }
    lines.push(o.line);
    if (lines.length > keptLines) {
      lines.shift();
    }
    entry.observers.forEach((v) => v.append(o.step, o.line));
  });
  // the ages shown in the panel
  window.setInterval(() => views.forEach((view) => view.tick()), 30000);
}

// The words inside a task are drawn once, so a change of language draws
// every task again from what was kept.
onLanguage(() => {
  for (const [id, entry] of entries) {
    const old = views.get(id);
    const view = panelView(entry);
    if (old) {
      entry.observers.delete(old);
      old.el.replaceWith(view.el);
    }
    entry.observers.add(view);
    views.set(id, view);
    replay(entry, view);
  }
  label();
});
label();
