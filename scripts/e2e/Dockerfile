# Tiny sandbox image for the browser tests (preview-portal workflow).
# The real base image (image/Dockerfile) carries a full toolchain and
# takes minutes to build; the control plane only needs something that
# stays running — plus git, which the A3 agent checkpoint needs (plan
# §8: files_changed + revert run on a git ref; without git a run
# finishes with the "no checkpoint" warning and cannot be reverted).
# apt pulls git at BUILD time (network needed; the CI runner has it).
FROM debian:stable-slim
RUN groupadd -g 1000 sandbox \
 && useradd -u 1000 -g 1000 -m -d /home/sandbox -s /bin/bash sandbox \
 && mkdir -p /opt/sandbox-skel/workspace \
 && chown -R 1000:1000 /opt/sandbox-skel \
 && apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
USER sandbox
WORKDIR /home/sandbox
# PID 1 must exit on SIGTERM so `docker stop` is fast.
CMD ["bash", "-c", "trap 'exit 0' TERM INT; while :; do sleep 1 & wait $!; done"]
