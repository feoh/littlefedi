// Lints the web UI's handwritten scripts. Run through prek, which installs
// these packages and puts them on NODE_PATH; the repository has no
// package.json. CommonJS because an ES module import would not search
// NODE_PATH.
const js = require("@eslint/js");
const globals = require("globals");

module.exports = [
  { ignores: ["**/*.min.js", "**/emoji_map.js"] },
  js.configs.recommended,
  {
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: "script",
      globals: { ...globals.browser, Alpine: "readonly", htmx: "readonly" },
    },
    rules: {
      // Upstream's idiom for best-effort storage and parsing:
      // try { ... } catch (_) {}
      "no-empty": ["error", { allowEmptyCatch: true }],
      "no-unused-vars": ["error", { caughtErrors: "none", argsIgnorePattern: "^_" }],
    },
  },
  {
    files: ["**/sw.js"],
    languageOptions: { globals: { ...globals.serviceworker } },
  },
];
