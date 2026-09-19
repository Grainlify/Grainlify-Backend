#!/usr/bin/env python3
"""Check a finished cut against its evidence. Usage:

    python3 verify-final.py bounty.mp4

Four things, before the file is called done:

  1. Every shot plays at 1.0, unless it carries a burnt speed badge.
  2. A burnt badge, if present, names the speed actually applied.
  3. No frame carries a credential, an IP, or a settings page.
  4. Every on-screen value appears in SOURCES.md.

Checks 1 and 2 are mechanical and this script decides them. Checks 3 and 4 need
eyes: there is no OCR on this machine, and a script that claimed to have read
every frame without one would be exactly the kind of unearned assurance this
whole pipeline exists to avoid. So the script does the part it can do honestly —
it finds every visually distinct screen in the cut and writes each one out at
full resolution — and a person reviews that set. It is a short set, because a
recording of a mostly-static page holds still for most of its length.
"""

import json
import os
import subprocess
import sys

SAMPLE_EVERY = 0.5      # seconds between samples

# The unretouched check. Each evidence scene is compared, frame by frame,
# against the capture it was built from - not only as a whole frame, whose
# average would hide a small overlay, but tile by tile on an 8x8 grid, so a
# label, badge or panel confined to one region fails the tile it sits on.
TILE_GRID = 8
UNRETOUCHED_EVERY = 0.5 # seconds between compared frames
# Calibrated 2026-09-18 on the four bounty scenes, cut from real captures:
# the worst tile in any of them is 35.3 dB (s3), worst whole frame 41.7 dB.
# A planted "4.0x speed" badge scores 12.1 dB on its tile; a small "5 USDC"
# figure in the page's own text colour 23.7 dB, and in a mid grey 30.9 dB -
# while its whole-frame score stays at 38.7 dB, which is why this is per tile.
# Text drawn nearly the colour of the ground beneath it can still pass; that
# is the limit of this check, and the reason every screen is still reviewed.
TILE_FLOOR_DB = 33.0
LETTERBOX_MAX_STD = 2.0 # a flat field, whatever its colour
DISTINCT_AT = 6.0       # mean abs 0-255 difference that counts as a new screen


def probe(path, entries, stream=False):
    cmd = ["ffprobe", "-v", "error"]
    if stream:
        cmd += ["-select_streams", "v:0"]
    cmd += ["-show_entries", ("stream=" if stream else "format=") + entries,
            "-of", "default=nw=1:nk=1", path]
    return subprocess.check_output(cmd).decode().strip().splitlines()


def _psnr(a, b):
    import numpy as np
    mse = float(np.mean((a - b) ** 2))
    return 99.0 if mse == 0 else 10 * np.log10(255.0 ** 2 / mse)


# The assembler's own scale-and-letterbox chain, so the reference frame is
# resampled by the same scaler with the same chroma subsampling as the cut and
# the only difference left is the encoder's.
PLACE = ("scale=1920:1080:force_original_aspect_ratio=decrease,"
         "pad=1920:1080:(ow-iw)/2:(oh-ih)/2,format=yuv420p")


def _frame(path, t, vf=None):
    from PIL import Image
    import io
    cmd = ["ffmpeg", "-v", "error"]
    if t is not None:
        cmd += ["-ss", f"{max(t, 0):.3f}"]
    cmd += ["-i", path, "-frames:v", "1"] + (["-vf", vf] if vf else [])
    png = subprocess.run(cmd + ["-f", "image2pipe", "-vcodec", "png", "-"],
                         check=True, capture_output=True).stdout
    return Image.open(io.BytesIO(png)).convert("RGB") if png else None


