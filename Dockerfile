FROM docker.io/library/golang:1.25.9-trixie AS builder

ARG TARGETARCH=amd64

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/porcupine_checker \
    ./main.go

FROM scratch

COPY --from=builder /out/porcupine_checker /porcupine_checker

ENTRYPOINT ["/porcupine_checker"]
