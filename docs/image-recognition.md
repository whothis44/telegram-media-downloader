# Image recognition (recommended) and speech to text (optional)

Image recognition is recommended. **Without it, these stop working:** copy checks by how alike the previews look
(and their calibration), search by what pictures show, "more like this", Things, look-alikes, faces and people,
text in pictures, and album suggestions by likeness. **These keep working without it:** downloads, the forward,
exact re-upload and likely video copy checks, sampling of big videos, the deadline order, backups and browsing.

The archive browser can use image recognition to:

- **Search by description.** Type what a picture shows ("two riders on a gravel road at sunset", in any of the
  many languages the model was trained on) and tick **by what they show**.
- **More like this.** A button in the picture viewer lists the pictures and videos that look most alike: the same
  scene or subject, not just copies.
- **Sort out sets.** A file that appears in only one post of a set is compared with the files the posts share.
  Files unlike all of them are set apart as **Probably not part of it**. Without image recognition they are listed
  as belonging, because nothing else can tell them apart. How alike counts as "unlike" is measured on your own
  sets, so it suits whichever model you use.
- **Find the album of a lone file.** A file posted on its own that looks very like the files of one album (and
  much less like any other) is offered in **Review → Lone files**: "Does this file belong with this album?"
- **Spot the same album in two chats.** Albums whose files look the same (re-encoded, cropped or watermarked) are
  offered in **Review → Same albums**; one answer settles every matching pair.

It is the machine-learning service of [Immich](https://immich.app), a self-hosted photo library, used on its own.
It runs in Docker on your PC and uses the graphics card. **No picture leaves your computer:**

- The archive only accepts an address on this computer or your local network.
- The service needs the internet once, to download its model (a few hundred MB, from Hugging Face). It does not
  send pictures anywhere.

## Install (Windows)

### 1. Docker Desktop

1. In an administrator PowerShell, run `winget install -e --id Docker.DockerDesktop`, or use the installer from
   docker.com. Keep **Use WSL 2** ticked and restart when asked.
2. Start Docker Desktop from the Start menu.
   - Accept the terms and skip sign-in; no account is needed.
   - Wait until it says **Engine running**.
   - For privacy, untick Settings → General → **Send usage statistics**.
3. Open a **new** PowerShell window and check that Docker can reach the graphics card:

   ```
   docker run --rm --gpus all nvidia/cuda:12.8.1-base-ubuntu22.04 nvidia-smi
   ```

   This should print a table naming your card.

**If Docker complains:**

- **"failed to connect to the docker API":** Docker Desktop isn't running, or hasn't finished starting.
- **"docker-credential-desktop: executable file not found":** happens until you next sign in to Windows. Run these
  once to fix it. They back up Docker's settings and switch off a sign-in helper that public images don't need:

  ```
  $cfg = "$env:USERPROFILE\.docker\config.json"; Copy-Item $cfg "$cfg.bak"
  (Get-Content $cfg -Raw) -replace '"credsStore"', '"credsStore_off"' | Set-Content $cfg
  ```

### 2. The image-recognition service

**RTX 50-series cards (5070 Ti and others):**

- Immich's own CUDA image is built on CUDA 12.2. On these cards it quietly falls back to the processor; its log
  says `Falling back to ['CPUExecutionProvider']`.
- Instead, build this archive's version: Immich's official image plus NVIDIA's CUDA 12.8 libraries (see
  `docs/immich-ml-rtx50/Dockerfile`). It has been checked on an RTX 5070 Ti.
- Run:

  ```
  docker build -t immich-ml-rtx50 https://github.com/whothis44/telegram-media-downloader.git#main:docs/immich-ml-rtx50
  docker run -d --name immich-ml --restart unless-stopped --gpus all -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache immich-ml-rtx50
  ```

**RTX 20, 30 or 40 series:**

```
docker run -d --name immich-ml --restart unless-stopped --gpus all -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache ghcr.io/immich-app/immich-machine-learning:v2.7.5-cuda
```

**No NVIDIA card:**

```
docker run -d --name immich-ml --restart unless-stopped -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache ghcr.io/immich-app/immich-machine-learning:v2.7.5
```

About these commands:

- `127.0.0.1:3003` makes the service reachable from this PC only.
- The models are kept in the `immich-ml-cache` volume, so they are downloaded once.
- The version is pinned (v2.7.5) because the archive talks to an internal part of Immich that could change in another
  version. Use that version unless the archive's release notes name a newer one.

