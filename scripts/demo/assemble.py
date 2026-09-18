#!/usr/bin/env python3
"""Assemble captured scenes and narration into one video. Usage:

    python3 assemble.py scenes.json

Each scene is a directory of frames produced by record.mjs, plus a narration
audio file and its text. Read README.md before adding a scene.

The speed-1.0 rule
------------------
By default a scene's footage is time-scaled so it exactly fills its narration.
That is fine for a product tour and wrong for evidence, where how long something
took is part of what the shot claims. A scene captured with "evidence": true is
therefore played at exactly 1.0 and the narration must be cut to fit the
footage, never the other way round.

The flag lives in the capture metadata that record.mjs writes next to the
frames, not in this file's config, so re-editing the assembly cannot quietly
un-flag a shot. If an evidence scene genuinely must be time-shifted to be
watchable — a long confirmation wait, say — the author has to set
"speed_label": true, and a badge is burned into the frame. The badge is a
boolean opt-in rather than a caption, because its number is computed from the
speed actually applied: a scene cannot claim 8x while running at 6.5x. A viewer
should never have to take our word for the timing of a clip we use as proof.
"""

import json
import os
import shutil
import subprocess
import sys

PRE, POST = 0.5, 0.7          # lead-in silence, tail silence
FONT = "/System/Library/Fonts/Supplemental/Arial Bold.ttf"
AUDIO_EXTS = (".wav", ".aiff", ".aif", ".m4a", ".mp3")
TOL = 0.05                     # seconds of slack before we call it a mismatch


def duration(path):
    out = subprocess.check_output(
        ["ffprobe", "-v", "error", "-show_entries", "format=duration",
         "-of", "csv=p=0", path])
    return float(out.strip())


def find_audio(base, key, explicit):
    """Narration is a file swap: drop s1.wav next to the frames and it is used."""
    if explicit:
        return os.path.join(base, explicit)
    for ext in AUDIO_EXTS:
        p = os.path.join(base, key + ext)
        if os.path.exists(p):
            return p
    raise SystemExit(
        f"{key}: no narration audio found. Expected one of "
        f"{', '.join(key + e for e in AUDIO_EXTS)} in {base}")


def speed_label_png(base, key, text):
    """ffmpeg here is built without drawtext, so render the label and overlay it."""
    from PIL import Image, ImageDraw, ImageFont
    font = ImageFont.truetype(FONT, 30)
    pad_x, pad_y = 20, 12
    probe = ImageDraw.Draw(Image.new("RGBA", (1, 1)))
    l, t, r, b = probe.textbbox((0, 0), text, font=font)
    w, h = r - l + pad_x * 2, b - t + pad_y * 2
    img = Image.new("RGBA", (w, h), (10, 13, 20, 220))
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, w - 1, h - 1], outline=(233, 237, 245, 140), width=2)
    d.text((pad_x - l, pad_y - t), text, font=font, fill=(255, 214, 102, 255))
    out = os.path.join(base, f"{key}.label.png")
    img.save(out)
    return out


def sdr_clip(path, key):
    """Tone-map a hand-recorded HDR clip down to SDR, once, and cache it.

    macOS screen recording on an HDR display writes PQ / BT.2020 10-bit. Dropped
    straight into an SDR timeline that renders washed out and grey — a white page
    comes out at about half the brightness it should be, next to SDR frames
    captured by record.mjs that are already correct. It looks like a bad camera
    rather than a colour-space mismatch, which is why it is worth catching here.

    This ffmpeg is built without libzimg and libplacebo, so it cannot linearise
    PQ; `colorspace` refuses smpte2084 outright and a plain format conversion
    leaves the picture dim. macOS ships AVFoundation, which does it properly, so
    the work goes to `avconvert` when a clip is actually HDR. An SDR clip is
    passed through untouched.
    """
    if not os.path.exists(path):
        raise SystemExit(f"{key}: clip not found: {path}")
    trc = subprocess.check_output(
        ["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
         "stream=color_transfer", "-of", "default=nw=1:nk=1", path]).decode().strip()
    # csv=p=0 returns "smpte2084," with a trailing comma, which silently failed an
    # equality test here and let HDR through as if it were SDR. Guard the unknown
    # case too: not being able to read the transfer is not evidence of SDR.
    if not trc or trc == "unknown":
        raise SystemExit(
            f"{key}: cannot read the colour transfer of {os.path.basename(path)}.\n"
            f"Refusing to guess — an HDR clip treated as SDR renders washed out and grey,\n"
            f"and the failure looks like a bad recording rather than a conversion bug.")
    if trc not in ("smpte2084", "arib-std-b67"):
        return path

    out = os.path.splitext(path)[0] + ".sdr.mov"
    if os.path.exists(out) and os.path.getmtime(out) >= os.path.getmtime(path):
        return out
    if not shutil.which("avconvert"):
        raise SystemExit(
            f"{key}: {os.path.basename(path)} is HDR ({trc}) and this ffmpeg cannot tone-map it\n"
            f"(no libzimg, no libplacebo). avconvert is missing too, so there is no way to\n"
            f"convert it here. Re-record with HDR off, or convert the clip on a machine that can.")
    print(f"  {key}: {os.path.basename(path)} is HDR ({trc}) — tone-mapping to SDR once")
    subprocess.run(["avconvert", "--source", path, "--output", out,
                    "--preset", "Preset1920x1080", "--replace"],
                   check=True, stdout=subprocess.DEVNULL)
    return out


