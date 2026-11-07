FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine AS build
WORKDIR /src
COPY src/api/go.mod src/api/go.sum ./
RUN go mod download
COPY src/api/cmd ./cmd
COPY src/api/internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /ting ./cmd/ting

FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/klaraalabs/ting" \
      org.opencontainers.image.description="Tiny, self-hosted push notifications"
RUN apk add --no-cache ca-certificates && addgroup -g 10001 ting && adduser -D -u 10001 -G ting ting
COPY --from=build /ting /usr/local/bin/ting
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["ting"]
CMD ["serve"]
