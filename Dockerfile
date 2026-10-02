# Builds one of the services under cmd/, selected with --build-arg SERVICE=<name>.

FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG SERVICE
ARG VERSION=dev
RUN test -n "$SERVICE" && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/ipekutku/ai-sre-agent/internal/version.Version=${VERSION}" \
      -o /out/app ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
