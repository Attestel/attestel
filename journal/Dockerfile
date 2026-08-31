# Trade-journal microservice.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /journal .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /journal /app/journal
# TRADES_DIR is retained for local fallback and one-time JSON import; production trades use DB.
ENV PORT=8096 TRADES_DIR=/data/journal
EXPOSE 8096
ENTRYPOINT ["/app/journal"]
