# Build the CLI binary. Mainly useful for `docker compose run cli ...`.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/cli ./cmd/cli

FROM alpine:3.20
COPY --from=build /out/cli /usr/local/bin/cli
ENTRYPOINT ["cli"]