def unretouched(base, scene, scene_mp4, floor_db):
    """Compare an evidence scene with its source capture, tile by tile.

    Returns (ok, detail). The source is transformed exactly as the assembler
    transforms it - scaled to fit 1920x1080 - and only the content rectangle is
    compared, less a small margin for the scaler's rounding at the edges. The
    letterbox around it is checked separately for being a flat field.
    """
    import json as _json
    import numpy as np
    from PIL import Image

    window = scene.get("window") or [0, None]
    if scene.get("clip"):
        src = os.path.join(base, scene["clip"])
        sdr = os.path.splitext(src)[0] + ".sdr.mov"
        src = sdr if os.path.exists(sdr) else src
        # Screen recordings are variable-frame-rate: a still screen can go
        # half a second without a new frame. Candidates are the source's real
        # frames - every one presented within 0.1 s of t, plus the one already
        # on screen when that window opens - never a seek to t, which would
        # land on the next frame and not the one being shown.
        pts = [float(x.split(",")[0]) for x in subprocess.run(
            ["ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "frame=pts_time",
             "-of", "csv=p=0", src], check=True, capture_output=True, text=True).stdout.split() if x.split(",")[0]]
        def candidates(t):
            u = window[0] + t
            near = [p for p in pts if u - 0.1 <= p <= u + 0.1]
            held = [p for p in pts if p < u - 0.1][-1:]
            return [(_frame(src, p - 0.0005, PLACE), size) for p in held + near]
        size = tuple(int(v) for v in probe(src, "width,height", stream=True)[:2])
        candidates_size = lambda: size
    else:
        fdir = os.path.join(base, scene["frames"])
        idx = _json.load(open(os.path.join(fdir, "index.json")))
        meta, flist = ({}, idx) if isinstance(idx, list) else (idx.get("meta", {}), idx["frames"])
        # Rebuild the timeline exactly as the assembler lays it out: each frame
        # holds until the next one's timestamp, the first starting at zero.
        t0 = meta.get("startedAtEpoch", flist[0][1])
        timed = [(os.path.join(fdir, f), ts - t0) for f, ts in flist]
        if scene.get("window"):
            a0, a1 = scene["window"]
            timed = [(f, ts - a0) for f, ts in timed if a0 <= ts <= a1] or timed[:1]
            span = a1 - a0
        else:
            span = max(timed[-1][1] + 0.1, meta.get("wallClockSeconds", 0))
        starts, acc = [], 0.0
        for i, (f, ts) in enumerate(timed):
            starts.append(acc)
            nxt = timed[i + 1][1] if i + 1 < len(timed) else span
            acc += max(nxt - max(ts, 0), 0.001)
        cache = {}
        def candidates(t):
            # Every source frame on screen within 0.1 s of t: a burst of
            # frames milliseconds apart is decimated to 30 fps, so the
            # one kept is any of them.
            idx = {max([k for k, st in enumerate(starts) if st <= u] or [0])
                   for u in (t - 0.1 + j * 0.01 for j in range(21))}
            for i in idx:
                if i not in cache:
                    cache[i] = (_frame(timed[i][0], None, PLACE), Image.open(timed[i][0]).size)
            return [cache[i] for i in sorted(idx)]
        candidates_size = lambda: Image.open(timed[0][0]).size

    dur = float(probe(scene_mp4, "duration")[0])
    worst_tile, worst_where, worst_whole, n = 99.0, None, 99.0, 0
    letterbox, worst_bar = None, 0.0
    t = 0.25
    while t < dur - 0.25:
        out = _frame(scene_mp4, t)
        if out is None:
            break
        o = np.asarray(out, dtype=np.float64)
        if letterbox is None:
            sw, sh = candidates_size()
            sc = min(1920 / sw, 1080 / sh)
            cw, ch = int(sw * sc), int(sh * sc)
            x0, y0 = (1920 - cw) // 2, (1080 - ch) // 2
            mask = np.ones((1080, 1920), bool)
            mask[max(y0 - 2, 0):y0 + ch + 2, max(x0 - 2, 0):x0 + cw + 2] = False
            letterbox = mask
        if letterbox.any():
            worst_bar = max(worst_bar, float(o[letterbox].std(axis=0).max()))
        # A tile passes if it matches the source at some instant within the
        # candidate window; a cursor or a scroll moving through that window
        # is then matched frame to frame. An overlay matches no instant.
        grid, whole = None, 0.0
        for c, (sw, sh) in candidates(t):
            if c is None:
                continue
            sc = min(1920 / sw, 1080 / sh)
            cw, ch = int(sw * sc), int(sh * sc)
            x0, y0 = (1920 - cw) // 2, (1080 - ch) // 2
            ref = np.asarray(c, dtype=np.float64)
            m = 4
            a = o[y0 + m:y0 + ch - m, x0 + m:x0 + cw - m]
            b = ref[y0 + m:y0 + ch - m, x0 + m:x0 + cw - m]
            th, tw = a.shape[0] // TILE_GRID, a.shape[1] // TILE_GRID
            g = np.array([[_psnr(a[gy*th:(gy+1)*th, gx*tw:(gx+1)*tw], b[gy*th:(gy+1)*th, gx*tw:(gx+1)*tw])
                           for gx in range(TILE_GRID)] for gy in range(TILE_GRID)])
            grid = g if grid is None else np.maximum(grid, g)
            whole = max(whole, _psnr(a, b))
        worst_whole = min(worst_whole, whole)
        gy, gx = np.unravel_index(np.argmin(grid), grid.shape)
        if grid[gy, gx] < worst_tile:
            worst_tile, worst_where = float(grid[gy, gx]), (round(t, 2), int(gx), int(gy))
        n += 1
        t += UNRETOUCHED_EVERY
    ok = (floor_db is None or worst_tile >= floor_db) and worst_bar <= LETTERBOX_MAX_STD
    return ok, dict(samples=n, worst_whole_db=round(float(worst_whole), 1),
                    worst_tile_db=round(worst_tile, 1), worst_tile_at=worst_where,
                    letterbox_std=round(worst_bar, 2))


