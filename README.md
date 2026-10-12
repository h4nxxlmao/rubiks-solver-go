# rubiks-solver-go

A Kociemba two-phase Rubik's Cube solver written in Go and compiled to
WebAssembly. It solves any valid 3x3 position, returns the move sequence, and
runs in the browser, in a Web Worker, or in Node.

- **Two-phase search** (Kociemba): phase 1 fixes corner orientation, edge
  orientation and the U/D-edge slice; phase 2 finishes with `U D R2 L2 F2 B2`.
- **Move derivation is geometric**, not table-copied: a move is "rotate this
  layer 90 degrees clockwise viewed from outside the face", which reproduces
  `U D L R F B`, `M E S`, `x y z` and wide turns from one implementation.
- **Fast**: pruning tables are built once at init (about a second in wasm),
  then random positions solve in ~19-20 moves in well under a second.
- **Self-contained**: no dependencies, no cgo. One Go file plus the wasm runtime
  that ships with your Go toolchain.
- **Self-test built in**: validates random states by solving and applying.

## Quick start

```bash
git clone https://github.com/h4nxxlmao/rubiks-solver-go
cd rubiks-solver-go

# needs Go 1.22+ on PATH
node build.mjs                 # -> dist/solver.wasm + dist/wasm_exec.js

node examples/node-selfcheck.mjs 25
```

The self-check solves random positions and verifies the solutions, printing a
summary:

```json
{ "passed": 25, "failed": 0, "avgMs": 640, "worstMs": 1500 }
```

Prefer Go commands directly? The only step is the wasm build:

```bash
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o dist/solver.wasm .
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" dist/
```

## JavaScript API

Loading the wasm registers one global, `cubeSolver`:

| method | description |
|---|---|
| `init()` | Build the pruning tables. Call once before solving. |
| `solve(facelets, maxDepth?, timeoutMs?)` | Returns `{ solution, error }`. `solution` is space-separated moves. |
| `apply(facelets, moves)` | Apply a move string, returns the new facelet string. |
| `isSolved(facelets)` | `true` when the position is solved. |
| `randomState(seed?)` | A uniformly random legal position (for random-state scrambles). |
| `selfTest(n?)` | Solve `n` random states and verify. Returns `{ passed, failed, avgMs, worstMs }`. |
| `stats()` | A one-line description of the built tables. |

During table construction, `globalThis.__solverProgress` is called with a
fraction from 0 to 1 if you define it:

```js
globalThis.__solverProgress = (value) => console.log(`${Math.round(value * 100)}%`);
```

## State format

A position is 54 characters in Kociemba facelet order: faces `U R F D L B`,
each 9 stickers read row-major from outside the face. The solved state is:

```
UUUUUUUUURRRRRRRRRFFFFFFFFFDDDDDDDDDLLLLLLLLLBBBBBBBBB
```

Moves use standard notation: `U D L R F B`, `U'` for counter-clockwise, `U2`
for a half turn, `M E S` for slices, `x y z` for whole-cube rotations.

## Rotated positions

Slices (`M E S`) and whole-cube turns (`x y z`) move the centres, which the
fixed-centre facelet model cannot express. The solver accepts those positions
anyway: it re-orients the input to the standard centres before searching, so a
cube scrambled with slices still solves. The solution that comes back is written
in that standard orientation, so a caller that draws the cube should turn it to
match before playing the moves.

Genuinely invalid positions (wrong sticker counts, a single flipped edge, two
swapped pieces) are still rejected with an `error`.

## How it works

Phase 1 reduces the cube to the coordinates `(corner orientation, edge
orientation, U/D-edge slice)`, then phase 2 finishes using only `U D R2 L2 F2 B2`.

| | coordinates | pruning |
|---|---|---|
| phase 1 | corner orientation (2187), edge orientation (2048), slice (495) | twist x slice, flip x slice |
| phase 2 | corner permutation (40320), edge permutation (40320), middle-edge permutation (24) | cp x slice, ep x slice |

Search is branch-and-bound with a full-depth phase 1 so it cannot wander past
its goal, and an iterative phase-2 budget seeded from the heuristic's lower
bound. All move and pruning tables are derived from the geometric move rule, so
the coordinates can never disagree with the moves.

See [TUTORIAL.md](./TUTORIAL.md) for a guided walkthrough, including a browser
Web Worker example.

## Project layout

```
main.go                     the solver (js/wasm build)
go.mod
build.mjs                   compiles to dist/solver.wasm
examples/
  node-selfcheck.mjs        run the self-test in Node
  browser/                  a minimal in-page demo (HTML + Worker)
.github/workflows/ci.yml    builds the wasm and runs the self-test
```

## Using it in a Web Worker

```js
self.importScripts("/dist/wasm_exec.js");

const go = new globalThis.Go();
const bytes = await (await fetch("/dist/solver.wasm")).arrayBuffer();
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
go.run(instance);

while (!globalThis.cubeSolver) await new Promise((r) => setTimeout(r, 25));
globalThis.cubeSolver.init();

const { solution, error } = globalThis.cubeSolver.solve(facelets, 22, 15000);
```

Run the bundled demo from the repository root so `/dist/...` resolves:

```bash
npx serve .          # then open http://localhost:3000/examples/browser/
```

## Browser support

WebAssembly and Web Workers are supported in every current browser. No
WebGL, camera or Bluetooth is involved.

## Performance notes

- Table construction dominates startup: roughly 0.6 to 1 second in wasm. Build
  it once per worker and keep it alive.
- Solutions average 19-20 moves. `maxDepth` defaults to 22 and `timeoutMs`
  defaults to your browser's patience; pass both when you want tighter control.

## License

MIT. See [LICENSE](./LICENSE).