def timestamp(t):
    h, m, s = int(t // 3600), int(t % 3600 // 60), t % 60
    return f"{h:02}:{m:02}:{s:06.3f}".replace(".", ",")


def main():
    cfg_path = sys.argv[1] if len(sys.argv) > 1 else "scenes.json"
    cfg = json.load(open(cfg_path))
    base = os.path.dirname(os.path.abspath(cfg_path))
    output = os.path.join(base, cfg.get("output", "demo.mp4"))

    parts_v, parts_a, srt, clock = [], [], [], 0.0

    for scene in cfg["scenes"]:
        key = scene["key"]
        clip = scene.get("clip")

        if clip:
            # A hand-recorded take: the explorer shots Cloudflare blocks, and
            # terminal runs. It is real footage, just not footage this pipeline
            # captured, so it defaults to evidence. A frames capture carries its
            # own flag in its metadata; a .mov has nowhere to carry one, so the
            # default here fails closed — you must say "evidence": false out loud
            # to let a hand-recorded clip be time-warped.
            clip_path = sdr_clip(os.path.join(base, clip), key)
            meta, frame_list, frames_dir = {}, None, None
            is_evidence = scene.get("evidence", True)
        else:
            frames_dir = os.path.join(base, scene["frames"])
            index = json.load(open(os.path.join(frames_dir, "index.json")))

            # Older captures were a bare list; current ones carry meta.
            if isinstance(index, list):
                meta, frame_list = {}, index
            else:
                meta, frame_list = index.get("meta", {}), index["frames"]
            if not frame_list:
                raise SystemExit(f"{key}: no frames in {frames_dir}")
            clip_path = None
            is_evidence = bool(meta.get("evidence"))
        # speed_label is a boolean opt-in, never author-supplied text. The number
        # on the badge is computed from the speed actually applied, so a scene
        # cannot claim "8x" while running at 6.5x — a mislabelled badge would be
        # its own small fabrication, on the very shot we are labelling to be honest
        # about.
        wants_label = bool(scene.get("speed_label"))

        audio_path = find_audio(base, key, scene.get("audio"))
        narration = duration(audio_path)

        window = scene.get("window")

        if clip_path:
            frames = None
            span = duration(clip_path)
            if window:
                start, end = window
                span = end - start
        else:
            # Offsets are measured from when recording started, not from the first
            # painted frame, so seconds in which the page was simply still are kept.
            t0 = meta.get("startedAtEpoch", frame_list[0][1])
            frames = [(os.path.join(frames_dir, f), t - t0) for f, t in frame_list]

            if window:
                start, end = window
                frames = [(f, t - start) for f, t in frames if start <= t <= end] or frames[:1]
                span = end - start
            else:
                span = max(frames[-1][1] + 0.1, meta.get("wallClockSeconds", 0))

        if is_evidence and not wants_label:
            # Footage dictates length. Narration must fit inside it.
            target = span
            speed = 1.0
            needed = PRE + narration + POST
            if needed > span + TOL:
                raise SystemExit(
                    f"{key}: evidence shot, so the footage runs at 1.0 and cannot be "
                    f"stretched to fit the narration.\n"
                    f"  footage    {span:.2f}s\n"
                    f"  narration  {narration:.2f}s (+{PRE}s lead-in +{POST}s tail = {needed:.2f}s)\n"
                    f"  over by    {needed - span:.2f}s\n"
                    f"Cut about {needed - span:.2f}s of narration from {os.path.basename(audio_path)}, "
                    f"or use a longer take. Do not set speed_label to paper over this: "
                    f"that burns a speed badge onto footage that is already real-time.")
        else:
            target = PRE + narration + POST
            speed = span / target

        label = f"{speed:.1f}x speed" if wants_label and abs(speed - 1.0) > 0.01 else None
        if is_evidence and label:
            print(f"  {key}: evidence shot time-shifted, burning {label!r} into the frame")
        elif not is_evidence and abs(speed - 1.0) > 0.01:
            print(f"  {key}: non-evidence shot scaled x{speed:.2f} to fit narration")

        vf = ("scale=1920:1080:force_original_aspect_ratio=decrease,"
              "pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=0x0a0d14,fps=30,format=yuv420p")
        scene_mp4 = os.path.join(base, f"{key}.mp4")

        if clip_path:
            cmd = ["ffmpeg", "-y", "-loglevel", "error"]
            if window:
                cmd += ["-ss", f"{window[0]:.3f}"]
            cmd += ["-i", clip_path]
            if abs(speed - 1.0) > 0.001:
                vf = f"setpts=PTS/{speed:.6f}," + vf
        else:
            # concat demuxer list, per-frame durations from the real timestamps
            lines = []
            for i, (path, t) in enumerate(frames):
                nxt = frames[i + 1][1] if i + 1 < len(frames) else span
                d = max(nxt - max(t, 0), 0.001) / speed
                lines.append(f"file '{path}'\nduration {d:.4f}")
            lines.append(f"file '{frames[-1][0]}'")
            list_path = os.path.join(base, f"{key}.list")
            open(list_path, "w").write("\n".join(lines) + "\n")
            cmd = ["ffmpeg", "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", list_path]
        if label:
            png = speed_label_png(base, key, label)
            cmd += ["-i", png, "-filter_complex",
                    f"[0:v]{vf}[v];[v][1:v]overlay=W-w-24:24:format=auto[out]",
                    "-map", "[out]"]
        else:
            cmd += ["-vf", vf]
        cmd += ["-t", f"{target:.3f}", "-c:v", "libx264", "-preset", "medium",
                "-crf", "20", scene_mp4]
        subprocess.run(cmd, check=True)

        scene_wav = os.path.join(base, f"{key}.wav.norm.wav")
        subprocess.run(
            ["ffmpeg", "-y", "-loglevel", "error", "-i", audio_path,
             "-af", f"adelay={int(PRE * 1000)}:all=1,apad",
             "-t", f"{target:.3f}", "-ar", "48000", "-ac", "2", scene_wav], check=True)

        parts_v.append(scene_mp4)
        parts_a.append(scene_wav)

        narration_txt = os.path.join(base, scene.get("narration", key + ".txt"))
        if os.path.exists(narration_txt):
            text = open(narration_txt).read().strip()
            sents = [x.strip() for x in text.replace("? ", "?|").replace(". ", ".|").split("|") if x.strip()]
            total = sum(len(x) for x in sents) or 1
            t = clock + PRE
            for x in sents:
                d = narration * len(x) / total
                srt.append((t, t + d, x))
                t += d

        flag = "evidence" if is_evidence else "tour"
        source = f"clip {os.path.basename(clip_path)}" if clip_path else f"{len(frames)} frames"
        print(f"{key}: {flag}, narration {narration:.1f}s, scene {target:.1f}s, "
              f"speed x{speed:.2f}, {source}")
        clock += target

    vlist = os.path.join(base, "vlist.txt")
    alist = os.path.join(base, "alist.txt")
    open(vlist, "w").write("".join(f"file '{p}'\n" for p in parts_v))
    open(alist, "w").write("".join(f"file '{p}'\n" for p in parts_a))
    subprocess.run(
        ["ffmpeg", "-y", "-loglevel", "error",
         "-f", "concat", "-safe", "0", "-i", vlist,
         "-f", "concat", "-safe", "0", "-i", alist,
         "-c:v", "copy", "-c:a", "aac", "-b:a", "160k", "-shortest",
         "-movflags", "+faststart", output], check=True)

    srt_path = os.path.splitext(output)[0] + ".srt"
    with open(srt_path, "w") as f:
        for n, (s, e, x) in enumerate(srt, 1):
            f.write(f"{n}\n{timestamp(s)} --> {timestamp(e)}\n{x}\n\n")

    print(f"total {clock:.1f}s -> {output}")


if __name__ == "__main__":
    main()
