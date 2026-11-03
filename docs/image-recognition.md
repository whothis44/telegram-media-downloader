# Image recognition for the archive browser (optional)

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

## Install (Windows, NVIDIA card)

1. Install the current NVIDIA driver (Game Ready or Studio).
2. Install **Docker Desktop** (docker.com) with the WSL 2 engine, which is the default. Start it once and let it
   finish setting up.
3. Open PowerShell and run (one line):

   ```
   docker run -d --name immich-ml --restart unless-stopped --gpus all -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache ghcr.io/immich-app/immich-machine-learning:v2.7.5-cuda
   ```

   - `127.0.0.1:3003` makes the service reachable from this PC only.
   - The model is kept in the `immich-ml-cache` volume, so it is downloaded once.
   - The version is pinned (v2.7.5) because the archive talks to an internal part of Immich that could change in
     another version. Use that version unless the archive's release notes name a newer one.
4. In the archive (**Archive → Settings → Image recognition**):
   - Enter `http://127.0.0.1:3003` and save.
   - Press **Check the connection**. The first check downloads the model and can take a few minutes.
   - It should then say **Connected**.

The archive then looks at each distinct picture once, in the background. Duplicates are not looked at again, and
videos are looked at as three frames each. It follows the duplicate-finding quiet hours, and pauses while the
service is unreachable. Progress is shown in Settings and in the browser's sidebar.

When idle for 5 minutes, the service frees the graphics card's memory, so games and video work are not affected
between batches. To stop it: `docker stop immich-ml`; to start it again: `docker start immich-ml`.

## Model

The default model is `ViT-B-16-SigLIP2__webli`: multilingual, fast, and good at photos.

- For somewhat better results, several times slower, use `ViT-L-16-SigLIP2-384__webli`.
- Changing the model makes the archive look at every picture again, because embeddings from different models
  cannot be compared. The old ones are replaced as the new ones are made.

## No NVIDIA card

Use the image without `-cuda` and drop `--gpus all`:

```
docker run -d --name immich-ml --restart unless-stopped -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache ghcr.io/immich-app/immich-machine-learning:v2.7.5
```

It works on the processor, but much more slowly.

## Running it on the NAS instead

If the NAS runs Docker, the same image can run there:

- Publish the port on the NAS's local address instead of 127.0.0.1.
- In the archive, enter that address (for example `http://192.168.1.20:3003`).

Most NAS processors are slow for this, so the PC is the better place.

Only addresses on this computer or the local network are accepted, and the address actually connected to is
checked too. A name that resolves to an internet address is refused, and so are VPN mesh addresses such as
Tailscale's 100.x.y.z. The archive never uses a proxy for this connection and never follows a redirect.

## What is stored

- **Where:** the archive's own database, table `ix_emb`. Each embedding is a list of 768 small numbers per distinct
  picture, under 1 KB each.
- **When it's wrong:** results are similarity guesses. A set's corrections ("belongs" / "not part of this set")
  always win over them.
- **Turning it off:** clear the address in Settings.
