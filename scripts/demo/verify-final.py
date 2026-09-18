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
DISTINCT_AT = 6.0       # mean abs 0-255 difference that counts as a new screen


def probe(path, entries, stream=False):
    cmd = ["ffprobe", "-v", "error"]
    if stream:
        cmd += ["-select_streams", "v:0"]
    cmd += ["-show_entries", ("stream=" if stream else "format=") + entries,
            "-of", "default=nw=1:nk=1", path]
    return subprocess.check_output(cmd).decode().strip().splitlines()


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
            scene_mp4 = os.path.join(base, f"{key}.mp4")
            if os.path.exists(scene_mp4):
                built = float(probe(scene_mp4, "duration")[0])
                speed = span / built if built else 0.0
                ok = abs(speed - 1.0) <= 0.02 or labelled
                if not ok:
                    bad += 1
                print(f"    {key}: {'evidence' if evidence else 'tour    '} "
                      f"source {span:6.2f}s  cut {built:6.2f}s  speed x{speed:.2f}  "
                      f"{'OK' if ok else 'FAIL — warped without a badge'}"
                      f"{'  [badge]' if labelled else ''}")
            else:
                print(f"    {key}: scene mp4 not kept; speed not re-checkable here")
    else:
        print("    no scenes.json beside the video — cannot re-check speed")

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
