#!/usr/bin/env node
/**
 * Builds main.go to dist/solver.wasm with GOOS=js GOARCH=wasm and copies the
 * matching wasm_exec.js runtime next to it.
 *
 *   node build.mjs
 *
 * Needs Go 1.22 or newer on PATH.
 */
import { execFileSync, spawnSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, rmSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(fileURLToPath(import.meta.url));
const outDir = join(root, "dist");
const outWasm = join(outDir, "solver.wasm");
const outExec = join(outDir, "wasm_exec.js");

function findGo() {
  const candidates = [
    process.env.GO_BINARY,
    "go",
    "C:\\Program Files\\Go\\bin\\go.exe",
    "C:\\Go\\bin\\go.exe",
    join(process.env.LOCALAPPDATA ?? "", "Programs", "Go", "bin", "go.exe"),
    "/usr/local/go/bin/go",
    "/usr/lib/go/bin/go",
  ].filter(Boolean);

  for (const candidate of candidates) {
    const result = spawnSync(candidate, ["version"], { encoding: "utf8" });
    if (result.status === 0) return candidate;
  }
  return null;
}

const go = findGo();
if (!go) {
  console.error("Go 1.22+ was not found on PATH. Install Go, then re-run: node build.mjs");
  process.exit(1);
}

rmSync(outDir, { recursive: true, force: true });
mkdirSync(outDir, { recursive: true });

console.log(`[build] compiling main.go with ${go}`);
execFileSync(
  go,
  ["build", "-trimpath", "-ldflags=-s -w", "-o", outWasm, "."],
  { cwd: root, env: { ...process.env, GOOS: "js", GOARCH: "wasm" }, stdio: "inherit" },
);

const goroot = execFileSync(go, ["env", "GOROOT"], { encoding: "utf8" }).trim();
const execCandidates = [
  join(goroot, "lib", "wasm", "wasm_exec.js"),
  join(goroot, "misc", "wasm", "wasm_exec.js"),
];
const execSrc = execCandidates.find((p) => existsSync(p));
if (!execSrc) {
  console.error(`[build] could not find wasm_exec.js under ${goroot}`);
  process.exit(1);
}
copyFileSync(execSrc, outExec);

const kb = Math.round(statSync(outWasm).size / 1024);
console.log(`[build] wrote dist/solver.wasm (${kb} KB)`);
console.log("[build] copied dist/wasm_exec.js");
