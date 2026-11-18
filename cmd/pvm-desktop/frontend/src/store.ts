// What the screens show, and the one way they change anything.

import { backend, errorText, type Exit, type Settings, type State, type Task, type VM, type Workspace } from "./api";
import { button, h } from "./dom";
import * as tasks from "./tasks";

export const data = {
  state: null as State | null,
  // stateError is why State itself could not be read.
  stateError: "",
  vms: null as VM[] | null,
  exits: null as Exit[] | null,
  // defaultExit is direct or a name; the name of a removed exit stays.
  defaultExit: null as string | null,
  workspaces: null as Workspace[] | null,
  settings: null as Settings | null,
  // errors holds the last failure of each list, by its key in data.
  errors: {} as Record<string, string>,
};

// Holds counts the actions that hold a name. Two can overlap, as a stop
// during a start's wait does, and the name stays until the last ends.
export class Holds {
  private readonly held = new Map<string, number>();
  has = (name: string): boolean => this.held.has(name);
  count = (name: string): number => this.held.get(name) ?? 0;
  add(name: string): void {
    this.held.set(name, this.count(name) + 1);
  }
  delete(name: string): void {
    const left = this.count(name) - 1;
    if (left > 0) {
      this.held.set(name, left);
    } else {
      this.held.delete(name);
    }
  }
  entries = (): [string, number][] => [...this.held];
}

// busy names what a running action works on: vm:<name>, exit:<name>,
// workspace:<id>, daemon or settings. Its controls are disabled.
export const busy = new Holds();

// following names the VMs whose action is a start. Its wait can take
// many minutes, so such a VM can still be stopped or deleted.
export const following = new Holds();

// onlyFollowed is true while nothing but a start's wait works on the VM:
// a stop or a delete sent meanwhile locks the row again.
export const onlyFollowed = (name: string): boolean => following.has(name) && busy.count("vm:" + name) === following.count(name);

const listeners: (() => void)[] = [];
export const onChange = (f: () => void): void => void listeners.push(f);
export const changed = (): void => listeners.forEach((f) => f());

export const ready = (): boolean => data.state?.backend === "native" && data.state.daemon_healthy;

async function read<T>(key: "vms" | "exits" | "defaultExit" | "workspaces" | "settings", call: () => Promise<T>): Promise<void> {
  try {
    (data[key] as T | null) = await call();
    delete data.errors[key];
  } catch (e) {
    data.errors[key] = errorText(e);
  }
}

export const readVMs = (): Promise<void> => read("vms", () => backend().VMs());

let refreshing: Promise<void> | null = null;
let again = false;

// refresh re-reads everything the daemon and the stores hold. A call
// during a refresh makes that one run once more instead of overlapping.
export function refresh(): Promise<void> {
  if (refreshing) {
    again = true;
    return refreshing;
  }
  refreshing = (async () => {
    do {
      again = false;
      if (ready()) {
        await Promise.all([
          readVMs(),
          read("exits", () => backend().Exits()),
          read("defaultExit", () => backend().DefaultExit()),
          read("workspaces", () => backend().Workspaces()),
          read("settings", () => backend().Settings()),
        ]);
      }
      changed();
    } while (again);
  })().finally(() => (refreshing = null));
  return refreshing;
}

const toasts = h("div", { class: "toasts", role: "alert" });
export const toastArea: HTMLElement = toasts;

export function toast(text: string): void {
  const el = h("div", { class: "toast" }, h("span", null, text), button("Dismiss", () => el.remove(), { kind: "plain" }));
  toasts.append(el);
  while (toasts.childElementCount > 3) {
    toasts.firstElementChild?.remove();
  }
}

export interface Outcome {
  task?: Task;
  // error is why the backend refused, before running anything.
  error?: string;
}

// act runs one action: it marks what it works on as busy, reports a
// refusal or a failed task, and re-reads the state afterwards. quiet
// leaves the reporting to the caller.
export async function act(keys: string[], call: () => Promise<Task | void>, quiet = false): Promise<Outcome> {
  keys.forEach((k) => busy.add(k));
  changed();
  const outcome: Outcome = {};
  try {
    const task = await call();
    if (task) {
      outcome.task = task;
      tasks.record(task);
      if (!task.ok && !quiet) {
        tasks.reveal(task.id);
        toast(task.title + " failed: " + task.error);
      }
    }
  } catch (e) {
    outcome.error = errorText(e);
    if (!quiet) {
      toast(outcome.error);
    }
  }
  // the lists are current again before the controls come back
  await refresh();
  keys.forEach((k) => busy.delete(k));
  changed();
  return outcome;
}

// actStart is act for an action that starts a VM and waits for it.
export async function actStart(name: string, call: () => Promise<Task>, quiet = false): Promise<Outcome> {
  following.add(name);
  try {
    return await act(["vm:" + name], call, quiet);
  } finally {
    following.delete(name);
  }
}

export const nav = { current: "vms", go: (_screen: string): void => {} };

export function memory(mb: number): string {
  return mb >= 1024 ? Number((mb / 1024).toFixed(1)) + " GB" : mb + " MB";
}
