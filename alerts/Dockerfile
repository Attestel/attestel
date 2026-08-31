# Alerts microservice.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /alerts .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /alerts /app/alerts
# RULES_DIR is the local fallback and legacy-import path; production records use PostgreSQL.
ENV PORT=8095 RULES_DIR=/data/alerts
EXPOSE 8095
ENTRYPOINT ["/app/alerts"]
