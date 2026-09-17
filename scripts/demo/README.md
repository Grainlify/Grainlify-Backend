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
node record.mjs scene.json          # once per scene
python3 assemble.py scenes.json     # once for the whole video
```

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

If an evidence shot genuinely must be time-shifted to stay watchable, set
`"speed_label": true` on the scene. A badge is burned into the frame. The badge
is a boolean opt-in rather than a caption you write, because the number on it is
computed from the speed actually applied — a scene cannot claim `8x` while
running at `6.5x`. A viewer should never have to take our word for the timing of
a clip we are using as proof.

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

To fold a hand-captured clip in, drop the file in as a scene's video and give it
the same narration treatment as any other scene, or concatenate it at the
`vlist.txt` stage.

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

## Narration

Narration is a plain audio file named after the scene key — `s1.wav`, `s1.aiff`,
`s1.m4a` and `s1.mp3` are all picked up automatically. Record it however you
like; swapping the file is all it takes, because the assembler reads the audio's
real duration and times the visuals to it rather than to an estimate.

`s1.txt` holds the same words as text and is used to generate subtitles. Caption
timing is apportioned by sentence length, which is an approximation — good
enough to read along with, not a real forced alignment.

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
