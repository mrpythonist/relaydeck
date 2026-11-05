# RelayDeck — multi-stage build, single static binary.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /relaydeck ./cmd/relaydeck

FROM scratch
COPY --from=build /relaydeck /relaydeck
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
VOLUME ["/data"]
ENV RELAYDECK_LISTEN_ADDR=:8080 RELAYDECK_DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/relaydeck", "serve"]
