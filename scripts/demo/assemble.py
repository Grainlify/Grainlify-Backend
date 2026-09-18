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
un-flag a shot. An evidence scene is never time-shifted, labelled, animated or
composited on: a speed badge, a plate entry or a "designed" flag on one is
refused. If a wait is too long to watch, cut away from it to a designed card and
back; do not speed it up. "speed_label" remains for tour footage only, where its
number is computed from the speed actually applied.

Designed cards and tour plates
------------------------------
"designed": true marks an HTML/CSS card recorded by record.mjs. It plays at 1.0
and is lengthened only by holding its final frame, never retimed, so its easing
is never stretched. "entry": "plate" on a tour capture slides and fades the whole
unmodified frame in over the ground and out again. Captured and designed frames
never share a frame: scenes hard-cut into one another, and all motion lives
inside the designed scenes.
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


# The letterbox and the ground behind a tour plate. Flat, one colour, no
# texture or gradient: the edge of an evidence frame has to be unambiguous, so
# nothing designed may sit against it but a plain field. It is the landing
# page's warm dark ground, where the old value (0x0a0d14) was a blue-black left
# over from before the design system existed.
GROUND = "0x1a1512"

# Rigid-plate entry for tour captures: the whole unmodified frame slides and
# fades in over the ground, and out again. Never offered to evidence.
PLATE_SECONDS = 0.3
PLATE_SLIDE_PX = 48


def plate_in(scene_mp4, duration):
    """Slide and fade a tour capture in over the ground, and out again.

    The frame moves as one rigid block - nothing is drawn on it, and its pixels
    are not resampled, only positioned and faded as a whole. Tour captures only:
    main() refuses this on any evidence scene.
    """
    T, P, Y = duration, PLATE_SECONDS, PLATE_SLIDE_PX
    y = (f"if(lt(t,{P}),{Y}*pow(1-t/{P},2),"
         f"if(gt(t,{T - P:.3f}),{Y}*pow((t-{T - P:.3f})/{P},2),0))")
    tmp = scene_mp4 + ".plate.mp4"
    subprocess.run(
        ["ffmpeg", "-y", "-loglevel", "error",
         "-f", "lavfi", "-i", f"color=c={GROUND}:s=1920x1080:r=30:d={T:.3f}",
         "-i", scene_mp4, "-filter_complex",
         f"[1:v]format=yuva420p,fade=t=in:st=0:d={P}:alpha=1,"
         f"fade=t=out:st={T - P:.3f}:d={P}:alpha=1[p];"
         f"[0:v][p]overlay=x=0:y='{y}':shortest=1,format=yuv420p[out]",
         "-map", "[out]", "-c:v", "libx264", "-preset", "medium", "-crf", "20", tmp],
        check=True)
    os.replace(tmp, scene_mp4)


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
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    # A cut built before the narration exists. It runs the real pipeline rather
    # than an ad-hoc ffmpeg line, so the per-scene mp4s exist and verify-final.py
    # can check the 1.00 guarantee on a preview instead of only on the final cut.
    silent = "--silent" in sys.argv
    cfg_path = args[0] if args else "scenes.json"
    cfg = json.load(open(cfg_path))
    base = os.path.dirname(os.path.abspath(cfg_path))
    output = os.path.join(base, cfg.get("output", "demo.mp4"))
    if silent:
        stem, ext = os.path.splitext(output)
        # Named so a silent preview can never be handed over as the finished cut.
        output = f"{stem}.SILENT{ext}"

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
        is_designed = bool(scene.get("designed"))
        entry = scene.get("entry")

        # The boundary between designed frames and evidence, enforced here so it
        # does not depend on anyone remembering it. An evidence frame is shown
        # as captured, at 1.00, with nothing composited on it: no speed badge,
        # no plate animation, and it can never be a designed card.
        if is_evidence:
            for flag, on in (("speed_label", wants_label), ("entry", bool(entry)), ("designed", is_designed)):
                if on:
                    raise SystemExit(
                        f"{key}: evidence scene with {flag!r} set. Evidence plays as captured, "
                        f"at 1.00, with nothing on or over it - titles, labels and motion "
                        f"belong on the designed cards around it, never on the shot.")
        if is_designed and (wants_label or entry):
            raise SystemExit(f"{key}: a designed card animates itself; speed_label and entry do not apply.")
        if entry not in (None, "plate"):
            raise SystemExit(f"{key}: unknown entry {entry!r}; the only entry is \"plate\".")

        audio_path = None if silent else find_audio(base, key, scene.get("audio"))
        narration = 0.0 if silent else duration(audio_path)

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
                    f"or use a longer take. An evidence scene cannot carry a speed badge.")
        elif is_designed:
            # A designed card is an eased animation. Retiming it would stretch
            # the easing, so it plays at 1.0 and is lengthened only by holding
            # its final, settled frame. It is never shortened either: if the
            # narration is shorter, the card plays out and the audio pads.
            target = max(span, 0.0 if silent else PRE + narration + POST)
            speed = 1.0
        elif silent:
            # A tour shot is normally scaled to fill its narration. With no
            # narration there is nothing to fill, so it runs at 1.0 and the
            # preview is longer than the final cut will be.
            target = span
            speed = 1.0
        else:
            target = PRE + narration + POST
            speed = span / target

        label = f"{speed:.1f}x speed" if wants_label and abs(speed - 1.0) > 0.01 else None
        if is_evidence and label:
            print(f"  {key}: evidence shot time-shifted, burning {label!r} into the frame")
        elif not is_evidence and abs(speed - 1.0) > 0.01:
            print(f"  {key}: non-evidence shot scaled x{speed:.2f} to fit narration")

        vf = ("scale=1920:1080:force_original_aspect_ratio=decrease,"
              f"pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color={GROUND},fps=30,format=yuv420p")
        if is_designed and target > span + 0.001:
            vf += f",tpad=stop_mode=clone:stop_duration={target - span:.3f}"
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

        if entry == "plate":
            plate_in(scene_mp4, target)

        scene_wav = os.path.join(base, f"{key}.wav.norm.wav")
        if silent:
            subprocess.run(
                ["ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi",
                 "-i", "anullsrc=r=48000:cl=stereo",
                 "-t", f"{target:.3f}", "-ar", "48000", "-ac", "2", scene_wav], check=True)
        else:
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
            # With no narration the subtitles spread across the shot instead of
            # collapsing to zero length, so the preview stays readable.
            spread = max(target - PRE - POST, 0.0) if silent else narration
            for x in sents:
                d = spread * len(x) / total
                srt.append((t, t + d, x))
                t += d

        flag = "evidence" if is_evidence else "designed" if is_designed else "tour"
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
