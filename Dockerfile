FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o operator ./cmd/operator && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o frontdoor ./cmd/frontdoor

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/operator /workspace/frontdoor /
USER 65532:65532
ENTRYPOINT ["/operator"]
