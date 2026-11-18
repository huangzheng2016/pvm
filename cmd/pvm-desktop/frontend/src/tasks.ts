// Tasks are the commands the app runs. The backend sends a snapshot on
// every step change and each output line as it is printed; this module
// keeps them and draws them, in the activity panel and on Create.

import { onTask, onTaskOutput, type Task } from "./api";
import { badge, button, command, h } from "./dom";

const keptTasks = 30;
const keptLines = 2000;

interface View {
  el: HTMLElement;
  update(task: Task): void;
  append(step: number, line: string): void;
}

interface Entry {
  task: Task;
  output: string[][];
  views: View[];
}

const entries = new Map<number, Entry>();
const starts: { title: string; found: (id: number) => void }[] = [];

function statusBadge(task: Task): HTMLElement {
  if (!task.done) {
    return badge("Running", "busy");
  }
  return task.ok ? badge("Done", "ok") : badge("Failed", "bad");
}

function newView(entry: Entry): View {
  const status = h("span", { class: "task-status" });
  const steps = h("ol", { class: "steps" });
  const result = h("div", { class: "task-result" });
  const el = h("section", { class: "task" }, h("div", { class: "task-head" }, h("strong", null, entry.task.title), status), steps, result);
  const outputs: HTMLElement[] = [];
  const rows: HTMLDetailsElement[] = [];
  const states: string[] = [];

  function append(step: number, line: string): void {
    const out = outputs[step];
    if (!out) {
      return;
    }
    const atEnd = out.scrollHeight - out.scrollTop - out.clientHeight < 8;
    out.append(line + "\n");
    while (out.childNodes.length > keptLines) {
      out.firstChild?.remove();
    }
    out.hidden = false;
    if (atEnd) {
      out.scrollTop = out.scrollHeight;
    }
  }

  function update(task: Task): void {
    task.steps.forEach((step, i) => {
      if (!rows[i]) {
        outputs[i] = h("pre", { class: "output", hidden: true, tabindex: 0 });
        rows[i] = h("details", null, h("summary", null, h("span", { class: "step-mark" }), h("code", null, step.command)), outputs[i]);
        steps.append(h("li", null, rows[i]));
      }
      if (states[i] !== step.state) {
        states[i] = step.state;
        rows[i].className = "step " + step.state;
        rows[i].querySelector(".step-mark")!.setAttribute("aria-label", step.state);
        // the step at work and a failed one show their output
        rows[i].open = step.state === "running" || step.state === "failed";
      }
    });
    status.replaceChildren(statusBadge(task));
    result.replaceChildren();
    if (task.done && !task.ok) {
      result.append(h("p", { class: "error" }, task.error));
      if (task.remaining.length > 0) {
        result.append(h("p", null, "These steps were not run. Fix the problem, then run them by hand:"), ...task.remaining.map(command));
      }
    }
  }

  update(entry.task);
  entry.output.forEach((lines, step) => lines.forEach((line) => append(step, line)));
  return { el, update, append };
}

// view draws one task; it stays live until the task is dropped.
export function view(id: number): HTMLElement | null {
  const entry = entries.get(id);
  if (!entry) {
    return null;
  }
  const v = newView(entry);
  entry.views.push(v);
  return v.el;
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

const list = h("div", { class: "activity-list", id: "activity-list", hidden: true });
const summary = h("span", { class: "activity-summary" }, "Nothing has run yet");
const toggle = h(
  "button",
  { type: "button", class: "activity-toggle", "aria-expanded": "false", "aria-controls": "activity-list", onclick: () => setOpen(list.hidden !== false) },
  h("strong", null, "Activity"),
  summary,
);
const clear = button("Clear finished", () => {
  for (const [id, entry] of entries) {
    if (entry.task.done) {
      drop(id);
    }
  }
  summarise();
});
clear.classList.add("small");
clear.hidden = true;

export const panel = h("section", { class: "activity", "aria-label": "Activity" }, h("div", { class: "activity-bar" }, toggle, clear), list);

const panelViews = new Map<number, HTMLElement>();

function setOpen(open: boolean): void {
  list.hidden = !open;
  clear.hidden = !open;
  toggle.setAttribute("aria-expanded", String(open));
}

function drop(id: number): void {
  entries.delete(id);
  panelViews.get(id)?.remove();
  panelViews.delete(id);
}

function summarise(): void {
  const all = [...entries.values()].map((e) => e.task);
  const running = all.filter((t) => !t.done);
  const last = all[all.length - 1];
  summary.className = "activity-summary";
  if (running.length > 1) {
    summary.textContent = running.length + " tasks running";
  } else if (running.length === 1) {
    summary.textContent = running[0].title + " - running";
  } else if (last) {
    summary.textContent = last.title + (last.ok ? " - done" : " - failed");
    summary.classList.toggle("bad", !last.ok);
  } else {
    summary.textContent = "Nothing has run yet";
  }
  panel.classList.toggle("running", running.length > 0);
}

// record keeps a snapshot. A task without steps ran nothing in the
// background, such as opening a terminal, and is not shown.
export function record(task: Task): void {
  if (task.steps.length === 0) {
    return;
  }
  let entry = entries.get(task.id);
  if (!entry) {
    entry = { task, output: task.steps.map(() => []), views: [] };
    entries.set(task.id, entry);
    const el = view(task.id)!;
    panelViews.set(task.id, el);
    list.prepend(el);
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
  entry.views.forEach((v) => v.update(task));
  summarise();
}

// reveal opens the panel at a task, as when it failed.
export function reveal(id: number): void {
  setOpen(true);
  panelViews.get(id)?.scrollIntoView({ block: "nearest" });
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
    entry.views.forEach((v) => v.append(o.step, o.line));
  });
}
