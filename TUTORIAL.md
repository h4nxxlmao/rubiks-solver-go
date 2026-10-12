# Tutorial: solving cubes with rubiks-solver-go

This walks from a fresh clone to a working solve in Node, then in a browser Web
Worker. Every step is copy-pasteable.

## 0. What you need

- Go 1.22 or newer (`go version`)
- Node 20 or newer for the build script and the examples (`node --version`)
- Optional: any static file server for the browser demo (`npx serve`)

## 1. Build the wasm

```bash
git clone https://github.com/h4nxxlmao/rubiks-solver-go
cd rubiks-solver-go
node build.mjs
```

You now have:

```
dist/solver.wasm       the compiled solver
dist/wasm_exec.js      the Go runtime that drives it
```

`build.mjs` is a thin wrapper around:

```bash
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o dist/solver.wasm .
```

## 2. Load it in Node and solve one cube

The Go runtime never exits (it holds JavaScript callbacks open), so a script has
to call `process.exit()` when it is done.

```js
// solve-one.mjs
import { readFileSync } from "node:fs";
import vm from "node:vm";

// wasm_exec.js expects to run at global scope
vm.runInThisContext(readFileSync("dist/wasm_exec.js", "utf8"));

const go = new globalThis.Go();
const bytes = readFileSync("dist/solver.wasm");
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
go.run(instance);

// wait for the Go program to register its global
while (!globalThis.cubeSolver) await new Promise((r) => setTimeout(r, 20));

const solver = globalThis.cubeSolver;
solver.init(); // builds the pruning tables, about a second

// a position: 54 facelets in U R F D L B order, row-major within each face
const scrambled = solver.randomState(42);
const { solution, error } = solver.solve(scrambled, 22, 15000);

if (error) {
  console.error("failed:", error);
  process.exit(1);
}

console.log("solution:", solution);
console.log("moves:", solution.split(" ").length);

// verify: applying the solution must reach the solved state
const solved = solver.apply(scrambled, solution);
console.log("solved:", solver.isSolved(solved));

process.exit(0);
```

Run it:

```bash
node solve-one.mjs
```

You should see something like:

```
solution: U' R2 F L' D B2 ... 
moves: 19
solved: true
```

## 3. Understand the state format

A position is 54 characters. Faces in order `U R F D L B`, each read row-major
from outside the face:

```
        U0 U1 U2
        U3 U4 U5
        U6 U7 U8
L0 L1 L2 F0 F1 F2 R0 R1 R2 B0 B1 B2
L3 L4 L5 F3 F4 F5 R3 R4 R5 B3 B4 B5
L6 L7 L8 F6 F7 F8 R6 R7 R8 B6 B7 B8
        D0 D1 D2
        D3 D4 D5
        D6 D7 D8
```

The solved state is therefore:

```
UUUUUUUUURRRRRRRRRFFFFFFFFFDDDDDDDDDLLLLLLLLLBBBBBBBBB
```

Sticker letters are the face letters themselves, so a solved face is nine of
its own letter.

## 4. Turn a solved cube into a scramble

To build a scramble that produces a chosen position, solve the position and
invert the solution:

```js
function invertAlg(moves) {
  const out = [];
  for (let i = moves.length - 1; i >= 0; i--) {
    const m = moves[i];
    out.push(m.endsWith("'") ? m.slice(0, -1) : m.endsWith("2") ? m : `${m}'`);
  }
  return out;
}

const target = solver.randomState(7);
const { solution } = solver.solve(target, 22, 15000);
const scramble = invertAlg(solution.split(" ")).join(" ");
console.log(scramble); // applying this to a solved cube reaches `target`
```

This is exactly how "random-state" scrambles work: uniform random position,
solve, invert.

## 5. Run the self-test

```bash
node examples/node-selfcheck.mjs 25
```

It generates random legal states, solves them, checks the solutions apply to
solved, and prints a JSON report. Exit code is non-zero if anything failed,
which makes it suitable for CI.

## 6. Use it in a browser Web Worker

The solver must not block the main thread while it builds tables, so run it in a
worker. A complete minimal example lives in `examples/browser/`.

`worker.js`:

```js
self.importScripts("/dist/wasm_exec.js"); // served from the repo root

let ready = false;

async function boot() {
  if (ready) return;
  globalThis.__solverProgress = (value) => self.postMessage({ type: "progress", value });

  const go = new globalThis.Go();
  const bytes = await (await fetch("/dist/solver.wasm")).arrayBuffer();
  const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
  go.run(instance);

  while (!globalThis.cubeSolver) await new Promise((r) => setTimeout(r, 25));
  globalThis.cubeSolver.init();
  ready = true;
}

self.onmessage = async (event) => {
  const { id, facelets } = event.data;
  await boot();
  const result = globalThis.cubeSolver.solve(facelets, 22, 15000);
  self.postMessage({ type: "result", id, ...result });
};
```

Serve the repository root so `/dist/...` resolves, then open the demo:

```bash
npx serve .
# open http://localhost:3000/examples/browser/
```

Paste a facelet string and press Solve. The page shows table-build progress and
then the move list.

## 7. Integrate with a UI

A few practical notes:

- **Build once.** Call `init()` a single time and keep the worker (and its
  tables) alive. Rebuilding tables per solve is the main way to make this feel
  slow.
- **Validate before solving.** Only feed 54-character strings with exactly nine
  of each letter, and with the centre stickers in place. Invalid input returns
  `error` rather than crashing, but rejecting it earlier gives better messages.
- **Show progress.** Point `globalThis.__solverProgress` at a callback before
  calling `init()`; it receives a fraction in `[0, 1]`.
- **Bound the work.** Pass `maxDepth` (default 22) and `timeoutMs` so a UI can
  always bail out with a message instead of hanging.

## 8. Rotated positions

If your UI lets people turn middle layers (`M E S`) or the whole cube
(`x y z`), the centres end up permuted, and the fixed-centre facelet model
cannot represent that. There are two ways to deal with it:

- **Let the solver handle it.** `solve()` re-orients the input to the standard
  centres and returns a solution in that frame. Your UI should turn the
  displayed cube to match before playing the moves.
- **Normalise on your side first.** If you need the rotation itself (to animate
  it, say), find it before calling `solve()` and pass the standard-centre state.

## 9. Troubleshooting

- **`Go 1.22+ was not found on PATH`** - install Go or set `GO_BINARY` to the
  `go` executable and re-run `node build.mjs`.
- **`could not find wasm_exec.js`** - your Go install is missing the js/wasm
  runtime. Update Go.
- **The solver never initialises** - the wasm failed to fetch. Check the path
  (`/dist/solver.wasm`) and the MIME type; serve `.wasm` as `application/wasm`
  or use `arrayBuffer()` + `WebAssembly.instantiate(bytes, ...)` as above.
- **`No solution within limits.`** - the position is not reachable, or the
  timeout is too small. Re-check the facelets.

## 10. Where to go next

- Read `main.go`: geometry, the cubie model, coordinate encoding, pruning-table
  breadth-first search and the two-phase search are all there, commented.
- Read the [README](./README.md) for the JavaScript API table and the phase
  coordinate summary.
