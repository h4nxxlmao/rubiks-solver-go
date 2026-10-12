#!/usr/bin/env node
/**
 * Loads dist/solver.wasm in Node and runs the built-in self-test:
 * random legal state -> solve -> apply -> must be solved.
 *
 *   node build.mjs && node examples/node-selfcheck.mjs 25
 *
 * The Go runtime never exits, so this script always calls process.exit().
 */
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..");
const wasmPath = join(root, "dist", "solver.wasm");
const execPath = join(root, "dist", "wasm_exec.js");

vm.runInThisContext(readFileSync(execPath, "utf8"), { filename: execPath });

const go = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
go.run(instance);

const started = Date.now();
while (!globalThis.cubeSolver) {
  if (Date.now() - started > 30000) throw new Error("solver never initialised");
  await new Promise((resolve) => setTimeout(resolve, 20));
}

const solver = globalThis.cubeSolver;
const initStart = Date.now();
solver.init();
const initMs = Date.now() - initStart;

const count = Number(process.argv[2] ?? 25);
const report = solver.selfTest(count);

// independent spot check through the exported apply/isSolved
let spotOk = true;
for (let i = 0; i < 5; i++) {
  const state = solver.randomState(Date.now() + i);
  const { solution, error } = solver.solve(state, 22, 15000);
  if (error || !solution || !solver.isSolved(solver.apply(state, solution))) {
    spotOk = false;
    break;
  }
}

console.log(JSON.stringify({ initMs, ...report, spotOk }, null, 2));

if (report.failed > 0 || !spotOk) {
  console.error("solver self-test FAILED");
  process.exit(1);
}
console.log("solver self-test OK");
process.exit(0);
