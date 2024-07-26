ARG GO_VERSION=1
FROM golang:${GO_VERSION}-bookworm as builder

RUN apt-get update && apt-get install -y \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /usr/src/app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN go build -v -o /main .


FROM debian:bookworm

COPY --from=builder /main /usr/local/bin/

EXPOSE 8080

CMD ["main"]