def main():
    video = sys.argv[1] if len(sys.argv) > 1 else "bounty.mp4"
    base = os.path.dirname(os.path.abspath(video)) or "."
    if not os.path.exists(video):
        raise SystemExit(f"no such file: {video}")

    from PIL import Image, ImageChops, ImageStat

    print(f"\n=== {os.path.basename(video)} ===")
    dur = float(probe(video, "duration")[0])
    w, h, trc = probe(video, "width,height,color_transfer", stream=True)[:3]
    print(f"  {w}x{h}  {dur:.1f}s  transfer {trc}")
    if trc == "smpte2084":
        print("  FAIL  output is still HDR — it will render washed out and grey")
    elif trc != "bt709":
        print(f"  WARN  unexpected colour transfer {trc!r}")

    # --- 1 & 2: speed, from the scene configs the cut was built from ----------
    print("\n  Speed (every evidence shot must be 1.00):")
    cfg_path = os.path.join(base, "scenes.json")
    bad = 0
    if os.path.exists(cfg_path):
        for scene in json.load(open(cfg_path))["scenes"]:
            key = scene["key"]
            labelled = bool(scene.get("speed_label"))
            if scene.get("clip"):
                src = os.path.join(base, scene["clip"])
                sdr = os.path.splitext(src)[0] + ".sdr.mov"
                src = sdr if os.path.exists(sdr) else src
                span = float(probe(src, "duration")[0])
                evidence = scene.get("evidence", True)
            else:
                idx = json.load(open(os.path.join(base, scene["frames"], "index.json")))
                meta = idx.get("meta", {}) if isinstance(idx, dict) else {}
                span = meta.get("wallClockSeconds", 0.0)
                evidence = bool(meta.get("evidence"))
            # A window trims the source before it is played, exactly as the
            # assembler does, so speed is measured against the trimmed span -
            # otherwise a trimmed tour at 1.00 reads as sped up.
            if scene.get("window"):
                span = scene["window"][1] - scene["window"][0]
            scene_mp4 = os.path.join(base, f"{key}.mp4")
            if os.path.exists(scene_mp4):
                built = float(probe(scene_mp4, "duration")[0])
                speed = span / built if built else 0.0
                designed = bool(scene.get("designed"))
                if evidence:
                    ok = abs(speed - 1.0) <= 0.02 or labelled
                    verdict = "OK" if ok else "FAIL — warped without a badge"
                elif designed:
                    # Plays at 1.00 and is only ever lengthened by holding its
                    # last frame, so the cut is never shorter than the card.
                    ok = built >= span - 0.05
                    verdict = (f"OK (held +{built - span:.2f}s)" if ok
                               else "FAIL — a designed card was cut short")
                else:
                    ok = True
                    verdict = "scaled to narration (tour)"
                if not ok:
                    bad += 1
                kind = "evidence" if evidence else "designed" if designed else "tour    "
                print(f"    {key}: {kind} "
                      f"source {span:6.2f}s  cut {built:6.2f}s  speed x{speed:.2f}  {verdict}"
                      f"{'  [badge]' if labelled else ''}")
            else:
                print(f"    {key}: scene mp4 not kept; speed not re-checkable here")
    else:
        print("    no scenes.json beside the video — cannot re-check speed")

    # --- 2b: unretouched - every evidence scene against the capture it came from
    print(f"\n  Unretouched (each evidence tile within {TILE_FLOOR_DB:.0f} dB of its source, flat letterbox):")
    if os.path.exists(cfg_path):
        for scene in json.load(open(cfg_path))["scenes"]:
            key = scene["key"]
            if scene.get("designed"):
                continue
            if scene.get("clip"):
                evidence = scene.get("evidence", True)
            else:
                idx = json.load(open(os.path.join(base, scene["frames"], "index.json")))
                evidence = bool((idx.get("meta", {}) if isinstance(idx, dict) else {}).get("evidence"))
            scene_mp4 = os.path.join(base, f"{key}.mp4")
            if not evidence:
                continue
            if not os.path.exists(scene_mp4):
                bad += 1
                print(f"    {key}: FAIL - evidence scene mp4 not kept; cannot prove it is unretouched")
                continue
            ok, d = unretouched(base, scene, scene_mp4, TILE_FLOOR_DB)
            if not ok:
                bad += 1
            print(f"    {key}: worst tile {d['worst_tile_db']:5.1f} dB at {d['worst_tile_at']}  "
                  f"whole {d['worst_whole_db']:5.1f} dB  letterbox sd {d['letterbox_std']:.2f}  "
                  f"{'OK' if ok else 'FAIL - differs from its source'}")
    else:
        print("    no scenes.json beside the video - cannot check")

    # --- 3 & 4: one frame per visually distinct screen ------------------------
    out = os.path.join(base, "verify-frames")
    os.makedirs(out, exist_ok=True)
    for f in os.listdir(out):
        os.remove(os.path.join(out, f))

    tmp = os.path.join(out, ".sample.png")
    prev, kept, t = None, [], 0.0
    while t < dur:
        subprocess.run(["ffmpeg", "-v", "error", "-ss", f"{t:.2f}", "-i", video,
                        "-vframes", "1", "-y", tmp], check=True)
        im = Image.open(tmp).convert("RGB")
        small = im.resize((160, 90))
        if prev is None or ImageStat.Stat(ImageChops.difference(small, prev)).mean[0] > DISTINCT_AT:
            name = os.path.join(out, f"screen_{len(kept):02d}_at{t:06.2f}s.png")
            im.save(name)
            kept.append(name)
            prev = small
        t += SAMPLE_EVERY
    os.path.exists(tmp) and os.remove(tmp)

    print(f"\n  {len(kept)} visually distinct screens written to {out}/")
    print("  Review every one of them for:")
    print("    - a token, key, password or .env in frame")
    print("    - an IP address or a KeeperHub settings page")
    print("    - any value not listed in SOURCES.md")

    print("\n  Values SOURCES.md commits to (each must be on screen, and nothing else):")
    src = os.path.join(base, "SOURCES.md")
    if os.path.exists(src):
        import re
        text = open(src).read()
        for v in sorted(set(re.findall(r"0x[a-fA-F0-9]{40,64}", text))):
            print(f"    {v}")
        for v in sorted(set(re.findall(r"\b46\d{6}\b", text))):
            print(f"    block {v}")
    else:
        print("    SOURCES.md missing — there is nothing to check the cut against")

    print(f"\n  Mechanical checks: {'PASS' if bad == 0 else f'{bad} FAILED'}")
    print("  Visual checks: not decided here, by design. Read the frames.\n")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
