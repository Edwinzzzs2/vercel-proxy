FROM --platform=$BUILDPLATFORM golang:1.25.5 AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.BuildVersion=$VERSION" -o /out/vercel-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/vercel-proxy /vercel-proxy

EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/vercel-proxy"]
CMD ["--addr", ":3000"]
