// The typed edge to the Go backend. Field names are the json tags of
// internal/desktop; the Backend interface is checked against the Go
// methods by TestFrontendAPI.

export interface State {
  backend: string; // native or wsl
  daemon_healthy: boolean;
  daemon_stopped: boolean;
  app_version: string;
  daemon_version: string;
  accel: string;
  tcg_fallback: number;
  data_dir: string;
}

export interface VM {
  name: string;
  image: string;
  state: "stopped" | "starting" | "ready" | "failed";
  stage: string;
  hint: string;
  cpus: number;
  mem_mb: number;
  disk_gb: number;
  exit: string;
  exit_own: boolean;
  default: boolean;
}

export interface CatalogApp {
  id: string;
  name: string;
  description: string;
  homepage: string;
  source: string; // official or community
  window: boolean;
  start: string;
}

export interface Exit {
  name: string;
  kind: "agent" | "warp";
  endpoint: string;
  tz: string;
  locale: string;
  default: boolean;
  vms: string[];
  rules: string[];
}

export interface Workspace {
  id: string;
  vm: string;
  mode: "mount" | "sync" | "copy";
  host_path: string;
  guest_path: string;
  read_only: boolean;
  last_transfer: string; // RFC 3339, or empty
}

export interface Settings {
  default_vm: string;
  daemon_at_login: boolean;
  app_at_login: boolean;
}

export interface Image {
  spec: string;
  golden: boolean;
}

export interface CreateDefaults {
  user: string;
  cpus: number;
  mem_mb: number;
  disk_gb: number;
  exit: string;
}

// An empty field leaves that setting as it is.
export interface AppConfig {
  clipboard: string; // off, to-guest, to-host or both
  drop: string; // on or off
  audio: string; // off, out or both
}

export interface CreateRequest {
  name: string;
  image: string;
  user: string;
  cpus: number;
  mem_mb: number;
  disk_gb: number;
  exit: string;
  app_config: AppConfig;
  apps: string[];
}

export interface AddExitRequest {
  name: string;
  server: string;
  token: string;
}

export interface CreateWorkspaceRequest {
  id: string;
  vm: string;
  mode: string;
  host_path: string;
  read_only: boolean;
  confirmed: boolean;
}

export interface Step {
  command: string;
  state: "pending" | "running" | "done" | "failed";
}

export interface Task {
  id: number;
  title: string;
  steps: Step[];
  done: boolean;
  ok: boolean;
  error: string;
  remaining: string[];
}

export interface TaskOutput {
  id: number;
  step: number;
  line: string;
}

// A read, or an action the backend refused, rejects with an error
// string. An action that ran resolves with its Task; ok says how it went.
export interface Backend {
  State(): Promise<State>;
  VMs(): Promise<VM[]>;
  InstalledApps(vm: string): Promise<CatalogApp[]>;
  Catalog(): Promise<CatalogApp[]>;
  Exits(): Promise<Exit[]>;
  DefaultExit(): Promise<string>;
  Workspaces(): Promise<Workspace[]>;
  Settings(): Promise<Settings>;
  Images(): Promise<Image[]>;
  CreateDefaults(): Promise<CreateDefaults>;
  PickFolder(): Promise<string>;

  StartDaemon(): Promise<Task>;
  StartVM(name: string): Promise<Task>;
  StopVM(name: string): Promise<Task>;
  DeleteVM(name: string, confirmed: boolean): Promise<Task>;
  RunApp(name: string, target: string): Promise<Task>;
  RunCatalogApp(name: string, id: string): Promise<Task>;
  OpenFiles(name: string): Promise<Task>;
  ConfigureApps(name: string, config: AppConfig): Promise<Task>;
  InstallApps(name: string, ids: string[]): Promise<Task>;
  SetDefaultVM(name: string): Promise<Task>;
  ClearDefaultVM(): Promise<Task>;
  SetVMExit(name: string, via: string, confirmed: boolean): Promise<Task>;
  SetDefaultExit(via: string): Promise<Task>;
  CreateVM(request: CreateRequest): Promise<Task>;
  AddExit(request: AddExitRequest): Promise<Task>;
  AddWarpExit(name: string): Promise<Task>;
  TestExit(name: string, speed: boolean): Promise<Task>;
  RemoveExit(name: string, confirmed: boolean): Promise<Task>;
  RunWorkspaceApp(id: string, target: string): Promise<Task>;
  OpenWorkspaceFiles(id: string): Promise<Task>;
  SyncWorkspace(id: string, direction: "push" | "pull"): Promise<Task>;
  CreateWorkspace(request: CreateWorkspaceRequest): Promise<Task>;
  RemoveWorkspace(id: string, confirmed: boolean): Promise<Task>;
  SetDaemonAtLogin(on: boolean): Promise<Task>;

  OpenTerminal(name: string): Promise<void>;
  OpenWorkspaceTerminal(id: string): Promise<void>;
  OpenWorkspaceFolder(id: string): Promise<void>;
  SetAppAtLogin(on: boolean): Promise<void>;
}

interface Runtime {
  EventsOn(name: string, callback: (data: never) => void): () => void;
  WindowIsMinimised(): Promise<boolean>;
  ClipboardSetText(text: string): Promise<boolean>;
}

declare global {
  interface Window {
    go: { desktop: { App: Backend } };
    runtime: Runtime;
  }
}

// Bridge is all the page needs from its host: the bound methods, the two
// events and two services of the window.
export interface Bridge {
  backend(): Backend;
  onTask(callback: (task: Task) => void): void;
  onTaskOutput(callback: (output: TaskOutput) => void): void;
  windowIsMinimised(): Promise<boolean>;
  copyText(text: string): Promise<boolean>;
}

let bridge: Bridge = {
  backend: () => window.go.desktop.App,
  onTask: (callback) => void window.runtime.EventsOn("task", callback),
  onTaskOutput: (callback) => void window.runtime.EventsOn("task-output", callback),
  windowIsMinimised: () => window.runtime.WindowIsMinimised(),
  copyText: (text) => window.runtime.ClipboardSetText(text),
};

// bound is false where no Wails window carries the page, as under
// `npm run dev` in a browser.
export const bound = (): boolean => "go" in window;

// connect puts another host in place of the Wails bindings: mock.ts.
export function connect(other: Bridge): void {
  bridge = other;
}

export const backend = (): Backend => bridge.backend();

export const onTask = (callback: (task: Task) => void): void => bridge.onTask(callback);

export const onTaskOutput = (callback: (output: TaskOutput) => void): void => bridge.onTaskOutput(callback);

export const windowIsMinimised = (): Promise<boolean> => bridge.windowIsMinimised();

export const copyText = (text: string): Promise<boolean> => bridge.copyText(text);

// errorText reads a rejected call: the backend rejects with a string.
export function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
