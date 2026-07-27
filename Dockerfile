FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/agent-server ./cmd/agent-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent-server /agent-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/agent-server"]
