# Demo capture pipeline

Records the real, deployed product and assembles the footage into a narrated
video. Two scripts:

- `record.mjs` — screencasts a live URL over the Chrome DevTools Protocol and
  saves the frames Chrome actually painted.
- `assemble.py` — times those frames against a narration track and muxes the
  scenes into one MP4 plus subtitles.

Nothing here draws a user interface. Every frame in the output came from a
browser pointed at a real URL. That is the point: these videos are used as
evidence, and a rendered mock of a page is not evidence of anything.

```
node record.mjs scene.json             # once per scene
python3 assemble.py scenes.json        # once for the whole video
python3 assemble.py scenes.json --silent   # a preview, before the narration exists
```

`--silent` builds the cut with silence in place of narration, writing
`<output>.SILENT.mp4`. It exists so a preview goes through this pipeline rather
than an ad-hoc ffmpeg line: the per-scene mp4s are written either way, so
`verify-final.py` can confirm the 1.00 guarantee on the preview instead of
waiting for the audio. Every shot runs at 1.0 in silent mode, including tour
shots, because there is no narration to scale them against — so a silent preview
is longer than the final cut will be. The `.SILENT` suffix is applied by the
assembler, not by the author, so a preview cannot be handed over as the cut.

## The two rules

### 1. Evidence shots run at 1.0

By default `assemble.py` time-scales a scene's footage so it exactly fills its
narration. That is fine for a product tour and wrong for evidence, where *how
long something took* is part of what the shot claims — a guard that fires in
200ms, a confirmation that lands in one block, a run that fails before
broadcasting.

Set `"evidence": true` in the scene's capture config. The footage then plays at
exactly 1.0 and **the narration is cut to fit the footage, never the reverse.**
If the narration is too long, the build fails and tells you by how much.

The flag is written by `record.mjs` into `index.json` next to the frames, and
`assemble.py` reads it from there rather than from the assembly config. A later
edit to `scenes.json` therefore cannot quietly un-flag a shot — the footage
carries its own status.

An evidence shot is never time-shifted, labelled, animated or composited on.
`assemble.py` refuses `speed_label`, `entry` or `designed` on one. If a wait is
too long to watch, cut away to a designed card and back rather than speeding the
footage up. `"speed_label": true` survives for tour footage only, and its number
is computed from the speed actually applied — a scene cannot claim `8x` while
running at `6.5x`.

### Designed cards, tour plates and the ground

- `"designed": true` marks a title or section card: HTML/CSS recorded with
  `record.mjs` (`"evidence": false`). It plays at 1.0 and is padded to its
  narration **only by holding its final frame** — never retimed, so its easing is
  never stretched, and never shortened.
- `"entry": "plate"` on a tour capture slides and fades the whole, unmodified
  frame in over the ground (300 ms, 48 px) and out again. Nothing is drawn on it.
- Captured and designed frames never share a frame. Scenes hard-cut into one
  another; all motion lives inside the designed scenes.
- The letterbox and the plate's ground is one flat colour, `#1a1512` — the
  landing page's warm dark ground. Flat on purpose: the edge of an evidence frame
  has to be unambiguous.

### 2. Moves may scroll, outline and focus — never change a value

A scene can carry `moves`: JavaScript run at scheduled offsets so the page
scrolls to the right row or highlights the right cell in time with the
narration. Those moves go through `Runtime.evaluate`, which means a move *could*
rewrite a balance, a transaction hash or a test count — and the recording would
be indistinguishable from an honest one. Same real page, same real Chrome, same
real frames, one false number.

`record.mjs` rejects a move that assigns to `textContent`, `innerHTML`,
`innerText` or `value`, or that calls `insertAdjacentHTML`.

**This check is a tripwire, not a sandbox.** Anyone who wants to get around it
can do so in seconds with a computed property name. It is there so nobody
crosses the line without noticing, and so the rule has somewhere to live in the
code. The real guarantee is review: the scene configs are committed, so what was
run against the page is readable by anyone who reads the diff.

## Basescan cannot be captured this way

Basescan (and `sepolia.basescan.org`) puts Cloudflare in front of automated
browsers. A navigation from Playwright driving the *real* Chrome binary returned
the "Performing security verification" interstitial rather than the page
(Ray ID `a3ca5c2a0ba817b7`). `chrome-headless-shell`, which is what `record.mjs`
drives, has a stronger bot signature and will not do better.

So any block-explorer shot has to be captured by a person, in an ordinary
browser, and handed to `assemble.py` as a finished clip. That is still real
footage; it is just not footage this pipeline can produce. GitHub has no such
restriction and records fine.

There is a second reason to shoot explorer pages manually: CDP screencast
captures the viewport only, with no browser chrome. Where the URL is itself the
evidence, you want the address bar in frame, and that means a screen recording.

To fold a hand-captured clip in, give the scene a `clip` instead of `frames`:

```jsonc
{ "key": "s1", "clip": "s1a-doublepay.mov", "narration": "s1.txt" }
```

**A clip defaults to evidence.** A frames capture carries its own flag in the
metadata beside the frames; a `.mov` has nowhere to carry one, so the default
fails closed — you must write `"evidence": false` out loud to let a hand-recorded
clip be time-warped.

