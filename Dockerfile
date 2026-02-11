# Build stage
FROM golang:1.25.2-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -buildvcs=false -ldflags="-s -w" -o scheldue-bot .

# Final stage
FROM alpine:3.20

RUN apk add --no-cache tzdata ca-certificates

WORKDIR /app

COPY --from=builder /app/scheldue-bot /app/bot
COPY web /app/web

EXPOSE 8080

CMD ["/app/bot"]
