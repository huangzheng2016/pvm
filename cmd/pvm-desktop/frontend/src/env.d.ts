// What Vite puts on import.meta: DEV is true under `npm run dev` and a
// constant false in a build, which drops the code behind it.
interface ImportMeta {
  readonly env: { readonly DEV: boolean };
}
