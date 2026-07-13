# Build the orchestrator binary from the repo-root Go module.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/orchestrator ./cmd/orchestrator

FROM alpine:3.20
COPY --from=build /out/orchestrator /usr/local/bin/orchestrator
EXPOSE 8080
ENTRYPOINT ["orchestrator"]
