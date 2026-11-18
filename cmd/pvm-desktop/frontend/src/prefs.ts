// What the window remembers between runs: how it reads and looks. It
// says nothing about pvm, so it stays in the page's own storage and not
// in pvm's config.

const prefix = "pvm-desktop.";

export function load(name: string): string | null {
  try {
    return localStorage.getItem(prefix + name);
  } catch {
    return null;
  }
}

// save keeps a choice, or forgets it for null. Without storage the choice
// lasts for this run.
export function save(name: string, value: string | null): void {
  try {
    if (value === null) {
      localStorage.removeItem(prefix + name);
    } else {
      localStorage.setItem(prefix + name, value);
    }
  } catch {
    // nothing to do: the caller holds the value for now
  }
}
