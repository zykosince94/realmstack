FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /realmstack ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /realmstack-admin ./cmd/realmstack-admin

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /realmstack ./realmstack
# Operator tasks: migrate, maintenance, rebuild/verify derived tables, seed-load.
COPY --from=build /realmstack-admin ./realmstack-admin
COPY migrations ./migrations
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["./realmstack"]
