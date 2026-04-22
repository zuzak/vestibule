FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/vestibule .

FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/vestibule /usr/local/bin/vestibule

USER 65532:65532

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/vestibule"]