**HDR is handled, because it has to be.** macOS screen recording on an HDR
display writes PQ / BT.2020 10-bit. Dropped into an SDR timeline it renders
washed out and grey — a white page comes out around half the brightness it should
be, sitting next to `record.mjs` frames that are already correct. It reads as a
bad recording rather than a colour-space mismatch, which is why it is worth
catching automatically. This ffmpeg has neither `libzimg` nor `libplacebo` and
cannot linearise PQ (`colorspace` refuses `smpte2084` outright), so the
conversion goes to macOS `avconvert`, once per clip, cached as
`<name>.sdr.mov`.

## Scene config

```jsonc
{
  "url": "https://github.com/…",      // the real page, always
  "dir": "./frames-s1",               // frames + index.json land here
  "evidence": true,                   // locks this shot to 1.0 at assembly
  "duration": 13000,                  // recording window, ms
  "wait": 8000,                       // settle time after navigation, ms
  "port": 9501,                       // unique per concurrent capture
  "theme": "dark",                    // prefers-color-scheme
  "moves": [
    { "at": 800,  "js": "…scrollIntoView…" },
    { "at": 4500, "js": "…style.outline…" }
  ]
}
```

And the assembly side:

```jsonc
{
  "output": "bounty.mp4",
  "scenes": [
    { "key": "s1", "frames": "frames-s1", "narration": "s1.txt" },
    { "key": "s2", "frames": "frames-s2", "window": [4.0, 72.0] },
    { "key": "s3", "frames": "frames-s3", "speed_label": true }
  ]
}
```

## Pages behind a login

Some shots live inside the app's admin screens. A scene can seed `localStorage`
before the page loads:

```jsonc
{
  "storage":        { "dashboardTab": "grainhack" },
  "storageFromEnv": { "patchwork_jwt": "GRAINLIFY_ADMIN_JWT" }
}
```

`storage` takes plain values. `storageFromEnv` maps a key to the **name** of an
environment variable holding the value, so a session token is read at capture
time and never written into a scene config — configs are committed precisely so
anyone can audit what was run against the page, and a token in a committed file
defeats that twice over. A missing variable fails the capture with a clear
message, and the seeding expression is never echoed, not even in an error, since
a token in a terminal transcript is a token in a screen recording.

```
GRAINLIFY_ADMIN_JWT='…' node record.mjs scene.json
```

**Review the frames.** A signed-in screen can show anything that screen shows.

## Narration

Narration is a plain audio file named after the scene key — `s1.wav`, `s1.aiff`,
`s1.m4a` and `s1.mp3` are all picked up automatically. Record it however you
like; swapping the file is all it takes, because the assembler reads the audio's
real duration and times the visuals to it rather than to an estimate.

`s1.txt` holds the same words as text and is used to generate subtitles. Caption
timing is apportioned by sentence length, which is an approximation — good
enough to read along with, not a real forced alignment.

## Checking a finished cut

```
python3 verify-final.py bounty.mp4
```

Five things before a cut is called done: every evidence shot at 1.0, every
evidence shot unretouched, every designed card held and never cut short, no frame
carrying a credential or an IP, and every on-screen value present in `SOURCES.md`.

**Unretouched** is checked against the capture each evidence scene was built
from. The source is put through the assembler's own scale-and-letterbox chain,
so the only remaining difference is the encoder's, and then compared every
0.5 s — not as a whole frame, whose average hides a small edit, but on an 8×8
grid of tiles. A tile passes if it matches the source at some instant within
0.1 s (so a moving cursor or a scroll is matched frame to frame; screen
recordings are variable-frame-rate, so the candidates are the source's real
frames, including the one still on screen). The letterbox must be a flat field.
Calibrated on the four bounty scenes: the worst clean tile is 35.3 dB; the floor
is 33 dB. Planted on copies, a speed badge scored 12.1 dB, a small "5 USDC" in
the page's own text colour 23.7 dB (its whole frame still 38.7 dB), and a
400×10 bar in the letterbox left every tile passing but failed the letterbox.
**Its limit:** text drawn in almost the colour of the ground under it can pass.

The script does **not** decide the last two, and says so: there is no OCR here,
and a script that claimed to have read every frame without one would be the same
unearned assurance the rest of this pipeline avoids. Instead it finds every
visually distinct screen in the cut and writes each out at full resolution — a
short set, since a recording of a mostly-static page holds still for most of its
length — and a person reads them.

## Gotchas

- **Recording zero frames is an error, not an empty result.** Chrome emits a
  frame only when it paints, so a scene whose selectors match nothing records
  nothing. `record.mjs` fails loudly rather than writing an empty index that
  would show up later as a missing shot.
- **A still page is fine.** Shot length comes from the recording window, not
  from the gap between the first and last painted frame, so holding on a static
  page keeps those seconds.
- **`ffmpeg` here has no `drawtext`.** The speed badge is rendered with Pillow
  and composited with `overlay`.
- **Give each concurrent capture its own `port`.**
- Chromium comes from the Playwright cache; `npx playwright install chromium`
  if it is missing, or point `CHROME_HEADLESS_SHELL` at your own build.
