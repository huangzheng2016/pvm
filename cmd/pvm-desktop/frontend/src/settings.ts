import { backend } from "./api";
import { checkbox, field, h, pageHeader, select } from "./dom";
import { act, busy, data } from "./store";

function about(): HTMLElement {
  const st = data.state!;
  const rows: [string, string][] = [
    ["pvm Desktop", st.app_version],
    ["pvm daemon", st.daemon_version],
    ["Accelerator", st.accel],
    ["Data folder", st.data_dir],
  ];
  const card = h("section", { class: "card" }, h("h2", null, "About"));
  if (st.daemon_version !== "" && st.app_version !== st.daemon_version) {
    card.append(
      h(
        "p",
        { class: "warning" },
        "pvm Desktop is " + st.app_version + " and pvm is " + st.daemon_version + ". `pvm upgrade` replaces pvm.exe only: run the pvm installer again to bring pvm Desktop to the same version.",
      ),
    );
  }
  if (st.tcg_fallback > 0) {
    card.append(
      h(
        "p",
        { class: "warning" },
        (st.tcg_fallback === 1 ? "1 running VM fell" : st.tcg_fallback + " running VMs fell") +
          " back to software emulation (TCG) because hardware acceleration was not available. They run much slower.",
      ),
    );
  }
  card.append(h("dl", null, ...rows.flatMap(([name, value]) => [h("dt", null, name), h("dd", { title: value }, value === "" ? "unknown" : value)])));
  return card;
}

export function render(): HTMLElement {
  const page = h("div", { class: "page" }, pageHeader("Settings"));
  if (data.errors.settings) {
    page.append(h("p", { class: "error banner", role: "alert" }, "Cannot read the settings: " + data.errors.settings));
  }
  const s = data.settings;
  if (s) {
    const working = busy.has("settings");
    const names = (data.vms ?? []).map((vm) => vm.name);
    const options = [{ value: "", label: "None" }, ...names.map((n) => ({ value: n, label: n }))];
    const missing = s.default_vm !== "" && !names.includes(s.default_vm);
    if (missing) {
      options.push({ value: s.default_vm, label: s.default_vm + " (deleted)" });
    }
    let chosen = s.default_vm;
    const vm = select(options, s.default_vm, {
      disabled: working,
      "data-key": "settings:default-vm",
      onchange: () => {
        // The screen is not redrawn under a focused field, so the field
        // itself refuses a second change while one runs. Disabling it
        // would drop the keyboard focus.
        if (busy.has("settings")) {
          vm.value = chosen;
          return;
        }
        chosen = vm.value;
        void act(["settings"], () => (chosen === "" ? backend().ClearDefaultVM() : backend().SetDefaultVM(chosen))).then((o) => {
          // a failed change redraws nothing: the state is what it was
          if (o.error !== undefined || o.task?.ok === false) {
            vm.value = chosen = data.settings?.default_vm ?? "";
          }
        });
      },
    });
    const daemon = checkbox("Start the pvm daemon when I sign in", s.daemon_at_login, {
      disabled: working,
      "data-key": "settings:daemon-login",
      onchange: () => void act(["settings"], () => backend().SetDaemonAtLogin(daemon.box.checked)),
    });
    const app = checkbox("Open pvm Desktop when I sign in", s.app_at_login, {
      disabled: working,
      "data-key": "settings:app-login",
      onchange: () => void act(["settings"], () => backend().SetAppAtLogin(app.box.checked)),
    });
    page.append(
      h(
        "section",
        { class: "card" },
        h("h2", null, "Default VM"),
        field("VM", vm, "The VM that pvm commands use when none is named and the folder is not in a workspace."),
        missing && h("p", { class: "warning" }, "The default VM " + s.default_vm + " no longer exists. Choose another, or None."),
      ),
      h("section", { class: "card" }, h("h2", null, "At sign-in"), daemon.el, app.el),
    );
  } else if (!data.errors.settings) {
    page.append(h("p", { class: "muted" }, "Loading..."));
  }
  if (data.state) {
    page.append(about());
  }
  return page;
}
