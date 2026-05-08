FROM golang:1.26.3 AS builder
WORKDIR /src

COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/ha-timescale-poller ./cmd/ha-timescale-poller

FROM gcr.io/distroless/base-debian12
COPY --from=builder /out/ha-timescale-poller /usr/local/bin/ha-timescale-poller

ENTRYPOINT ["/usr/local/bin/ha-timescale-poller"]
