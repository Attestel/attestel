# API gateway — standard-library-only Go, so the build needs no module download step.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
# The gateway //go:embed's seed/nvda.json (verified NVDA data compiled into the binary),
# so the seed directory MUST be in the build context before `go build`.
COPY seed ./seed
RUN CGO_ENABLED=0 go build -o /gateway .

FROM alpine:3.20
# ca-certificates: the gateway makes outbound HTTPS calls (SEC EDGAR, Alpha Vantage, Marketaux).
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /gateway /app/gateway
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
