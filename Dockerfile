# siteproxy-go — multi-stage build producing a tiny static image.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/siteproxy-go .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 1000 siteproxy
COPY --from=builder /out/siteproxy-go /usr/local/bin/siteproxy-go
COPY config.json /etc/siteproxy/config.json
USER siteproxy
EXPOSE 5006
ENTRYPOINT ["siteproxy-go"]
CMD ["-config", "/etc/siteproxy/config.json"]
