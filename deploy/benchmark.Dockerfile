FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal ./internal
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/baseline ./cmd/benchmark-baseline
FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/baseline /usr/local/bin/baseline
ENTRYPOINT ["baseline"]
