FROM --platform=$BUILDPLATFORM golang:1.27.1 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/ha-timescale-poller ./cmd/ha-timescale-poller
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/backfill ./cmd/backfill

FROM gcr.io/distroless/base-debian12
COPY --from=builder /out/ha-timescale-poller /usr/local/bin/ha-timescale-poller
COPY --from=builder /out/backfill /usr/local/bin/backfill

ENTRYPOINT ["/usr/local/bin/ha-timescale-poller"]
