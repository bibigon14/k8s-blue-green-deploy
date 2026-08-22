# Stage 1: build
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG VERSION=dev

WORKDIR /src
COPY app/go.mod ./
RUN go mod download

COPY app/ ./
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /server .

# Stage 2: runtime
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /server /server

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/server"]
