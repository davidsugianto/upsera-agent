FROM golang:1.26-alpine AS build
WORKDIR /src
# Copy module files first so the dependency layer is cached until they change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /agent-server ./cmd/agent-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agent-server /agent-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/agent-server"]
CMD ["--config", "/etc/k8s-triage-agent/config.yaml"]
