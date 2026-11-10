# Tiny sandbox image for the lifecycle e2e test (scripts/e2e.sh). The real
# base image (image/Dockerfile) carries a full toolchain and takes minutes to
# build; the control plane only needs something that stays running.
#
# Contract the control plane relies on today (relaxed in M3a): bash, a
# `sandbox` user (uid 1000) and /opt/sandbox-skel for workspace seeding.
# No package installs, so it builds without network access.
FROM debian:stable-slim
RUN groupadd -g 1000 sandbox \
 && useradd -u 1000 -g 1000 -m -d /home/sandbox -s /bin/bash sandbox \
 && mkdir -p /opt/sandbox-skel/workspace \
 && chown -R 1000:1000 /opt/sandbox-skel
USER sandbox
WORKDIR /home/sandbox
# PID 1 must exit on SIGTERM so `docker stop` is fast.
CMD ["bash", "-c", "trap 'exit 0' TERM INT; while :; do sleep 1 & wait $!; done"]
