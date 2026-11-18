import { readFileSync } from "node:fs";
import { defineConfig } from "vite";

// The Open Font License asks that its text travels with the font, so the
// build puts it next to the embedded font file.
const fontLicence = () => ({
  name: "pvm-font-licence",
  apply: "build",
  generateBundle() {
    this.emitFile({
      type: "asset",
      fileName: "licenses/Newsreader-OFL.txt",
      source: readFileSync(new URL("./node_modules/@fontsource-variable/newsreader/LICENSE", import.meta.url)),
    });
  },
});

// The page's policy allows what the built app needs and no more. The dev
// server also injects styles and talks to itself over a websocket.
const devPolicy = () => ({
  name: "pvm-dev-policy",
  apply: "serve",
  transformIndexHtml: (html) => html.replace("style-src 'self'", "style-src 'self' 'unsafe-inline'; connect-src 'self' ws:"),
});

export default defineConfig({ plugins: [fontLicence(), devPolicy()] });
