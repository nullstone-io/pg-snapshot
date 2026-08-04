# The final image is based on the postgres image rather than a bare runtime because pgsnap shells
# out to pg_dump, pg_restore and vacuumdb. Pinning to the highest supported major (18) satisfies
# the rule that pg_dump must be at least the version of the server it reads.
ARG PG_MAJOR=18

FROM golang:1.26-alpine AS build
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/pgsnap ./cmd/pgsnap

FROM postgres:${PG_MAJOR}-alpine

RUN apk --no-cache add ca-certificates tzdata

USER postgres
WORKDIR /app

COPY --from=build /out/pgsnap /app/pgsnap

# Customers extend this image to supply their migration step, then point MIGRATE_COMMAND at it.
# There is no conventional hook path -- naming the command is one mechanism instead of two, and it
# stays visible in the app's configuration.
#
#   FROM nullstone/pg-snapshot:v1.0.0
#   COPY --from=migrations /app/bin/migrate /usr/local/bin/
#   COPY migrate.sh /app/migrate.sh

# Replaces the postgres image's own entrypoint, which exists to start a server -- we only want its
# client binaries. Commands are issued straight off the image:
#
#   docker run ... nullstone/pg-snapshot snapshot
#   docker run ... nullstone/pg-snapshot restore
ENTRYPOINT ["/app/pgsnap"]
CMD ["--help"]
