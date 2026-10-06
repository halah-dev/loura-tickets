# Build stage. Dependencies are downloaded in their own layer so that editing
# source does not re-fetch the module cache.
FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off: the SQLite driver is pure Go (modernc.org/sqlite), which is why
# this produces a static binary and the final image needs no C library.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Run stage. Alpine rather than scratch only so that there is a shell to debug
# with; the binary itself has no runtime dependencies.
FROM alpine:3.20

RUN adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/server /app/server
COPY testdata /app/testdata

# The database lives in a directory the app user owns, mounted as a volume in
# compose so that tickets survive a container restart -- which is the whole
# point of the restart-safety behaviour.
RUN mkdir -p /app/data && chown -R app:app /app/data
USER app

EXPOSE 8080

ENTRYPOINT ["/app/server"]
CMD ["-addr=:8080", "-db=/app/data/tickets.db", "-seed=/app/testdata/tickets.json"]
