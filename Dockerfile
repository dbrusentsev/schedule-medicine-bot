# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o scheldue-bot .

# Final stage
FROM alpine:3.20

RUN apk add --no-cache tzdata ca-certificates

COPY --from=builder /app/scheldue-bot /bot

CMD ["/bot"]
