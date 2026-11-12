/**
 * Root entry point for @4aicoding/portal-sdk.
 *
 * Thin re-export of `./src/index.js` so that `package.json` can map
 * `exports['.']` to `./dist/index.js` (a top-level file) while keeping
 * all SDK source under `src/`.
 */

export * from "./src/index.js";
export { VERSION } from "./src/index.js";
