# A Debian host with systemd as PID 1, for testing the installer.
FROM debian:trixie
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      systemd systemd-sysv curl ca-certificates iproute2 procps >/dev/null \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask getty.target systemd-logind.service console-getty.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
