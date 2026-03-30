FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /ollama-exporter ./cmd/exporter

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /ollama-exporter /ollama-exporter
USER nonroot:nonroot
EXPOSE 9400 9401
ENTRYPOINT ["/ollama-exporter"]
