FROM golang:1.26-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/consolebrowser/hosted ./internal/consolebrowser/hosted
COPY cmd/console-browser ./cmd/console-browser
RUN CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -buildvcs=false -trimpath -ldflags="-s -w" -o /console-browser ./cmd/console-browser

FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3
RUN apt-get update && apt-get install -y --no-install-recommends chromium xvfb x11vnc fonts-liberation iptables util-linux tini ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /console-browser /usr/local/bin/console-browser
COPY deploy/console-browser/entrypoint.sh /usr/local/bin/console-browser-init
RUN chmod 0755 /usr/local/bin/console-browser-init
ENV HOME=/run/console-browser
EXPOSE 8320
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["setpriv", "--reuid=65532", "--regid=65532", "--clear-groups", "--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs", "/usr/local/bin/console-browser", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/console-browser-init"]
CMD ["/usr/local/bin/console-browser"]
