FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3
RUN apt-get update && apt-get install --yes --no-install-recommends \
  ca-certificates curl dumb-init && \
  rm -rf /var/lib/apt/lists/*

COPY slagworks-agent_*.deb slagworks_*.deb /tmp/
RUN dpkg --unpack /tmp/slagworks-agent_*.deb /tmp/slagworks_*.deb && \
    rm -rf /tmp/slagworks-agent_*.deb /tmp/slagworks_*.deb \
           /var/lib/dpkg/info/slagworks-agent.* /var/lib/dpkg/info/slagworks.*

EXPOSE 8081
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD curl -sf http://localhost:8081/readyz || exit 1
ENTRYPOINT ["/usr/bin/dumb-init", "--"]
CMD ["/usr/bin/slagworksd", "global"]
