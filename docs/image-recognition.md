# Image recognition and speech to text (optional)

The archive browser can use image recognition to:

- **Search by description.** Type what a picture shows ("two riders on a gravel road at sunset", in any of the
  many languages the model was trained on) and tick **by what they show**.
- **More like this.** A button in the picture viewer lists the pictures and videos that look most alike: the same
  scene or subject, not just copies.
- **Sort out sets.** A file that appears in only one post of a set is compared with the files the posts share.
  Files unlike all of them are set apart as **Probably not part of it**. Without image recognition they are listed
  as belonging, because nothing else can tell them apart.

It is the machine-learning service of [Immich](https://immich.app), a self-hosted photo library, used on its own.
It runs in Docker on your PC and uses the graphics card. **No picture leaves your computer:**

- The archive only accepts an address on this computer or your local network.
- The service needs the internet once, to download its model (a few hundred MB, from Hugging Face). It does not
  send pictures anywhere.

## Install (Windows)

**RTX 50-series cards (5070 Ti and others): use the processor image.**

- Immich's CUDA image (v2.7.5, and Immich's main branch as of November 2026) is built on CUDA 12.2.
- The RTX 50 series needs CUDA 12.8 or later, so on these cards that image quietly falls back to the processor
  anyway; its log says `Falling back to ['CPUExecutionProvider']`.
- The processor image does the same work. It's slower, but it runs in the background behind everything else.
- Check Immich's release notes for "CUDA 12.8" before switching to the `-cuda` image.

1. Install **Docker Desktop** (docker.com) with the WSL 2 engine, which is the default. Start it once and let it
   finish setting up.
2. Open PowerShell and run (one line):

   ```
   docker run -d --name immich-ml --restart unless-stopped -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache ghcr.io/immich-app/immich-machine-learning:v2.7.5
   ```

   - `127.0.0.1:3003` makes the service reachable from this PC only.
   - The models are kept in the `immich-ml-cache` volume, so they are downloaded once.
   - The version is pinned (v2.7.5) because the archive talks to an internal part of Immich that could change in
     another version. Use that version unless the archive's release notes name a newer one.
   - With an older NVIDIA card (RTX 20, 30 or 40 series), add `--gpus all` after `-d` and use the image
     `ghcr.io/immich-app/immich-machine-learning:v2.7.5-cuda`.
3. In the archive (**Archive → Settings → Image recognition**):
   - Enter `http://127.0.0.1:3003` and save.
   - Press **Check the connection**. The first check downloads the models and can take a few minutes.
   - It should then say **Connected**.

The archive then looks at each distinct picture once, in the background. Duplicates are not looked at again.

- **What it covers:** what the picture shows, then faces and text, when those are on.
- **When it runs:** it follows the duplicate-finding quiet hours, and pauses while the service is unreachable.
- **Progress:** shown in Settings and in the browser's sidebar.
- **How long:** on the processor, expect hours rather than minutes for tens of thousands of distinct pictures. Faces
  and text take longer than the rest.

When idle for 5 minutes, the service frees the memory its models use. To stop it: `docker stop immich-ml`; to
start it again: `docker start immich-ml`.

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
- **Long recordings:** only the first three hours of each are written out.
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
