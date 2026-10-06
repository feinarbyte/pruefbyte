# syntax=docker/dockerfile:1
ARG GO_VERSION=1.25

# Build stages run on the build machine's platform and cross-compile, so a
# multi-arch build only emulates the final stage's package install.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# BuildKit sets these for the target platform; defaults would override them.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pruefbyte ./cmd/pruefbyte

FROM --platform=$BUILDPLATFORM alpine:3 AS ocr
# The OCR version is pinned in internal/ocrbin/VERSION, which the release
# binaries embed too, so the image and `pruefbyte local` run the same ocr.
# tr also drops the CR a Windows checkout (core.autocrlf) adds.
COPY internal/ocrbin/VERSION /tmp/OCR_VERSION
# BuildKit sets TARGETARCH for the platform being built; a default here would
# override it (e.g. an amd64 binary in an arm64 image).
ARG TARGETARCH
RUN apk add --no-cache curl \
 && OCR_VERSION="$(tr -d '[:space:]' < /tmp/OCR_VERSION)" \
 && arch="${TARGETARCH:-amd64}" \
 && cd /tmp \
 && curl -fsSLO "https://github.com/alibaba/open-code-review/releases/download/${OCR_VERSION}/opencodereview-linux-${arch}" \
 && curl -fsSLO "https://github.com/alibaba/open-code-review/releases/download/${OCR_VERSION}/sha256sum.txt" \
 && grep " opencodereview-linux-${arch}\$" sha256sum.txt | sha256sum -c - \
 && install -m 0755 "opencodereview-linux-${arch}" /out-ocr \
 && curl -fsSL -o /out-ocr-LICENSE "https://raw.githubusercontent.com/alibaba/open-code-review/${OCR_VERSION}/LICENSE"

FROM alpine:3
# OCR needs git >= 2.41; alpine:3 ships a current git.
RUN apk add --no-cache git ca-certificates
COPY --from=ocr /out-ocr /usr/local/bin/ocr
COPY --from=build /out/pruefbyte /usr/local/bin/pruefbyte
# pruefbyte is MIT licensed; the bundled ocr binary is Apache-2.0.
COPY LICENSE /usr/share/licenses/pruefbyte/LICENSE
COPY --from=ocr /out-ocr-LICENSE /usr/share/licenses/open-code-review/LICENSE
LABEL org.opencontainers.image.licenses="MIT AND Apache-2.0"
ENTRYPOINT []
CMD ["pruefbyte", "review"]
