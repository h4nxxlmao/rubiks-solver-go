/* Minimal solver Web Worker.
 *
 * Serve the repository root so /dist/wasm_exec.js and /dist/solver.wasm resolve.
 */
self.importScripts("/dist/wasm_exec.js");

let ready = false;

async function boot() {
  if (ready) return;

  globalThis.__solverProgress = (value) =>
    self.postMessage({ type: "progress", value });

  const go = new globalThis.Go();
  const bytes = await (await fetch("/dist/solver.wasm")).arrayBuffer();
  const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
  go.run(instance);

  const started = Date.now();
  while (!globalThis.cubeSolver) {
    if (Date.now() - started > 60000) throw new Error("solver did not start");
    await new Promise((resolve) => setTimeout(resolve, 25));
  }

  globalThis.cubeSolver.init();
  ready = true;
}

self.onmessage = async (event) => {
  const { id, facelets, maxDepth = 22, timeoutMs = 15000 } = event.data ?? {};
  try {
    await boot();
    const result = globalThis.cubeSolver.solve(facelets, maxDepth, timeoutMs);
    self.postMessage({ type: "result", id, ...result });
  } catch (error) {
    self.postMessage({ type: "result", id, solution: "", error: String(error) });
  }
};