### 3. Connect the archive

1. In **Archive → Settings → Image recognition**, enter `http://127.0.0.1:3003` and save.
2. Press **Check the connection**. The first check downloads the models and can take a few minutes. It should then
   say **Connected**.
3. Check that the graphics card is used. Run:

   ```
   docker logs immich-ml 2>&1 | Select-String "Failed|Error|fall"
   ```

   It should print nothing. While the archive works through pictures, Task Manager → Performance → GPU should show
   activity on its **Cuda** or **Compute** graph.

The archive then looks at each distinct picture once, in the background. Duplicates are not looked at again.

- **What it covers:** what the picture shows, then faces and text, when those are on.
- **When it runs:** it follows the duplicate-finding quiet hours, and pauses while the service is unreachable.
- **Progress:** shown in Settings and in the browser's sidebar.
- **How fast:** several files are sent at a time, so the card stays busy. How many is found by measuring: the
  archive tries one more at a time and keeps it only if more get done, and backs off at once when the service
  fails or times out (1 to 16 per kind of work). Without a usable graphics card, expect hours rather than minutes
  for tens of thousands of distinct pictures. Faces and text take longer than the rest.
- **Videos:** a frame about every 5 seconds (3 to 12 per video) is looked at. The distinct frames are kept, with
  where they are in the video, for finding a scene seen in one part of it. Videos looked at before this change are done again in the background, after everything
  else. This is up to 12 pictures per video for the graphics card instead of 3.

#### Keeping the graphics card busy

