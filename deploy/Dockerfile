# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fujipic ./cmd/fujipic
RUN mkdir -p /out/data/tmp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=nonroot:nonroot /out/fujipic /usr/local/bin/fujipic
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV DATABASE_PATH=/data/fujipic.db \
    TMP_DIR=/data/tmp \
    LISTEN_ADDR=:8080 \
    APP_ENV=production
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/fujipic"]
CMD ["serve"]
