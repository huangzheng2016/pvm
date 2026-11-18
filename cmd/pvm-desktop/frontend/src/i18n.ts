// The language of the interface. The choice is the window's own: it is
// kept by prefs, and "system" follows the language Windows shows.

import { en, type Messages } from "./en";
import * as prefs from "./prefs";
import { zh } from "./zh";

export type Language = "en" | "zh-CN";
export type Choice = "system" | Language;

const tables: Record<Language, Messages> = { en, "zh-CN": zh };

function stored(): Choice {
  const value = prefs.load("language");
  return value === "en" || value === "zh-CN" ? value : "system";
}

let chosen = stored();

export const choice = (): Choice => chosen;

export function language(): Language {
  if (chosen !== "system") {
    return chosen;
  }
  return /^zh\b/i.test(navigator.language) ? "zh-CN" : "en";
}

// t is the text of the current language. It changes when the language
// does, so read it when drawing and do not keep a message in a constant.
export let t: Messages = tables[language()];

const listeners: (() => void)[] = [];

// onLanguage registers a redraw for the parts that are built once and
// kept, such as the sidebar.
export const onLanguage = (f: () => void): void => void listeners.push(f);

function apply(): void {
  t = tables[language()];
  document.documentElement.lang = language();
  listeners.forEach((f) => f());
}

export function choose(c: Choice): void {
  chosen = c;
  prefs.save("language", c === "system" ? null : c);
  apply();
}

window.addEventListener("languagechange", () => {
  if (chosen === "system") {
    apply();
  }
});
document.documentElement.lang = language();