The diagnostics report (background work) shows, per kind of work, how many files are sent at once and where the
time of a file goes, in milliseconds: `read` (disk or NAS), `ffmpeg` (frames or sound out of videos), `jpeg`,
`upload`, `server` (the service's own work) and `answer`.

- If `read` or `ffmpeg` is larger than `server`, the card waits for the files, not the other way round. Settings of
  the service will not help.
- Immich's service runs requests on a pool of threads, `MACHINE_LEARNING_REQUEST_THREADS`. It defaults to the number
  of processor cores, and Immich's docs say to start with this setting when changing how much it does at once. The
  archive sends up to 16 files at once, so on a processor with fewer than 16 threads, raise it: add
  `-e MACHINE_LEARNING_REQUEST_THREADS=16` before the image name in the `docker run` command above.
- Leave `MACHINE_LEARNING_WORKERS` at 1 with one graphics card. Immich's docs advise against changing it unless
  memory is abundant (each worker loads its own copy of the models); it is meant for several cards.
- Leave `MACHINE_LEARNING_MODEL_INTER_OP_THREADS` and `..._INTRA_OP_THREADS` unset. In v2.7.5 they are only set
  for the processor; on the graphics card they are left to the runtime unless you set them, and the docs do not say
  that setting them helps there.
- Immich's hardware-acceleration page notes that sending more at once raises use, but also graphics memory use.

Sources: [environment variables](https://docs.immich.app/install/environment-variables),
[hardware-accelerated machine learning](https://docs.immich.app/features/ml-hardware-acceleration), and Immich
v2.7.5's `machine-learning/immich_ml/config.py`, `main.py` and `sessions/ort.py` (checked November 2026).

When idle for 5 minutes, the service frees the memory its models use. To stop it: `docker stop immich-ml`; to
start it again: `docker start immich-ml`.

### Starting the services without PowerShell

Both containers are made with `--restart unless-stopped`, so they start by themselves whenever Docker does. What
usually stops them is that Docker Desktop isn't running after the PC restarts. Three ways to deal with it:

- **The button.** In **Archive → Settings → Image recognition**, press **Start the recognition services**. The
  archive starts Docker Desktop if it isn't running (and waits up to 3 minutes for it), then starts each container
  that is stopped: `immich-ml` when image recognition has an address, `speaches` when speech to text has one. It
  then says, for each one, whether it was already running, was started, or isn't installed (then follow the steps
  above to install it). If you named your containers differently, enter the names under **Docker containers**.
- **Start them when the archive starts** (off by default). The archive does the same by itself a few seconds after
  it starts, and again at most every 30 minutes while a service doesn't answer.
- **Docker's own setting.** In Docker Desktop, tick Settings → General → **Start Docker Desktop when you sign in to
  your computer**. Docker then starts with Windows and brings the containers up with it, without the archive.

The archive only runs the `docker` program (and Docker Desktop) with fixed commands and the container names from
Settings. It never runs anything else.

## What else the same service does

Turn each of these on or off in **Archive → Settings → Image recognition**:

- **Text in pictures** (on by default). Screenshots, signs, documents and captions burned into videos are read,
  from three frames of each video. The search box then finds them. Choose the script under **Script**: Latin
  (French, Spanish…), Cyrillic, Greek, Korean, Thai, or the default (Chinese, English, Japanese). Arabic script
  isn't offered by Immich's text reader.
- **Look-alikes** (on by default). Pictures the model finds near-identical (crops, filters, stickers or captions
  added) but that the copy detector missed go to **Review look-alikes**. Nothing is merged by itself.
- **Things** (whenever image recognition is on). A list of subjects you can edit in Settings. The **Things** tab
  shows how many pictures stand out for each, and clicking one lists them, best first. It uses the embeddings
  already made, so it costs no extra work.
- **Faces** (off by default).
  - Faces are found in each picture and in three frames of each video, and grouped into people once three or more
    look alike, the way Immich does it.
  - The **People** tab lets you name a person, merge two people who are the same, hide one, take out a face that
    is someone else, and browse a person's posts.
  - In sets, a file showing a person from the set's shared files counts as belonging.

### Self-filmed, front-camera content

- **Faces work well on it:** faces are frontal and close.
- **Faces miss some:** motion blur, dark rooms and heavy compression lose some faces.
- **Faces can split one person:** face filters can split one person into several. Merge them in the People tab.
- **"What it shows" is less discriminating here:** to the model, one person talking to a phone looks much like
  another. That's why faces are used for sets when they're on.

## Speech to text (a second, optional container)

Voice notes, audio and videos can be written out so the search finds what is said, with timings shown next to
the file. It uses [speaches](https://github.com/speaches-ai/speaches), a Whisper server, in Docker on the
graphics card. Run (one line):

```
docker run -d --name speaches --restart unless-stopped --gpus all -p 127.0.0.1:8000:8000 -v speaches-cache:/home/ubuntu/.cache/huggingface/hub ghcr.io/speaches-ai/speaches:0.8.3-cuda
```

- **Graphics card:** this image is built on CUDA 12.9, new enough for RTX 50-series cards, so it should use the card.
- **If it doesn't:** if the check fails with a cuBLAS or "no kernel image" error, use the processor image
  `ghcr.io/speaches-ai/speaches:0.8.3-cpu` without `--gpus all`. It is much slower.

Then, in **Archive → Settings → Speech to text**:

1. Enter `http://127.0.0.1:8000` and save.
2. Press **Check the connection**. The first time, this downloads the Whisper model (`Systran/faster-whisper-large-v3`,
   about 3 GB).

How it behaves:

- **Languages:** they are recognised on their own.
- **Order:** files are written out one at a time, after everything else.
- **Long recordings:** only the first 90 minutes of each are written out.
- **No sound:** videos without a sound track are noted and skipped.

**Voice notes are off by default** in each chat's download filters. Switch on **voice** for the chats where they
matter, or they are neither downloaded nor written out.

## Model

The default model is `ViT-B-16-SigLIP2__webli`: multilingual, fast, and good at photos.

- For somewhat better results, several times slower, use `ViT-L-16-SigLIP2-384__webli`.
- Changing the model makes the archive look at every picture again, because embeddings from different models
  cannot be compared. The old ones are replaced as the new ones are made.

## Running it on the NAS instead

If the NAS runs Docker, the same image can run there:

- Publish the port on the NAS's local address instead of 127.0.0.1.
- In the archive, enter that address (for example `http://192.168.1.20:3003`).

Most NAS processors are slow for this. A NAS with an Intel processor that has built-in graphics can use Immich's
`-openvino` image instead, which is faster than its processor alone.

Only addresses on this computer or the local network are accepted, and the address actually connected to is
checked too. A name that resolves to an internet address is refused, and so are VPN mesh addresses such as
Tailscale's 100.x.y.z. The archive never uses a proxy for this connection and never follows a redirect.

## What is stored

- **Where:** the archive's own database, table `ix_emb`. Each embedding is a list of 768 small numbers per distinct
  picture, under 1 KB each.
- **When it's wrong:** results are similarity guesses. A set's corrections ("belongs" / "not part of this set")
  always win over them.
- **Turning it off:** clear the address in Settings.
