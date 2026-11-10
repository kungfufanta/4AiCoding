# sandboxd-upgrader — runs ./upgrade.sh on behalf of the control plane so an
# in-place upgrade can outlive the control-plane container it replaces.
# docker:cli already ships the compose plugin and git; upgrade.sh needs bash.
FROM docker:27-cli
RUN apk add --no-cache bash curl coreutils \
 # The upgrader runs as root and mounts a checkout owned by whoever installed
 # sandboxd; without this git refuses with "dubious ownership". The container
 # is throwaway and only ever sees that one checkout, so trusting it is safe.
 && git config --system --add safe.directory '*'
ENTRYPOINT ["/bin/bash"]
