// Capture a scene by screencasting the real, deployed page over the Chrome
// DevTools Protocol. Usage: node record.mjs <scene.json>
//
// This is not a screen recorder. It drives a headless Chromium at a live URL and
// saves the frames Chrome actually painted, so the footage is the real site
// rather than a reconstruction of it. Read README.md before adding a scene.

import { spawn } from "node:child_process";
import { readFileSync, writeFileSync, mkdirSync, rmSync, readdirSync } from "node:fs";
import { join } from "node:path";

// ---------------------------------------------------------------------------
// Rule: a move may scroll, outline and focus. It may never change a value the
// viewer can read.
//
// Moves are arbitrary JS handed to Runtime.evaluate, so a move could rewrite a
// balance, a transaction hash or a test count, and the recording would be
// indistinguishable from an honest one — same real page, same real Chrome, same
// frames. That is fabrication, and it is the single thing this pipeline must not
// be able to produce by accident. The check below is a tripwire for the obvious
// spellings, not a sandbox: a determined author can evade it in seconds with a
// computed property name. It exists so that nobody crosses the line without
// noticing, and so the rule has a place in the code where it can be read.
// ---------------------------------------------------------------------------
const MUTATORS = [
  // .textContent = / .innerHTML += / ... but not == or ===
  /\.(textContent|innerHTML|innerText|value)\s*(?:\+|-|\*|\/|\*\*|\|\||&&|\?\?)?=(?!=)/,
  // ["innerHTML"] = and friends
  /\[\s*(["'`])(textContent|innerHTML|innerText|value)\1\s*\]\s*(?:\+)?=(?!=)/,
  // insertAdjacentHTML writes displayed content just as directly
  /\.insertAdjacentHTML\s*\(/,
];

function assertPresentational(js, where) {
  for (const re of MUTATORS) {
    const m = js.match(re);
    if (m) {
      throw new Error(
        `${where}: a move may not assign to textContent, innerHTML, innerText or value.\n` +
          `  matched: ${m[0]}\n` +
          `  in: ${js.slice(0, 160)}${js.length > 160 ? "…" : ""}\n` +
          `Moves may scroll, outline and focus. Changing a displayed value would make\n` +
          `fabricated footage that looks exactly like real capture. See README.md.`
      );
    }
  }
}

// ---------------------------------------------------------------------------

function resolveShell() {
  if (process.env.CHROME_HEADLESS_SHELL) return process.env.CHROME_HEADLESS_SHELL;
  const cache = join(process.env.HOME, "Library/Caches/ms-playwright");
  const builds = readdirSync(cache)
    .filter((d) => d.startsWith("chromium_headless_shell-"))
    .sort((a, b) => Number(b.split("-")[1]) - Number(a.split("-")[1]));
  if (!builds.length) {
    throw new Error(
      "no chromium_headless_shell in ~/Library/Caches/ms-playwright.\n" +
        "Run: npx playwright install chromium — or set CHROME_HEADLESS_SHELL."
    );
  }
  return join(cache, builds[0], "chrome-headless-shell-mac-arm64", "chrome-headless-shell");
}

const cfg = JSON.parse(readFileSync(process.argv[2], "utf8"));
for (const [i, m] of (cfg.moves ?? []).entries()) {
  assertPresentational(m.js, `${process.argv[2]} moves[${i}] (at ${m.at}ms)`);
}
if (cfg.setup) assertPresentational(cfg.setup, `${process.argv[2]} setup`);

const SHELL = resolveShell();
const PORT = cfg.port ?? 9490;
const WIDTH = cfg.width ?? 1280;
const HEIGHT = cfg.height ?? 720;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

rmSync(cfg.dir, { recursive: true, force: true });
mkdirSync(cfg.dir, { recursive: true });

const chrome = spawn(
  SHELL,
  [`--remote-debugging-port=${PORT}`, "--headless", "--hide-scrollbars",
   `--window-size=${WIDTH},${HEIGHT}`, "about:blank"],
  { stdio: "ignore" }
);

let version;
for (let i = 0; i < 60; i++) {
  try {
    version = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
    break;
  } catch { await sleep(250); }
}
if (!version) { chrome.kill(); throw new Error("chrome-headless-shell did not start"); }

const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((r) => (ws.onopen = r));

let id = 0, sess = null, n = 0, recording = false;
const pending = new Map();
const frames = [];

ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.method === "Page.screencastFrame") {
    if (recording) {
      const f = `f${String(n).padStart(5, "0")}.jpg`;
      writeFileSync(join(cfg.dir, f), Buffer.from(m.params.data, "base64"));
      frames.push([f, m.params.metadata.timestamp]);
      n++;
    }
    ws.send(JSON.stringify({
      id: ++id, method: "Page.screencastFrameAck",
      params: { sessionId: m.params.sessionId }, sessionId: sess,
    }));
  }
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
};

const send = (method, params = {}, sessionId) =>
  new Promise((r) => {
    const i = ++id;
    pending.set(i, r);
    ws.send(JSON.stringify({ id: i, method, params, ...(sessionId ? { sessionId } : {}) }));
  });
// A move that throws must not pass quietly. Runtime.evaluate reports the throw
// in the result rather than rejecting, so an unchecked move leaves the page
// exactly as it was and the take looks merely uneventful — the failure shows up
// as a shot that does not do what the narration says it does.
async function evaluate(expression, where = "move") {
  const res = await send("Runtime.evaluate", { expression, awaitPromise: true }, sess);
  const ex = res.result?.exceptionDetails;
  if (ex) {
    throw new Error(
      `${where} threw: ${ex.exception?.description ?? ex.text}\n  in: ${expression.slice(0, 200)}`
    );
  }
  return res;
}

const target = await send("Target.createTarget", { url: "about:blank", width: WIDTH, height: HEIGHT });
sess = (await send("Target.attachToTarget", { targetId: target.result.targetId, flatten: true })).result.sessionId;

await send("Page.enable", {}, sess);
await send("Emulation.setDeviceMetricsOverride",
  { width: WIDTH, height: HEIGHT, deviceScaleFactor: cfg.scale ?? 1.5, mobile: false }, sess);
await send("Emulation.setEmulatedMedia",
  { features: [{ name: "prefers-color-scheme", value: cfg.theme ?? "dark" }] }, sess);

const navigatedAt = new Date().toISOString();
await send("Page.navigate", { url: cfg.url }, sess);
await sleep(cfg.wait ?? 7000);
if (cfg.setup) await evaluate(cfg.setup, "setup");
await sleep(600);

await send("Page.startScreencast",
  { format: "jpeg", quality: cfg.quality ?? 88, maxWidth: 1920, maxHeight: 1080, everyNthFrame: 1 }, sess);
await sleep(300);
recording = true;
const t0 = Date.now();

// CDP emits a screencast frame only when the compositor paints. A page that has
// finished loading is perfectly still, so a scene whose moves do not actually
// move anything would otherwise record zero frames. Nudge one pixel and back to
// guarantee a first frame; it is a scroll, it is reversible, and it leaves the
// page showing exactly what it showed before.
await evaluate("window.scrollBy(0,1);window.scrollBy(0,-1)");

for (const m of cfg.moves ?? []) {
  const wait = m.at - (Date.now() - t0);
  if (wait > 0) await sleep(wait);
  await evaluate(m.js, `move at ${m.at}ms`);
}
const rest = cfg.duration - (Date.now() - t0);
if (rest > 0) await sleep(rest);

recording = false;
await send("Page.stopScreencast", {}, sess);

// A capture that recorded nothing is a failed capture, not an empty one. Writing
// an index.json with zero frames and exiting 0 would hand the assembler a scene
// that silently contributes nothing, and the missing shot would surface as a gap
// in the finished video rather than as an error here.
if (n === 0) {
  ws.close(); chrome.kill();
  throw new Error(
    `${cfg.dir}: recorded 0 frames from ${cfg.url}.\n` +
      `The page painted nothing during the capture window. Usually a move's selector\n` +
      `matched no element, so nothing scrolled or highlighted. Check the selectors\n` +
      `against the live page before re-running.`
  );
}

// The shot is as long as the recording window, not as long as the gap between
// the first and last painted frame. Chrome emits a frame only when something
// changes, so a page held still at the end of a take emits nothing for those
// seconds — they are real seconds in which the page really looked like that, and
// measuring first-frame-to-last-frame would silently truncate them.
const wallClock = (Date.now() - t0) / 1000;
writeFileSync(join(cfg.dir, "index.json"), JSON.stringify({
  meta: {
    url: cfg.url,
    evidence: cfg.evidence === true,
    capturedAt: navigatedAt,
    browser: version.Browser,
    viewport: { width: WIDTH, height: HEIGHT, scale: cfg.scale ?? 1.5 },
    frameCount: n,
    startedAtEpoch: t0 / 1000,
    wallClockSeconds: Number(wallClock.toFixed(3)),
  },
  frames,
}, null, 2));

console.log(`${cfg.dir.split("/").pop()}  frames ${n}  recorded ${wallClock.toFixed(1)}s` +
  (cfg.evidence === true ? "  [evidence: will not be time-warped]" : ""));

ws.close();
chrome.kill();
process.exit(0);
