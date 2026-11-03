# Immich's machine-learning service (v2.7.5) for NVIDIA RTX 50-series cards.
#
# Immich's own CUDA image is built on CUDA 12.2. Its ONNX Runtime
# (onnxruntime-gpu 1.24, built with CUDA 12.8) needs CUDA 12.8 or later to run
# on Blackwell cards, so the image falls back to the processor there. This
# image is Immich's official one, unchanged, plus NVIDIA's CUDA 12.8 runtime
# libraries from NVIDIA's own package repository (already configured in the
# base image), put first on the library path.
#
# Build (once):
#   docker build -t immich-ml-rtx50 https://github.com/whothis44/telegram-media-downloader.git#main:docs/immich-ml-rtx50
# Run:
#   docker run -d --name immich-ml --restart unless-stopped --gpus all -p 127.0.0.1:3003:3003 -v immich-ml-cache:/cache immich-ml-rtx50
# Check: `docker logs immich-ml` should list CUDAExecutionProvider and not say
# "Falling back to ['CPUExecutionProvider']". If it does fall back, nothing
# breaks: it works on the processor, as the plain image does.

FROM ghcr.io/immich-app/immich-machine-learning:v2.7.5-cuda

USER root
RUN apt-get update && \
    apt-get install --no-install-recommends -yqq cuda-cudart-12-8 libcublas-12-8 libcufft-12-8 libcurand-12-8 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

ENV LD_LIBRARY_PATH=/usr/local/cuda-12.8/targets/x86_64-linux/lib:/usr/local/nvidia/lib:/usr/local/nvidia/lib64
