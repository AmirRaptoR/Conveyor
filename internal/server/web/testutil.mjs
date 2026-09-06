// Shared helper for UI interaction tests: pulls a named top-level function
// out of index.html's inline <script> and evaluates it in a fresh sandbox,
// so a test exercises the real function with no browser, no build step, and
// no npm install — just the node:vm module from a stock Node.
//
// index.html declares one function per top-level `function name(...) {`
// and never nests a <script> tag, so brace-counting from the opening brace
// is enough to find where the function ends, however its body is written.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const indexHTML = readFileSync(path.join(here, "index.html"), "utf8");

export function extractFunction(name) {
  const marker = `function ${name}(`;
  const start = indexHTML.indexOf(marker);
  if (start === -1) {
    throw new Error(`extractFunction: no "${marker}" in index.html`);
  }
  const braceStart = indexHTML.indexOf("{", start);
  let depth = 0;
  let end = -1;
  for (let i = braceStart; i < indexHTML.length; i++) {
    if (indexHTML[i] === "{") depth++;
    else if (indexHTML[i] === "}") {
      depth--;
      if (depth === 0) {
        end = i + 1;
        break;
      }
    }
  }
  if (end === -1) {
    throw new Error(`extractFunction: unbalanced braces for "${name}"`);
  }
  return indexHTML.slice(start, end);
}

// Loads one or more functions into a fresh sandbox and returns them,
// callable, with no DOM and none of index.html's other globals in scope —
// so a test can only see what it names.
export function loadFunctions(...names) {
  const src =
    names.map(extractFunction).join("\n\n") +
    `\n\nmodule.exports = { ${names.join(", ")} };`;
  const sandbox = { module: { exports: {} } };
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox);
  return sandbox.module.exports;
}
