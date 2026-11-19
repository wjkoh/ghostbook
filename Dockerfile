FROM golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ghostbook ./cmd/ghostbook

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/ghostbook /usr/local/bin/ghostbook
VOLUME ["/data"]
EXPOSE 8090

ENTRYPOINT ["/usr/local/bin/ghostbook"]
CMD ["--addr=:8090", "--data-dir=/data"]
